// Brokered system tools of the Agent Runtime MCP endpoint (v1.1): the
// sandboxed Agent reaches the Model execution layer and the Memory store
// through the same MCP surface it uses for tenant tools, with the fenced
// attempt identity and capability grants injected by the runtime worker.
// Nothing here trusts agent-supplied identity: the AttemptContext (and its
// capability grants) come from the IdentityResolver the worker controls.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/memory"
	kernelmodel "github.com/CloudEdgeCore/AgentOS/internal/kernel/model"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/model/provider"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/google/uuid"
)

// System tool names exposed alongside the tenant tool registry.
const (
	SystemModelInvoke   = "agentos.model.invoke"
	SystemMemoryPut     = "agentos.memory.put"
	SystemMemorySearch  = "agentos.memory.search"
	SystemTaskSpawn     = "agentos.task.spawn"
	systemToolsRevision = "v1.3"
)

// SpawnRequest is one dynamic-step spawn derived from the fenced identity:
// the workflow lineage (workflow id, version, parent step) comes from the
// worker-injected execution window, never from agent arguments.
type SpawnRequest struct {
	TenantID        string
	AttemptID       uuid.UUID
	FencingToken    int64
	WorkflowID      uuid.UUID
	ParentStepName  string
	Name            string
	Goal            string
	AgentVersionRef string
	Spec            json.RawMessage
	MaxAttempts     int
	IdempotencyKey  string
	Arguments       json.RawMessage
}

// SpawnOutcome is the authoritative result of one spawn call.
type SpawnOutcome struct {
	Code       string // "created" | "replayed" | guard denial code
	Message    string
	StepName   string
	SpawnDepth int
}

// WorkflowSpawner executes dynamic-step spawns through the kernel's guarded
// transaction. The runtime-adapter gRPC client satisfies it.
type WorkflowSpawner interface {
	Spawn(context.Context, SpawnRequest) (SpawnOutcome, error)
}

// ModelBroker executes fenced model invocations. The kernel model.Invoker
// and the runtime-adapter gRPC client both satisfy it.
type ModelBroker interface {
	InvokeStream(context.Context, kernelmodel.InvokeInput, func(string)) (kernelmodel.InvokeOutput, error)
}

// MemoryBroker reads and writes the tenant memory store with fenced
// provenance. The identity of the calling attempt is explicit: brokers never
// trust tenant or provenance fields from the agent-supplied arguments.
type MemoryBroker interface {
	Put(context.Context, AttemptContext, memory.PutInput) (store.MemoryRecord, bool, error)
	Search(context.Context, AttemptContext, memory.SearchInput) ([]store.MemoryRecord, error)
}

// KernelMemoryBroker adapts the kernel memory.Gateway to the broker surface,
// filling tenant and source provenance from the fenced identity.
type KernelMemoryBroker struct {
	Gateway *memory.Gateway
}

// Put writes with tenant and provenance derived from the fenced identity.
func (k KernelMemoryBroker) Put(ctx context.Context, identity AttemptContext, in memory.PutInput) (store.MemoryRecord, bool, error) {
	in.TenantID = identity.TenantID
	in.SourceTaskID = &identity.TaskID
	in.SourceRunID = &identity.RunID
	in.SourceAttemptID = &identity.AttemptID
	return k.Gateway.Put(ctx, in)
}

// Search reads with the tenant scope of the fenced identity.
func (k KernelMemoryBroker) Search(ctx context.Context, identity AttemptContext, in memory.SearchInput) ([]store.MemoryRecord, error) {
	in.TenantID = identity.TenantID
	return k.Gateway.Search(ctx, in)
}

// Broker extends a ToolAdapter with the system tools: tools/list merges the
// tenant registry with the brokered declarations, tools/call dispatches the
// system names and delegates everything else to the tenant adapter.
type Broker struct {
	tools    *ToolAdapter
	models   ModelBroker
	memory   MemoryBroker
	spawner  WorkflowSpawner
	identity IdentityResolver
}

// NewBroker builds the brokered MCP surface. models, memory and spawner may
// be nil: the corresponding tools are then not listed and their calls deny
// closed.
func NewBroker(tools *ToolAdapter, models ModelBroker, memories MemoryBroker, spawner WorkflowSpawner, identity IdentityResolver) *Broker {
	return &Broker{tools: tools, models: models, memory: memories, spawner: spawner, identity: identity}
}

