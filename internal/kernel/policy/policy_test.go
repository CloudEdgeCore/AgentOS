package policy

import (
	"context"
	"slices"
	"testing"
)

func TestEngineAllowsWithinTenantPolicy(t *testing.T) {
	engine, err := New(TenantPolicies{"tenant-a": {MaxPriority: 90}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	decision := engine.Evaluate(context.Background(), "tenant-a", TaskContext{Priority: 70})
	if !decision.Allow {
		t.Fatalf("priority 70 was denied: %+v", decision)
	}
}

func TestEngineDeniesAboveTenantMaximum(t *testing.T) {
	engine, err := New(TenantPolicies{"tenant-a": {MaxPriority: 90}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	decision := engine.Evaluate(context.Background(), "tenant-a", TaskContext{Priority: 95})
	if decision.Allow {
		t.Fatal("priority 95 was allowed")
	}
	if !slices.Contains(decision.DenyReasons, "TASK_PRIORITY_EXCEEDS_TENANT_MAX") {
		t.Fatalf("missing deny reason: %+v", decision)
	}
}

func TestEngineDeniesUnknownTenant(t *testing.T) {
	engine, err := New(TenantPolicies{"tenant-a": {MaxPriority: 90}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	decision := engine.Evaluate(context.Background(), "tenant-unknown", TaskContext{Priority: 1})
	if decision.Allow {
		t.Fatal("unknown tenant was allowed")
	}
	if !slices.Contains(decision.DenyReasons, "TENANT_POLICY_NOT_FOUND") {
		t.Fatalf("missing deny reason: %+v", decision)
	}
}

func TestEngineBoundaryPriorityIsAllowed(t *testing.T) {
	engine, err := New(TenantPolicies{"tenant-a": {MaxPriority: 90}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if decision := engine.Evaluate(context.Background(), "tenant-a", TaskContext{Priority: 90}); !decision.Allow {
		t.Fatalf("boundary priority 90 was denied: %+v", decision)
	}
}

func TestBrokenPolicyFailsClosedAtConstruction(t *testing.T) {
	if _, err := newWithModule("package broken {{", TenantPolicies{"tenant-a": {MaxPriority: 90}}); err == nil {
		t.Fatal("broken policy module compiled")
	}
}

func TestEmptyTenantPoliciesDenyEverything(t *testing.T) {
	engine, err := New(TenantPolicies{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	decision := engine.Evaluate(context.Background(), "tenant-a", TaskContext{Priority: 1})
	if decision.Allow {
		t.Fatal("empty policy set allowed a task")
	}
}

func TestToolAllowedByTenantPolicy(t *testing.T) {
	engine, err := New(TenantPolicies{"tenant-a": {AllowedTools: []string{"github.read"}}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	decision := engine.EvaluateTool(context.Background(), "tenant-a", ToolContext{
		Name: "github.read", Version: "1.0.0", Action: "read", Resource: "github:acme/repo", Risk: "low",
	})
	if !decision.Allow || decision.RequiresApproval {
		t.Fatalf("allowed tool was gated: %+v", decision)
	}
}

func TestToolDeniedByDefault(t *testing.T) {
	engine, err := New(TenantPolicies{"tenant-a": {AllowedTools: []string{"github.read"}}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	decision := engine.EvaluateTool(context.Background(), "tenant-a", ToolContext{
		Name: "shell.exec", Version: "1.0.0", Action: "write", Resource: "shell:*", Risk: "high",
	})
	if decision.Allow {
		t.Fatal("unlisted tool was allowed")
	}
	if !slices.Contains(decision.DenyReasons, "TOOL_NOT_ALLOWED") {
		t.Fatalf("missing deny reason: %+v", decision)
	}
}

func TestHighRiskToolRequiresApproval(t *testing.T) {
	engine, err := New(TenantPolicies{"tenant-a": {
		AllowedTools: []string{"github.write"}, ApprovalRequiredRisk: "high",
	}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	decision := engine.EvaluateTool(context.Background(), "tenant-a", ToolContext{
		Name: "github.write", Version: "1.0.0", Action: "write", Resource: "github:acme/repo", Risk: "high",
	})
	if !decision.Allow || !decision.RequiresApproval {
		t.Fatalf("high-risk tool must require approval: %+v", decision)
	}
}

func TestLowRiskToolDoesNotRequireApproval(t *testing.T) {
	engine, err := New(TenantPolicies{"tenant-a": {
		AllowedTools: []string{"github.write"}, ApprovalRequiredRisk: "high",
	}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	decision := engine.EvaluateTool(context.Background(), "tenant-a", ToolContext{
		Name: "github.write", Version: "1.0.0", Action: "read", Resource: "github:acme/repo", Risk: "low",
	})
	if !decision.Allow || decision.RequiresApproval {
		t.Fatalf("low-risk action must not require approval: %+v", decision)
	}
}

func TestToolDeniedForUnknownTenant(t *testing.T) {
	engine, err := New(TenantPolicies{"tenant-a": {AllowedTools: []string{"github.read"}}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	decision := engine.EvaluateTool(context.Background(), "tenant-unknown", ToolContext{Name: "github.read", Risk: "low"})
	if decision.Allow {
		t.Fatal("unknown tenant was allowed a tool")
	}
	if !slices.Contains(decision.DenyReasons, "TENANT_POLICY_NOT_FOUND") {
		t.Fatalf("missing deny reason: %+v", decision)
	}
}

func TestModelAllowedByTenantPolicy(t *testing.T) {
	engine, err := New(TenantPolicies{"tenant-a": {AllowedModels: []string{"openai/gpt-4o"}}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	decision := engine.EvaluateModel(context.Background(), "tenant-a", ModelContext{Name: "openai/gpt-4o"})
	if !decision.Allow {
		t.Fatalf("allowed model was denied: %+v", decision)
	}
}

func TestModelDeniedByDefault(t *testing.T) {
	engine, err := New(TenantPolicies{"tenant-a": {AllowedModels: []string{"openai/gpt-4o"}}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	decision := engine.EvaluateModel(context.Background(), "tenant-a", ModelContext{Name: "anthropic/claude"})
	if decision.Allow {
		t.Fatal("unlisted model was allowed")
	}
	if !slices.Contains(decision.DenyReasons, "MODEL_NOT_ALLOWED") {
		t.Fatalf("missing deny reason: %+v", decision)
	}
}
