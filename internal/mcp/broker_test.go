package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/memory"
	kernelmodel "github.com/CloudEdgeCore/AgentOS/internal/kernel/model"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/model/provider"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/tool"
	"github.com/google/uuid"
)

type fakeModelBroker struct {
	input  kernelmodel.InvokeInput
	output kernelmodel.InvokeOutput
	err    error
}

func (f *fakeModelBroker) InvokeStream(_ context.Context, input kernelmodel.InvokeInput, _ func(string)) (kernelmodel.InvokeOutput, error) {
	f.input = input
	return f.output, f.err
}

type fakeMemoryBroker struct {
	putInput    memory.PutInput
	searchInput memory.SearchInput
	records     []store.MemoryRecord
}

func (f *fakeMemoryBroker) Put(_ context.Context, _ AttemptContext, input memory.PutInput) (store.MemoryRecord, bool, error) {
	f.putInput = input
	return store.MemoryRecord{
		ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), TenantID: input.TenantID,
		Namespace: input.Namespace, Key: input.Key, ResourceVersion: 1,
	}, false, nil
}

func (f *fakeMemoryBroker) Search(_ context.Context, _ AttemptContext, input memory.SearchInput) ([]store.MemoryRecord, error) {
	f.searchInput = input
	if f.records != nil {
		return f.records, nil
	}
	return []store.MemoryRecord{{
		ID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), TenantID: input.TenantID,
		Namespace: "runs", Key: "note", Content: "recall me", ResourceVersion: 3,
	}}, nil
}

type fakeToolInvokerForBroker struct {
	listed []store.ToolDescriptor
}

func (f *fakeToolInvokerForBroker) InvokeTool(_ context.Context, input tool.InvokeInput) (tool.InvokeResult, error) {
	return tool.InvokeResult{}, nil
}
func (f *fakeToolInvokerForBroker) ListTools(_ context.Context, _ string) ([]store.ToolDescriptor, error) {
	return f.listed, nil
}

func brokerTestContext() AttemptContext {
	return AttemptContext{
		TenantID: "tenant-1", TaskID: uuid.New(), RunID: uuid.New(), AttemptID: uuid.New(),
		FencingToken: 5, AgentVersionRef: "agent@1",
		AllowedModels:           []string{"fake/agent-model"},
		AllowedMemoryNamespaces: []string{"runs"},
	}
}

func newBrokerForTest(t *testing.T, identity AttemptContext, models ModelBroker, memories MemoryBroker, listed []store.ToolDescriptor) *Broker {
	t.Helper()
	slot := &StaticIdentity{Context: identity}
	tools := NewToolAdapter(&fakeToolInvokerForBroker{listed: listed}, slot)
	return NewBroker(tools, models, memories, slot)
}

func decodeToolJSON(t *testing.T, result any) map[string]any {
	t.Helper()
	document, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is not an MCP result object: %#v", result)
	}
	content, ok := document["content"].([]map[string]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v", document["content"])
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(content[0]["text"].(string)), &parsed); err != nil {
		t.Fatalf("decode tool payload: %v", err)
	}
	return parsed
}

func TestBrokerListsSystemToolsAlongsideTenantTools(t *testing.T) {
	broker := newBrokerForTest(t, brokerTestContext(), &fakeModelBroker{}, &fakeMemoryBroker{},
		[]store.ToolDescriptor{{Name: "echo.dev", Version: "1.0.0", ParamsSchema: []byte(`{"type":"object"}`)}})
	listed, rpcErr := broker.ListTools(context.Background(), json.RawMessage(`{}`))
	if rpcErr != nil {
		t.Fatalf("list: %v", rpcErr)
	}
	names := map[string]bool{}
	for _, tool := range listed.(map[string]any)["tools"].([]map[string]any) {
		names[tool["name"].(string)] = true
	}
	for _, expected := range []string{"echo.dev", SystemModelInvoke, SystemMemoryPut, SystemMemorySearch} {
		if !names[expected] {
			t.Fatalf("tool %q missing from %v", expected, names)
		}
	}
}