type systemTool struct {
	name        string
	description string
	schema      string
}

func systemToolDeclarations(models ModelBroker, memories MemoryBroker, spawner WorkflowSpawner) []systemTool {
	var declarations []systemTool
	if models != nil {
		declarations = append(declarations, systemTool{
			name:        SystemModelInvoke,
			description: "Invoke a model through the AgentOS Model Gateway (policy, budget and audit enforced; the provider credential never reaches the agent)",
			schema:      `{"type":"object","properties":{"modelRef":{"type":"string","description":"provider/model reference declared by the AgentVersion"},"messages":{"type":"array","items":{"type":"object","properties":{"role":{"type":"string"},"content":{"type":"string"},"toolCallId":{"type":"string"},"toolCalls":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"name":{"type":"string"},"arguments":{"type":"string"}}}}},"required":["role","content"]}},"maxOutputTokens":{"type":"integer"},"temperature":{"type":"number"}},"required":["modelRef","messages"]}`,
		})
	}
	if memories != nil {
		declarations = append(declarations,
			systemTool{
				name:        SystemMemoryPut,
				description: "Write a memory record into the tenant store (namespaced, provenance-tracked, tombstone-deletable)",
				schema:      `{"type":"object","properties":{"namespace":{"type":"string"},"key":{"type":"string"},"contentType":{"type":"string"},"content":{"type":"string"},"sensitivity":{"type":"string"}},"required":["namespace","key","content"]}`,
			},
			systemTool{
				name:        SystemMemorySearch,
				description: "Hybrid keyword/vector memory search scoped to the tenant and namespace allowlist",
				schema:      `{"type":"object","properties":{"query":{"type":"string"},"namespace":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`,
			})
	}
	if spawner != nil {
		declarations = append(declarations, systemTool{
			name:        SystemTaskSpawn,
			description: "Spawn a new dynamic step into the running workflow (recursion, fan-out and budget guards enforced by the kernel; identical calls replay the same step)",
			schema:      `{"type":"object","properties":{"name":{"type":"string","description":"step name, lowercase token"},"goal":{"type":"string"},"agentVersionRef":{"type":"string"},"spec":{"type":"object"},"maxAttempts":{"type":"integer"}},"required":["name","goal"]}`,
		})
	}
	return declarations
}

// ListTools merges the tenant registry with the brokered system tools.
func (b *Broker) ListTools(ctx context.Context, params json.RawMessage) (any, *Error) {
	if err := rejectUnknownFields(params, new(any), true); err != nil {
		return nil, invalidParams("unknown cursor field")
	}
	listed, rpcErr := b.tools.ListTools(ctx, params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	result, ok := listed.(map[string]any)
	if !ok {
		return nil, &Error{Code: codeInternalError, Message: "list tools"}
	}
	tools, _ := result["tools"].([]map[string]any)
	if tools == nil {
		tools = []map[string]any{}
	}
	for _, declaration := range systemToolDeclarations(b.models, b.memory, b.spawner) {
		tools = append(tools, map[string]any{
			"name":        declaration.name,
			"description": declaration.description + " (system tool " + systemToolsRevision + ")",
			"inputSchema": json.RawMessage(declaration.schema),
		})
	}
	return map[string]any{"tools": tools}, nil
}

// CallTool dispatches the system tools and delegates tenant tools.
func (b *Broker) CallTool(ctx context.Context, params json.RawMessage) (any, *Error) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := rejectUnknownFields(params, &call, false); err != nil {
		return nil, invalidParams("name and arguments are required")
	}
	switch call.Name {
	case SystemModelInvoke:
		if b.models == nil {
			return nil, invalidParams("model execution is not configured on this runtime")
		}
		return b.callModel(ctx, call.Arguments)
	case SystemMemoryPut:
		if b.memory == nil {
			return nil, invalidParams("memory access is not configured on this runtime")
		}
		return b.putMemory(ctx, call.Arguments)
	case SystemMemorySearch:
		if b.memory == nil {
			return nil, invalidParams("memory access is not configured on this runtime")
		}
		return b.searchMemory(ctx, call.Arguments)
	case SystemTaskSpawn:
		if b.spawner == nil {
			return nil, invalidParams("dynamic spawn is not configured on this runtime")
		}
		return b.spawnTask(ctx, call.Arguments)
	default:
		return b.tools.CallTool(ctx, params)
	}
}

