package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/bian-cloud-skill/agentos/internal/kernel/tool"
	"github.com/google/uuid"
)

var testIdentity = AttemptContext{
	TenantID: "tenant-a", TaskID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
	RunID:        uuid.MustParse("22222222-2222-2222-2222-222222222222"),
	AttemptID:    uuid.MustParse("33333333-3333-3333-3333-333333333333"),
	FencingToken: 1, AgentVersionRef: "agent@1",
}

func TestAdapterListsToolsWithSchemas(t *testing.T) {
	invoker := &fakeInvoker{descriptors: []store.ToolDescriptor{
		{Name: "fs.read", Version: "1.0.0", SideEffectRisk: store.ToolRiskLow, Actions: []string{"read"},
			ResourcePatterns: []string{"fs:/tmp"}, ParamsSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
	}}
	adapter := NewToolAdapter(invoker, StaticIdentity{Context: testIdentity})

	result, rpcErr := adapter.ListTools(context.Background(), nil)
	if rpcErr != nil {
		t.Fatalf("ListTools: %v", rpcErr)
	}
	tools := result.(map[string]any)["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["name"] != "fs.read" {
		t.Fatalf("tools: %+v", tools)
	}
	schema, ok := tools[0]["inputSchema"].(json.RawMessage)
	if !ok || string(schema) == "" {
		t.Fatalf("inputSchema missing: %+v", tools[0])
	}
}

func TestAdapterCallsToolWithMappedIdentityAndDeterministicKey(t *testing.T) {
	invoker := &fakeInvoker{descriptors: []store.ToolDescriptor{
		{Name: "fs.read", Version: "1.0.0", SideEffectRisk: store.ToolRiskLow, Actions: []string{"read"},
			ResourcePatterns: []string{"fs:/tmp"}, ParamsSchema: json.RawMessage(`{"type":"object"}`)},
	}}
	adapter := NewToolAdapter(invoker, StaticIdentity{Context: testIdentity})

	params := json.RawMessage(`{"name":"fs.read","arguments":{"path":"a.txt"}}`)
	result, rpcErr := adapter.CallTool(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("CallTool: %v", rpcErr)
	}
	if invoker.calls != 1 || invoker.input.AttemptID != testIdentity.AttemptID ||
		invoker.input.TenantID != testIdentity.TenantID || invoker.input.Action != "read" || invoker.input.Resource != "fs:/tmp" {
		t.Fatalf("invoker input: %+v calls=%d", invoker.input, invoker.calls)
	}
	if result.(map[string]any)["isError"] != false {
		t.Fatalf("unexpected error result: %+v", result)
	}

	// Identical call replays with the same deterministic idempotency key.
	invoker.replay = true
	if _, rpcErr := adapter.CallTool(context.Background(), params); rpcErr != nil {
		t.Fatalf("replay CallTool: %v", rpcErr)
	}
	if invoker.keys[0] != invoker.keys[1] {
		t.Fatalf("idempotency keys differ across identical calls: %v", invoker.keys)
	}
}

func TestAdapterSurfacesDeniedAndApprovalOutcomes(t *testing.T) {
	invoker := &fakeInvoker{descriptors: []store.ToolDescriptor{
		{Name: "fs.write", Version: "1.0.0", SideEffectRisk: store.ToolRiskHigh, Actions: []string{"write"},
			ResourcePatterns: []string{"fs:/tmp"}, ParamsSchema: json.RawMessage(`{"type":"object"}`)},
	}}
	adapter := NewToolAdapter(invoker, StaticIdentity{Context: testIdentity})

	invoker.denied = true
	result, rpcErr := adapter.CallTool(context.Background(), json.RawMessage(`{"name":"fs.write","arguments":{}}`))
	if rpcErr != nil {
		t.Fatalf("denied CallTool: %v", rpcErr)
	}
	denied := result.(map[string]any)
	if denied["isError"] != true || !json.Valid([]byte(denied["content"].([]map[string]any)[0]["text"].(string))) {
		t.Fatalf("denied result: %+v", denied)
	}

	invoker.denied = false
	invoker.approvalID = uuid.New()
	result, rpcErr = adapter.CallTool(context.Background(), json.RawMessage(`{"name":"fs.write","arguments":{}}`))
	if rpcErr != nil {
		t.Fatalf("approval CallTool: %v", rpcErr)
	}
	if result.(map[string]any)["isError"] != true {
		t.Fatalf("approval-required result must be an error outcome: %+v", result)
	}
}

func TestAdapterRejectsUnknownToolAndMalformedParams(t *testing.T) {
	invoker := &fakeInvoker{descriptors: []store.ToolDescriptor{
		{Name: "fs.read", Version: "1.0.0", ParamsSchema: json.RawMessage(`{"type":"object"}`)},
	}}
	adapter := NewToolAdapter(invoker, StaticIdentity{Context: testIdentity})

	if _, rpcErr := adapter.CallTool(context.Background(), json.RawMessage(`{"name":"nope","arguments":{}}`)); rpcErr == nil {
		t.Fatal("unknown tool accepted")
	}
	if _, rpcErr := adapter.CallTool(context.Background(), json.RawMessage(`{"name":"fs.read","arguments":{},"extra":1}`)); rpcErr == nil {
		t.Fatal("unknown field accepted")
	}
	if _, rpcErr := adapter.CallTool(context.Background(), json.RawMessage(`{}`)); rpcErr == nil {
		t.Fatal("missing name accepted")
	}
}

type fakeInvoker struct {
	descriptors []store.ToolDescriptor
	input       tool.InvokeInput
	calls       int
	keys        []string
	denied      bool
	approvalID  uuid.UUID
	replay      bool
}

func (f *fakeInvoker) ListTools(context.Context, string) ([]store.ToolDescriptor, error) {
	return f.descriptors, nil
}

func (f *fakeInvoker) InvokeTool(_ context.Context, in tool.InvokeInput) (tool.InvokeResult, error) {
	f.calls++
	f.input = in
	f.keys = append(f.keys, in.IdempotencyKey)
	if f.denied {
		return tool.InvokeResult{Outcome: tool.OutcomeDenied, DenyReasons: []string{"TOOL_NOT_ALLOWED"}}, nil
	}
	if f.approvalID != uuid.Nil {
		return tool.InvokeResult{Outcome: tool.OutcomeRequiresApproval, ApprovalID: &f.approvalID}, nil
	}
	if f.replay {
		return tool.InvokeResult{Outcome: tool.OutcomeReplayed, Result: json.RawMessage(`{"ok":true}`), ReceiptOperation: "TOOL:fs.read@1.0.0"}, nil
	}
	return tool.InvokeResult{Outcome: tool.OutcomeExecuted, Result: json.RawMessage(`{"ok":true}`), ReceiptOperation: "TOOL:fs.read@1.0.0"}, nil
}