func TestBrokerModelInvokeEnforcesGrantAndFencing(t *testing.T) {
	models := &fakeModelBroker{
		output: kernelmodel.InvokeOutput{
			Call: store.ModelCall{ID: uuid.MustParse("00000000-0000-0000-0000-00000000000a"),
				TenantID: "tenant-1", ModelRef: "fake/agent-model", Status: store.ModelCallCompleted,
				InputTokens: 30, OutputTokens: 12, CostUSD: 0.0002, FinishReason: "stop", ProviderRequestID: "req-1"},
			Content: "the answer",
		},
	}
	broker := newBrokerForTest(t, brokerTestContext(), models, nil, nil)

	result, rpcErr := broker.CallTool(context.Background(), mustJSON(t, map[string]any{
		"name":      SystemModelInvoke,
		"arguments": map[string]any{"modelRef": "fake/agent-model", "messages": []map[string]any{{"role": "user", "content": "hi"}}},
	}))
	if rpcErr != nil {
		t.Fatalf("call: %v", rpcErr)
	}
	payload := decodeToolJSON(t, result)
	if payload["status"] != "COMPLETED" || payload["content"] != "the answer" {
		t.Fatalf("payload = %#v", payload)
	}
	usage := payload["usage"].(map[string]any)
	if usage["inputTokens"] != float64(30) || usage["outputTokens"] != float64(12) {
		t.Fatalf("usage = %#v", usage)
	}
	// The invocation carries the fenced identity injected by the runtime.
	if models.input.TenantID != "tenant-1" || models.input.FencingToken != 5 || models.input.AgentVersionRef != "agent@1" {
		t.Fatalf("invocation identity = %+v", models.input)
	}
	if !strings.HasPrefix(models.input.IdempotencyKey, "mcp/") {
		t.Fatalf("idempotency key = %q", models.input.IdempotencyKey)
	}

	// A model outside the grant is denied as a tool outcome.
	result, rpcErr = broker.CallTool(context.Background(), mustJSON(t, map[string]any{
		"name":      SystemModelInvoke,
		"arguments": map[string]any{"modelRef": "other/model", "messages": []map[string]any{{"role": "user", "content": "hi"}}},
	}))
	if rpcErr != nil {
		t.Fatalf("call: %v", rpcErr)
	}
	payload = decodeToolJSON(t, result)
	if payload["error"] == nil || !strings.Contains(payload["error"].(string), "capability grant") {
		t.Fatalf("expected capability denial, got %#v", payload)
	}
}

func TestBrokerModelInvokeReportsProviderFailure(t *testing.T) {
	models := &fakeModelBroker{
		output: kernelmodel.InvokeOutput{Call: store.ModelCall{ID: uuid.New(), Status: store.ModelCallFailed, FinishReason: "provider_rejected"}},
		err:    provider.ErrProviderRejected,
	}
	broker := newBrokerForTest(t, brokerTestContext(), models, nil, nil)
	result, rpcErr := broker.CallTool(context.Background(), mustJSON(t, map[string]any{
		"name":      SystemModelInvoke,
		"arguments": map[string]any{"modelRef": "fake/agent-model", "messages": []map[string]any{{"role": "user", "content": "hi"}}},
	}))
	if rpcErr != nil {
		t.Fatalf("call: %v", rpcErr)
	}
	payload := decodeToolJSON(t, result)
	if payload["error"] != "PROVIDER_REJECTED" {
		t.Fatalf("failure payload = %#v", payload)
	}
}

func TestBrokerMemoryToolsEnforceNamespaceGrantAndProvenance(t *testing.T) {
	memories := &fakeMemoryBroker{}
	broker := newBrokerForTest(t, brokerTestContext(), nil, memories, nil)

	result, rpcErr := broker.CallTool(context.Background(), mustJSON(t, map[string]any{
		"name":      SystemMemoryPut,
		"arguments": map[string]any{"namespace": "runs", "key": "fact-1", "content": "learned"},
	}))
	if rpcErr != nil {
		t.Fatalf("put: %v", rpcErr)
	}
	payload := decodeToolJSON(t, result)
	if payload["key"] != "fact-1" || payload["replayed"] != false {
		t.Fatalf("put payload = %#v", payload)
	}
	if memories.putInput.Provenance["agentVersionRef"] != "agent@1" || memories.putInput.Provenance["via"] != "mcp:"+SystemMemoryPut {
		t.Fatalf("provenance = %#v", memories.putInput.Provenance)
	}
	if memories.putInput.Provenance["via"] != "mcp:"+SystemMemoryPut {
		t.Fatalf("provenance = %#v", memories.putInput.Provenance)
	}

	// A namespace outside the grant is denied.
	result, _ = broker.CallTool(context.Background(), mustJSON(t, map[string]any{
		"name":      SystemMemoryPut,
		"arguments": map[string]any{"namespace": "secret", "key": "x", "content": "y"},
	}))
	payload = decodeToolJSON(t, result)
	if payload["error"] == nil || !strings.Contains(payload["error"].(string), "capability grant") {
		t.Fatalf("expected namespace denial, got %#v", payload)
	}

	result, rpcErr = broker.CallTool(context.Background(), mustJSON(t, map[string]any{
		"name":      SystemMemorySearch,
		"arguments": map[string]any{"query": "recall"},
	}))
	if rpcErr != nil {
		t.Fatalf("search: %v", rpcErr)
	}
	payload = decodeToolJSON(t, result)
	records := payload["records"].([]any)
	if len(records) != 1 || records[0].(map[string]any)["content"] != "recall me" {
		t.Fatalf("search payload = %#v", payload)
	}
	if memories.searchInput.TenantID != "" || memories.putInput.TenantID != "" {
		t.Fatal("broker must not pass tenant through agent-controlled input; the kernel adapter derives it from identity")
	}
}