func (b *Broker) callModel(ctx context.Context, params json.RawMessage) (any, *Error) {
	var call struct {
		ModelRef        string          `json:"modelRef"`
		Messages        []brokerMessage `json:"messages"`
		MaxOutputTokens int32           `json:"maxOutputTokens"`
		Temperature     *float64        `json:"temperature"`
		Stream          bool            `json:"stream"`
	}
	if err := strictDecode(params, &call); err != nil {
		return nil, invalidParams(err.Error())
	}
	if strings.TrimSpace(call.ModelRef) == "" || len(call.Messages) == 0 {
		return nil, invalidParams("modelRef and messages are required")
	}
	if len(call.Messages) > 256 {
		return nil, invalidParams("too many messages")
	}
	identity, err := b.resolveIdentity(ctx)
	if err != nil {
		return toolErrorResult("no fenced attempt identity"), nil
	}
	if !granted(call.ModelRef, identity.AllowedModels) {
		return toolErrorResult("model is not in the AgentVersion capability grant: " + call.ModelRef), nil
	}
	messages := make([]provider.Message, 0, len(call.Messages))
	for _, message := range call.Messages {
		if strings.TrimSpace(message.Role) == "" || len(message.Content) > 1<<20 {
			return nil, invalidParams("each message needs a role and bounded content")
		}
		entry := provider.Message{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		for _, toolCall := range message.ToolCalls {
			entry.ToolCalls = append(entry.ToolCalls, provider.ToolCall{
				ID: toolCall.ID, Name: toolCall.Name, Arguments: toolCall.Arguments,
			})
		}
		messages = append(messages, entry)
	}
	taskID, runID, rpcErr := parseIdentityUUIDs(identity)
	if rpcErr != nil {
		return nil, rpcErr
	}
	output, invokeErr := b.models.InvokeStream(ctx, kernelmodel.InvokeInput{
		TenantID: identity.TenantID, TaskID: taskID, RunID: runID, AttemptID: identity.AttemptID,
		FencingToken: identity.FencingToken, AgentVersionRef: identity.AgentVersionRef,
		ModelRef: call.ModelRef, IdempotencyKey: systemIdempotencyKey(identity, SystemModelInvoke, params),
		Messages: messages, MaxOutputTokens: call.MaxOutputTokens, Temperature: call.Temperature,
		Stream: call.Stream,
	}, nil)
	if invokeErr != nil {
		document, _ := json.Marshal(map[string]any{
			"error":        invokeCode(invokeErr),
			"message":      boundedMessage(invokeErr),
			"callStatus":   string(output.Call.Status),
			"finishReason": output.Call.FinishReason,
		})
		return textResult(document, true), nil
	}
	toolCalls := make([]map[string]any, 0, len(output.ToolCalls))
	for _, toolCall := range output.ToolCalls {
		toolCalls = append(toolCalls, map[string]any{"id": toolCall.ID, "name": toolCall.Name, "arguments": toolCall.Arguments})
	}
	document, _ := json.Marshal(map[string]any{
		"callId": output.Call.ID.String(), "modelRef": output.Call.ModelRef, "status": string(output.Call.Status),
		"content": output.Content, "toolCalls": toolCalls, "finishReason": output.Call.FinishReason,
		"usage":   map[string]any{"inputTokens": output.Call.InputTokens, "outputTokens": output.Call.OutputTokens},
		"costUsd": output.Call.CostUSD, "providerRequestId": output.Call.ProviderRequestID,
	})
	return textResult(document, false), nil
}

type brokerMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCallID string           `json:"toolCallId"`
	ToolCalls  []brokerToolCall `json:"toolCalls"`
}

type brokerToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (b *Broker) putMemory(ctx context.Context, params json.RawMessage) (any, *Error) {
	var call struct {
		Namespace   string `json:"namespace"`
		Key         string `json:"key"`
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
		Sensitivity string `json:"sensitivity"`
	}
	if err := strictDecode(params, &call); err != nil {
		return nil, invalidParams(err.Error())
	}
	if strings.TrimSpace(call.Namespace) == "" || strings.TrimSpace(call.Key) == "" || call.Content == "" {
		return nil, invalidParams("namespace, key and content are required")
	}
	if len(call.Content) > 1<<20 {
		return nil, invalidParams("content exceeds 1 MiB")
	}
	identity, err := b.resolveIdentity(ctx)
	if err != nil {
		return toolErrorResult("no fenced attempt identity"), nil
	}
	if !granted(call.Namespace, identity.AllowedMemoryNamespaces) {
		return toolErrorResult("namespace is not in the AgentVersion capability grant: " + call.Namespace), nil
	}
	if call.ContentType == "" {
		call.ContentType = "text/plain"
	}
	if call.Sensitivity == "" {
		call.Sensitivity = "internal"
	}
	record, replayed, putErr := b.memory.Put(ctx, identity, memory.PutInput{
		Namespace: call.Namespace, Key: call.Key,
		ContentType: call.ContentType, Content: call.Content, Sensitivity: call.Sensitivity,
		Provenance: map[string]any{
			"agentVersionRef": identity.AgentVersionRef, "via": "mcp:" + SystemMemoryPut,
			"fencingToken": identity.FencingToken,
		},
	})
	if putErr != nil {
		return toolErrorResult("memory write failed: " + boundedMessage(putErr)), nil
	}
	document, _ := json.Marshal(map[string]any{
		"id": record.ID.String(), "namespace": record.Namespace, "key": record.Key,
		"resourceVersion": record.ResourceVersion, "replayed": replayed,
	})
	return textResult(document, false), nil
}

func (b *Broker) searchMemory(ctx context.Context, params json.RawMessage) (any, *Error) {
	var call struct {
		Query     string `json:"query"`
		Namespace string `json:"namespace"`
		Limit     int32  `json:"limit"`
	}
	if err := strictDecode(params, &call); err != nil {
		return nil, invalidParams(err.Error())
	}
	if strings.TrimSpace(call.Query) == "" {
		return nil, invalidParams("query is required")
	}
	if call.Limit < 0 || call.Limit > 100 {
		call.Limit = 20
	}
	identity, err := b.resolveIdentity(ctx)
	if err != nil {
		return toolErrorResult("no fenced attempt identity"), nil
	}
	if call.Namespace != "" && !granted(call.Namespace, identity.AllowedMemoryNamespaces) {
		return toolErrorResult("namespace is not in the AgentVersion capability grant: " + call.Namespace), nil
	}
	records, searchErr := b.memory.Search(ctx, identity, memory.SearchInput{
		Query: call.Query, Namespace: call.Namespace, Limit: int(call.Limit),
	})
	if searchErr != nil {
		return toolErrorResult("memory search failed: " + boundedMessage(searchErr)), nil
	}
	results := make([]map[string]any, 0, len(records))
	for _, record := range records {
		results = append(results, map[string]any{
			"id": record.ID.String(), "namespace": record.Namespace, "key": record.Key,
			"contentType": record.ContentType, "content": record.Content,
			"resourceVersion": record.ResourceVersion, "createdAt": record.CreatedAt.Format(time.RFC3339),
		})
	}
	document, _ := json.Marshal(map[string]any{"records": results})
	return textResult(document, false), nil
}

