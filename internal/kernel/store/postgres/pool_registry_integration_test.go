//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/scheduler"
)

func TestRuntimePoolRegistryUsesDynamicTenantGrants(t *testing.T) {
	clock := newFakeClock()
	_, repository := prepare(t, clock.Now)
	ctx := context.Background()
	pools := []scheduler.RuntimePool{{
		ID: "dynamic-a", TenantIDs: []string{"tenant-a"}, RuntimeClass: "oci",
		RuntimeInstanceID: "worker-a", Region: "cn-east", Ready: true, Status: "ACTIVE",
		AvailableCPU: 1000, AvailableMemory: 1024, AvailableLLMSlots: 2,
	}}
	if err := repository.RegisterRuntimePools(ctx, pools); err != nil {
		t.Fatal(err)
	}
	allowed, err := repository.ListRuntimePools(ctx, "tenant-a")
	if err != nil || len(allowed) != 1 || allowed[0].ID != "dynamic-a" {
		t.Fatalf("allowed=%+v err=%v", allowed, err)
	}
	denied, err := repository.ListRuntimePools(ctx, "tenant-b")
	if err != nil || len(denied) != 0 {
		t.Fatalf("cross-tenant pools=%+v err=%v", denied, err)
	}
	pools[0].TenantIDs = []string{"tenant-b"}
	pools[0].Status = "CORDONED"
	if err := repository.RegisterRuntimePools(ctx, pools); err != nil {
		t.Fatal(err)
	}
	allowed, _ = repository.ListRuntimePools(ctx, "tenant-a")
	granted, _ := repository.ListRuntimePools(ctx, "tenant-b")
	if len(allowed) != 0 || len(granted) != 1 || granted[0].Status != "CORDONED" {
		t.Fatalf("grant replacement allowed=%+v granted=%+v", allowed, granted)
	}
}