// closedWindowResolver simulates an MCP call outside any execution window.
type closedWindowResolver struct{}

func (closedWindowResolver) Resolve(context.Context) (AttemptContext, error) {
	return AttemptContext{}, errors.New("no active execution window")
}

func TestBrokerDeniesWithoutIdentity(t *testing.T) {
	models := &fakeModelBroker{
		output: kernelmodel.InvokeOutput{Call: store.ModelCall{ID: uuid.New(), Status: store.ModelCallCompleted}},
	}
	tools := NewToolAdapter(&fakeToolInvokerForBroker{}, closedWindowResolver{})
	broker := NewBroker(tools, models, &fakeMemoryBroker{}, closedWindowResolver{})
	result, rpcErr := broker.CallTool(context.Background(), mustJSON(t, map[string]any{
		"name":      SystemModelInvoke,
		"arguments": map[string]any{"modelRef": "fake/agent-model", "messages": []map[string]any{{"role": "user", "content": "hi"}}},
	}))
	if rpcErr != nil {
		t.Fatalf("call: %v", rpcErr)
	}
	payload := decodeToolJSON(t, result)
	if !strings.Contains(payload["error"].(string), "no fenced attempt identity") {
		t.Fatalf("expected default deny, got %#v", payload)
	}
}

func TestBrokerDelegatesTenantTools(t *testing.T) {
	models := &fakeModelBroker{
		output: kernelmodel.InvokeOutput{Call: store.ModelCall{ID: uuid.New(), Status: store.ModelCallCompleted}},
	}
	broker := newBrokerForTest(t, brokerTestContext(), models, &fakeMemoryBroker{}, nil)
	// The tenant tool path (unknown to the broker) is delegated: it fails in
	// the fake invoker with unknown-tool, proving dispatch reached it.
	_, rpcErr := broker.CallTool(context.Background(), mustJSON(t, map[string]any{
		"name": "some.tenant.tool", "arguments": map[string]any{},
	}))
	if rpcErr == nil || !strings.Contains(fmt.Sprintf("%v", rpcErr.Data), "unknown tool") {
		t.Fatalf("expected delegated unknown-tool error, got %v", rpcErr)
	}
	if models.input.ModelRef != "" {
		t.Fatal("model broker must not be invoked for tenant tools")
	}
}

func TestBrokerFailsClosedWithoutBrokers(t *testing.T) {
	identity := brokerTestContext()
	identity.AllowedModels = nil
	tools := NewToolAdapter(&fakeToolInvokerForBroker{}, &StaticIdentity{Context: identity})
	broker := NewBroker(tools, nil, nil, &StaticIdentity{Context: identity})
	_, rpcErr := broker.CallTool(context.Background(), mustJSON(t, map[string]any{
		"name":      SystemModelInvoke,
		"arguments": map[string]any{"modelRef": "fake/agent-model", "messages": []map[string]any{{"role": "user", "content": "hi"}}},
	}))
	if rpcErr == nil {
		t.Fatal("model invocation must fail closed without a broker")
	}
	listed, _ := broker.ListTools(context.Background(), json.RawMessage(`{}`))
	if tools := listed.(map[string]any)["tools"].([]map[string]any); len(tools) != 0 {
		t.Fatalf("system tools must not be listed without brokers: %v", tools)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return encoded
}
