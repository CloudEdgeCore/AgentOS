// Brokered system tools of the Agent Runtime MCP endpoint: the
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
	"reflect"
	"strings"
	"time"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/capability"
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
	systemToolsRevision = "v1.4"
)

// Bounds of the tool-contract surface an agent may attach to one model call:
// the count and serialized size of the platform-generated schemas stay
// bounded so a sandbox cannot inflate the provider request through the
// broker.
const (
	maxModelTools       = 64
	maxModelToolsBytes  = 256 << 10
	toolNameMaxLenBytes = 256
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
	schema      json.RawMessage
}

type modelToolInput struct {
	ModelRef        string          `json:"modelRef" required:"true"`
	Messages        []brokerMessage `json:"messages" required:"true"`
	MaxOutputTokens int32           `json:"maxOutputTokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	// Tools names the tenant tools the model may call, as bare names
	// ("weather") or pinned references ("weather@1.2.0"). Only names cross
	// this boundary: the platform resolves each to the capability-filtered
	// descriptor and generates the schema, so a sandbox cannot invent or
	// widen a tool contract or attach a tool outside the AgentVersion grant.
	Tools []string `json:"tools,omitempty"`
}

type memoryPutToolInput struct {
	Namespace   string `json:"namespace" required:"true"`
	Key         string `json:"key" required:"true"`
	ContentType string `json:"contentType,omitempty"`
	Content     string `json:"content" required:"true"`
	Sensitivity string `json:"sensitivity,omitempty" enum:"internal,confidential,restricted"`
}

type memorySearchToolInput struct {
	Query       string `json:"query" required:"true"`
	Namespace   string `json:"namespace,omitempty"`
	Sensitivity string `json:"sensitivity,omitempty" enum:"internal,confidential,restricted"`
	Limit       int32  `json:"limit,omitempty"`
}

type spawnToolInput struct {
	Name            string          `json:"name" required:"true"`
	Goal            string          `json:"goal" required:"true"`
	AgentVersionRef string          `json:"agentVersionRef,omitempty"`
	Spec            json.RawMessage `json:"spec,omitempty"`
	MaxAttempts     int             `json:"maxAttempts,omitempty"`
}

func systemToolDeclarations(models ModelBroker, memories MemoryBroker, spawner WorkflowSpawner) []systemTool {
	var declarations []systemTool
	if models != nil {
		declarations = append(declarations, systemTool{
			name:        SystemModelInvoke,
			description: "Invoke a model through the AgentOS Model Gateway (policy, budget and audit enforced; the provider credential never reaches the agent)",
			schema:      schemaFor[modelToolInput](),
		})
	}
	if memories != nil {
		declarations = append(declarations,
			systemTool{
				name:        SystemMemoryPut,
				description: "Write a memory record into the tenant store (namespaced, provenance-tracked, tombstone-deletable)",
				schema:      schemaFor[memoryPutToolInput](),
			},
			systemTool{
				name:        SystemMemorySearch,
				description: "Hybrid keyword/vector memory search scoped to the tenant and namespace allowlist",
				schema:      schemaFor[memorySearchToolInput](),
			})
	}
	if spawner != nil {
		declarations = append(declarations, systemTool{
			name:        SystemTaskSpawn,
			description: "Spawn a new dynamic step into the running workflow (recursion, fan-out and budget guards enforced by the kernel; identical calls replay the same step)",
			schema:      schemaFor[spawnToolInput](),
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
			"inputSchema": declaration.schema,
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
	var call modelToolInput
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
	tools, denial, rpcErr := b.resolveModelTools(ctx, identity, call.Tools)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if denial != "" {
		return toolErrorResult(denial), nil
	}
	output, invokeErr := b.models.InvokeStream(ctx, kernelmodel.InvokeInput{
		TenantID: identity.TenantID, TaskID: taskID, RunID: runID, AttemptID: identity.AttemptID,
		FencingToken: identity.FencingToken, AgentVersionRef: identity.AgentVersionRef,
		ModelRef: call.ModelRef, IdempotencyKey: systemIdempotencyKey(identity, SystemModelInvoke, params),
		Messages: messages, MaxOutputTokens: call.MaxOutputTokens, Temperature: call.Temperature,
		Stream: call.Stream, Tools: tools,
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
		"costUsd": output.Call.CostMicroUSD.USD(), "costMicroUsd": output.Call.CostMicroUSD,
		"providerRequestId": output.Call.ProviderRequestID,
	})
	return textResult(document, false), nil
}

// resolveModelTools turns agent-supplied tool names into platform-generated
// tool definitions. Every name must resolve to a descriptor the AgentVersion
// is granted (the same capability-filtered registry tools/list exposes):
// a bare name resolves to the latest granted version, a "name@version"
// reference pins one exact version. The definition the model sees carries
// the pinned name when a version was requested, so the model's tool_calls
// refer back to the exact version the agent chose. Unknown or ungranted
// names deny closed as a tool outcome (denial), protocol violations as a
// params error (rpcErr).
func (b *Broker) resolveModelTools(ctx context.Context, identity AttemptContext, requested []string) (tools []provider.ToolDefinition, denial string, rpcErr *Error) {
	if len(requested) == 0 {
		return nil, "", nil
	}
	if len(requested) > maxModelTools {
		return nil, "", invalidParams(fmt.Sprintf("too many tools: at most %d per model call", maxModelTools))
	}
	descriptors, err := b.tools.listTools(ctx, identity)
	if err != nil {
		return nil, "", &Error{Code: codeInternalError, Message: "resolve tool registry"}
	}
	tools = make([]provider.ToolDefinition, 0, len(requested))
	used := 0
	for _, name := range requested {
		if len(name) > toolNameMaxLenBytes {
			return nil, "", invalidParams("tool name exceeds the length bound")
		}
		advertised, descriptor, ok := resolveToolReference(descriptors, name)
		if !ok {
			return nil, "tool is not granted to this AgentVersion or the pinned version is not registered: " + name, nil
		}
		definition := provider.ToolDefinition{
			Name:        advertised,
			Description: fmt.Sprintf("%s (version %s, sideEffectRisk %s)", descriptor.Name, descriptor.Version, descriptor.SideEffectRisk),
			Parameters:  json.RawMessage(descriptor.ParamsSchema),
		}
		used += len(definition.Name) + len(definition.Description) + len(definition.Parameters)
		if used > maxModelToolsBytes {
			return nil, "", invalidParams("tool contracts exceed the size bound")
		}
		tools = append(tools, definition)
	}
	return tools, "", nil
}

// resolveToolReference resolves one agent-requested tool reference against
// the granted descriptors. It returns the name advertised to the model (the
// pinned "name@version" when a version was requested), the descriptor, and
// whether the reference resolved.
func resolveToolReference(descriptors []store.ToolDescriptor, reference string) (string, store.ToolDescriptor, bool) {
	name, version, pinned := strings.Cut(reference, "@")
	if strings.TrimSpace(name) == "" || (pinned && strings.TrimSpace(version) == "") {
		return "", store.ToolDescriptor{}, false
	}
	if !pinned {
		descriptor, ok := latestDescriptor(descriptors, name)
		if !ok {
			return "", store.ToolDescriptor{}, false
		}
		return descriptor.Name, descriptor, true
	}
	for _, descriptor := range descriptors {
		if descriptor.Name == name && descriptor.Version == version {
			return reference, descriptor, true
		}
	}
	return "", store.ToolDescriptor{}, false
}

type brokerMessage struct {
	Role       string           `json:"role" required:"true"`
	Content    string           `json:"content" required:"true"`
	ToolCallID string           `json:"toolCallId,omitempty"`
	ToolCalls  []brokerToolCall `json:"toolCalls,omitempty"`
}

type brokerToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (b *Broker) putMemory(ctx context.Context, params json.RawMessage) (any, *Error) {
	var call memoryPutToolInput
	if err := strictDecode(params, &call); err != nil {
		return nil, invalidParams(err.Error())
	}
	if strings.TrimSpace(call.Namespace) == "" || strings.TrimSpace(call.Key) == "" || call.Content == "" {
		return nil, invalidParams("namespace, key and content are required")
	}
	if len(call.Content) > store.MemoryContentLimit {
		return nil, invalidParams("content exceeds 256 KiB")
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
	if !granted(call.Sensitivity, effectiveMemorySensitivities(identity.AllowedMemorySensitivities)) {
		return toolErrorResult("memory sensitivity is not granted: " + call.Sensitivity), nil
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
	var call memorySearchToolInput
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
	if call.Sensitivity == "" {
		call.Sensitivity = "internal"
	}
	if !granted(call.Sensitivity, effectiveMemorySensitivities(identity.AllowedMemorySensitivities)) {
		return toolErrorResult("memory sensitivity is not granted: " + call.Sensitivity), nil
	}
	records, searchErr := b.memory.Search(ctx, identity, memory.SearchInput{
		Query: call.Query, Namespace: call.Namespace, Sensitivity: call.Sensitivity, Limit: int(call.Limit),
	})
	if searchErr != nil {
		return toolErrorResult("memory search failed: " + boundedMessage(searchErr)), nil
	}
	results := make([]map[string]any, 0, len(records))
	const maxMemoryResponseBytes = 1 << 20
	used := 0
	truncated := false
	for _, record := range records {
		if used+len(record.Content) > maxMemoryResponseBytes {
			truncated = true
			break
		}
		results = append(results, map[string]any{
			"id": record.ID.String(), "namespace": record.Namespace, "key": record.Key,
			"contentType": record.ContentType, "content": record.Content,
			"resourceVersion": record.ResourceVersion, "createdAt": record.CreatedAt.Format(time.RFC3339),
		})
		used += len(record.Content)
	}
	document, _ := json.Marshal(map[string]any{"records": results, "truncated": truncated})
	return textResult(document, false), nil
}

// spawnTask extends the calling attempt's workflow with one dynamic step.
// The workflow lineage comes from the fenced identity (worker-injected);
// the agent only names the step, its goal and its task spec. Standalone
// tasks (no workflow) deny closed.
func (b *Broker) spawnTask(ctx context.Context, params json.RawMessage) (any, *Error) {
	var call spawnToolInput
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
	return resolveAttemptIdentity(ctx, b.identity)
}

// resolveAttemptIdentity is the single identity boundary for system and
// tenant tools. A reported execution id must never fall back to an unrelated
// default window when the configured resolver cannot scope it.
func resolveAttemptIdentity(ctx context.Context, resolver IdentityResolver) (AttemptContext, error) {
	if execution := ExecutionID(ctx); execution != "" {
		scoped, ok := resolver.(ExecutionIdentityResolver)
		if !ok {
			return AttemptContext{}, ErrNoExecutionContext
		}
		return scoped.ResolveExecution(ctx, execution)
	}
	return resolver.Resolve(ctx)
}

// granted is the default-deny capability check: an empty grant denies.
func granted(requested string, allowed []string) bool {
	for _, candidate := range allowed {
		if capability.MatchGrant(candidate, requested) {
			return true
		}
	}
	return false
}

func effectiveMemorySensitivities(allowed []string) []string {
	if len(allowed) == 0 {
		return []string{"internal"}
	}
	return allowed
}

// schemaFor generates system-tool input schemas from the exact Go request
// types decoded by the handlers. This keeps tools/list and runtime validation
// on one contract and makes schema drift testable in CI.
func schemaFor[T any]() json.RawMessage {
	schema := schemaForType(reflect.TypeFor[T]())
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic("generate system tool schema: " + err.Error())
	}
	return encoded
}

func schemaForType(typ reflect.Type) map[string]any {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[json.RawMessage]() {
		return map[string]any{"type": "object"}
	}
	switch typ.Kind() {
	case reflect.Struct:
		properties := map[string]any{}
		required := []string{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "" {
				name = field.Name
			}
			if name == "-" {
				continue
			}
			property := schemaForType(field.Type)
			if enum := field.Tag.Get("enum"); enum != "" {
				property["enum"] = strings.Split(enum, ",")
			}
			properties[name] = property
			if field.Tag.Get("required") == "true" || (options != "omitempty" && field.Type.Kind() != reflect.Pointer) {
				required = append(required, name)
			}
		}
		result := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			result["required"] = required
		}
		return result
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaForType(typ.Elem())}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{"type": "string"}
	}
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
	return fmt.Sprintf("mcp/%s/%s/%s", identity.AttemptID, tool, hex.EncodeToString(digest[:]))
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
