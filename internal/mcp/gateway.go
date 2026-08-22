package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/tool"
	"github.com/google/uuid"
)

// AttemptContext is the fenced execution identity an MCP call is bound to.
// The adapter never trusts MCP-supplied identity: it comes from the resolver,
// which in production is injected by the runtime that owns the Attempt.
type AttemptContext struct {
	TenantID        string
	TaskID          uuid.UUID
	RunID           uuid.UUID
	AttemptID       uuid.UUID
	FencingToken    int64
	AgentVersionRef string
	// AllowedModels is the AgentVersion's declared model capability grant;
	// the brokered model tool denies everything outside it (default deny).
	AllowedModels []string
	// AllowedMemoryNamespaces is the declared memory capability grant; the
	// brokered memory tools deny writes and reads outside it.
	AllowedMemoryNamespaces []string
}

// IdentityResolver supplies the fenced identity for inbound MCP calls.
type IdentityResolver interface {
	Resolve(context.Context) (AttemptContext, error)
}

// ExecutionIdentityResolver resolves the identity of one open execution
// window from the execution id the agent reports; shared Agent endpoints
// use it to keep concurrent attempts correctly fenced.
type ExecutionIdentityResolver interface {
	ResolveExecution(context.Context, string) (AttemptContext, error)
}

// StaticIdentity is a fixed identity for deployments where the attempt
// context is known at construction (tests and explicitly wired dev setups).
type StaticIdentity struct {
	Context AttemptContext
}

func (s StaticIdentity) Resolve(context.Context) (AttemptContext, error) { return s.Context, nil }

// ToolInvoker is the fenced decision chain MCP calls flow through. The
// tool.Gateway satisfies it.
type ToolInvoker interface {
	InvokeTool(context.Context, tool.InvokeInput) (tool.InvokeResult, error)
	ListTools(context.Context, string) ([]store.ToolDescriptor, error)
}

type versionedToolLister interface {
	ListToolsForAgent(context.Context, string, string) ([]store.ToolDescriptor, error)
}

// ToolAdapter exposes a Tool Gateway as an MCP tools server. Mapping rules
// (documented, v0.1):
//   - MCP tool name is the descriptor name; version and risk ride in the
//     description, and calls resolve the latest registered version.
//   - action defaults to the descriptor's first declared action ("invoke"
//     when none), resource to the first resource pattern ("tool:<name>"
//     when none).
//   - the idempotency key is derived deterministically from the attempt and
//     the canonical arguments, so identical calls replay instead of repeating
//     an external side effect (at-least-once + idempotent consumer).
type ToolAdapter struct {
	invoker  ToolInvoker
	identity IdentityResolver
}

func NewToolAdapter(invoker ToolInvoker, identity IdentityResolver) *ToolAdapter {
	return &ToolAdapter{invoker: invoker, identity: identity}
}

// ListTools implements tools/list.
func (a *ToolAdapter) ListTools(ctx context.Context, params json.RawMessage) (any, *Error) {
	if err := rejectUnknownFields(params, new(any), true); err != nil {
		return nil, invalidParams("unknown cursor field")
	}
	identity, err := a.identity.Resolve(ctx)
	if err != nil {
		return nil, unauthorizedError(err)
	}
	descriptors, err := a.listTools(ctx, identity)
	if err != nil {
		return nil, &Error{Code: codeInternalError, Message: "list tools"}
	}
	tools := make([]map[string]any, 0, len(descriptors))
	for _, descriptor := range descriptors {
		tools = append(tools, map[string]any{
			"name":        descriptor.Name,
			"description": fmt.Sprintf("%s (version %s, sideEffectRisk %s)", descriptor.Name, descriptor.Version, descriptor.SideEffectRisk),
			"inputSchema": json.RawMessage(descriptor.ParamsSchema),
		})
	}
	return map[string]any{"tools": tools}, nil
}

// unauthorizedError reports a call outside any fenced execution window.
func unauthorizedError(err error) *Error {
	return &Error{Code: codeUnauthorized, Message: "no fenced attempt identity", Data: err.Error()}
}

