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