// spawnTask extends the calling attempt's workflow with one dynamic step.
// The workflow lineage comes from the fenced identity (worker-injected);
// the agent only names the step, its goal and its task spec. Standalone
// tasks (no workflow) deny closed.
func (b *Broker) spawnTask(ctx context.Context, params json.RawMessage) (any, *Error) {
	var call struct {
		Name            string          `json:"name"`
		Goal            string          `json:"goal"`
		AgentVersionRef string          `json:"agentVersionRef"`
		Spec            json.RawMessage `json:"spec,omitempty"`
		MaxAttempts     int             `json:"maxAttempts"`
	}
	if err := strictDecode(params, &call); err != nil {
		return nil, invalidParams(err.Error())
	}
	if strings.TrimSpace(call.Name) == "" || strings.TrimSpace(call.Goal) == "" {
		return nil, invalidParams("name and goal are required")
	}
	if len(call.Goal) > 8192 {
		return nil, invalidParams("goal exceeds 8192 bytes")
	}
	if call.MaxAttempts < 0 || call.MaxAttempts > 10 {
		return nil, invalidParams("maxAttempts must be 0..10")
	}
	identity, err := b.resolveIdentity(ctx)
	if err != nil {
		return toolErrorResult("no fenced attempt identity"), nil
	}
	if identity.WorkflowID == uuid.Nil {
		return toolErrorResult("task is not part of a workflow; dynamic spawn is unavailable"), nil
	}
	if !identity.CanSpawnTasks {
		return toolErrorResult("dynamic spawn is not granted by the AgentVersion"), nil
	}
	agentVersionRef := call.AgentVersionRef
	if agentVersionRef == "" {
		agentVersionRef = identity.AgentVersionRef
	}
	if !granted(agentVersionRef, identity.AllowedChildAgents) {
		return toolErrorResult("child agent is not in the AgentVersion spawn allowlist: " + agentVersionRef), nil
	}
	outcome, spawnErr := b.spawner.Spawn(ctx, SpawnRequest{
		TenantID: identity.TenantID, AttemptID: identity.AttemptID, FencingToken: identity.FencingToken,
		WorkflowID:     identity.WorkflowID,
		ParentStepName: identity.ParentStepName, Name: call.Name, Goal: call.Goal,
		AgentVersionRef: agentVersionRef, Spec: call.Spec, MaxAttempts: call.MaxAttempts,
		IdempotencyKey: systemIdempotencyKey(identity, SystemTaskSpawn, params),
		Arguments:      params,
	})
	if spawnErr != nil {
		return toolErrorResult("spawn failed: " + boundedMessage(spawnErr)), nil
	}
	document, _ := json.Marshal(map[string]any{
		"outcome": outcome.Code, "message": outcome.Message, "step": outcome.StepName,
		"spawnDepth": outcome.SpawnDepth,
	})
	isError := outcome.Code != "created" && outcome.Code != "replayed"
	return textResult(document, isError), nil
}

// resolveIdentity prefers the execution-scoped window when the agent
// reported its execution id, falling back to the resolver's default (the
// single-assignment window). Both paths deny closed.
func (b *Broker) resolveIdentity(ctx context.Context) (AttemptContext, error) {
	if execution := ExecutionID(ctx); execution != "" {
		if scoped, ok := b.identity.(ExecutionIdentityResolver); ok {
			return scoped.ResolveExecution(ctx, execution)
		}
	}
	return b.identity.Resolve(ctx)
}

// granted is the default-deny capability check: an empty grant denies.
func granted(requested string, allowed []string) bool {
	for _, candidate := range allowed {
		if candidate == requested {
			return true
		}
	}
	return false
}

func parseIdentityUUIDs(identity AttemptContext) (uuid.UUID, uuid.UUID, *Error) {
	if identity.TaskID == uuid.Nil || identity.RunID == uuid.Nil {
		return uuid.Nil, uuid.Nil, &Error{Code: codeInternalError, Message: "attempt context is missing task or run id"}
	}
	return identity.TaskID, identity.RunID, nil
}

// systemIdempotencyKey derives a deterministic replay key from the attempt
// identity and the canonical arguments.
func systemIdempotencyKey(identity AttemptContext, tool string, args json.RawMessage) string {
	var document any
	if err := json.Unmarshal(args, &document); err != nil {
		document = args
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		canonical = args
	}
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("mcp/%s/%s/%s", identity.AttemptID, tool, hex.EncodeToString(digest[:8]))
}

// strictDecode decodes with unknown-field rejection and a trailing-value
// check for the system tool arguments.
func strictDecode(params json.RawMessage, target any) error {
	if len(params) == 0 {
		return fmt.Errorf("arguments are required")
	}
	return rejectUnknownFields(params, target, false)
}

// invokeCode maps invocation errors onto stable agent-facing codes.
func invokeCode(err error) string {
	switch {
	case errors.Is(err, kernelmodel.ErrBudgetExhausted):
		return "BUDGET_EXHAUSTED"
	case errors.Is(err, kernelmodel.ErrModelDenied):
		return "MODEL_DENIED"
	case errors.Is(err, kernelmodel.ErrNoProviderExecution):
		return "NO_PROVIDER_EXECUTION"
	case errors.Is(err, provider.ErrProviderUnavailable):
		return "PROVIDER_UNAVAILABLE"
	case errors.Is(err, provider.ErrProviderRejected):
		return "PROVIDER_REJECTED"
	default:
		return "INVOCATION_FAILED"
	}
}

func boundedMessage(err error) string {
	message := err.Error()
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}