// CallTool implements tools/call.
func (a *ToolAdapter) CallTool(ctx context.Context, params json.RawMessage) (any, *Error) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := rejectUnknownFields(params, &call, false); err != nil {
		return nil, invalidParams("name and arguments are required")
	}
	if strings.TrimSpace(call.Name) == "" {
		return nil, invalidParams("name is required")
	}
	if len(call.Arguments) > 0 && !json.Valid(call.Arguments) {
		return nil, invalidParams("arguments must be a JSON object")
	}
	identity, err := a.identity.Resolve(ctx)
	if err != nil {
		// Default deny: without a fenced attempt identity the call is
		// refused as a tool outcome, not a protocol failure.
		return toolErrorResult("no fenced attempt identity"), nil
	}
	descriptors, err := a.listTools(ctx, identity)
	if err != nil {
		return nil, &Error{Code: codeInternalError, Message: "resolve tool descriptor"}
	}
	descriptor, ok := latestDescriptor(descriptors, call.Name)
	if !ok {
		return nil, invalidParams("unknown tool: " + call.Name)
	}
	action := firstOr(descriptor.Actions, "invoke")
	resource := firstOr(descriptor.ResourcePatterns, "tool:"+descriptor.Name)

	args := call.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	result, err := a.invoker.InvokeTool(ctx, tool.InvokeInput{
		TenantID: identity.TenantID, TaskID: identity.TaskID, RunID: identity.RunID,
		AttemptID: identity.AttemptID, FencingToken: identity.FencingToken,
		AgentVersionRef: identity.AgentVersionRef,
		ToolName:        descriptor.Name, Action: action, Resource: resource,
		Args: args, IdempotencyKey: mcpIdempotencyKey(identity, descriptor.Name, args),
	})
	if err != nil {
		return toolErrorResult("invocation failed: " + err.Error()), nil
	}
	switch result.Outcome {
	case tool.OutcomeExecuted, tool.OutcomeReplayed:
		return textResult(result.Result, false), nil
	case tool.OutcomeDenied:
		denied, _ := json.Marshal(map[string]any{"denied": true, "denyReasons": result.DenyReasons})
		return textResult(denied, true), nil
	case tool.OutcomeRequiresApproval:
		text, _ := json.Marshal(map[string]any{
			"requiresApproval": true,
			"approvalId":       result.ApprovalID.String(),
		})
		return textResult(text, true), nil
	default:
		return toolErrorResult("unexpected outcome: " + string(result.Outcome)), nil
	}
}

func (a *ToolAdapter) listTools(ctx context.Context, identity AttemptContext) ([]store.ToolDescriptor, error) {
	if versioned, ok := a.invoker.(versionedToolLister); ok {
		return versioned.ListToolsForAgent(ctx, identity.TenantID, identity.AgentVersionRef)
	}
	return a.invoker.ListTools(ctx, identity.TenantID)
}

// textResult wraps an output document as an MCP text content item.
func textResult(text []byte, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(text)}},
		"isError": isError,
	}
}

func toolErrorResult(message string) map[string]any {
	text, _ := json.Marshal(map[string]any{"error": message})
	return textResult(text, true)
}

// mcpIdempotencyKey derives a deterministic key from the attempt identity and
// the canonical arguments, so identical MCP calls replay instead of repeating
// external side effects.
func mcpIdempotencyKey(identity AttemptContext, toolName string, args json.RawMessage) string {
	var document any
	if err := json.Unmarshal(args, &document); err != nil {
		document = args
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		canonical = args
	}
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("mcp/%s/%s/%s", identity.AttemptID, toolName, hex.EncodeToString(digest[:8]))
}

func latestDescriptor(descriptors []store.ToolDescriptor, name string) (store.ToolDescriptor, bool) {
	var latest store.ToolDescriptor
	found := false
	for _, descriptor := range descriptors {
		if descriptor.Name == name && (!found || descriptor.Version > latest.Version) {
			latest, found = descriptor, true
		}
	}
	return latest, found
}

func firstOr(values []string, fallback string) string {
	if len(values) == 0 {
		return fallback
	}
	return values[0]
}

// rejectUnknownFields strictly decodes a JSON-RPC params document, rejecting
// unknown fields, duplicate keys and trailing values.
func rejectUnknownFields(params json.RawMessage, target any, allowEmpty bool) error {
	if len(params) == 0 {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("params are required")
	}
	decoder := json.NewDecoder(bytes.NewReader(params))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureEOF(decoder)
}
