package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/bian-cloud-skill/agentos/internal/kernel/workload"
)

func TestSelectFiltersAndExplainsPlacement(t *testing.T) {
	spec := workload.Spec{Placement: workload.Placement{
		RuntimeClasses: []string{"oci"}, PreferredClass: "oci", Region: "cn-east",
		DataResidency: "cn", ArtifactRegion: "cn-east", CPU: 500, Memory: 512, LLMConcurrency: 1,
	}}
	pools := []RuntimePool{
		{ID: "not-ready", RuntimeClass: "oci", RuntimeInstanceID: "worker-0", Region: "cn-east", DataResidency: "cn"},
		{ID: "remote", RuntimeClass: "oci", RuntimeInstanceID: "worker-2", Region: "cn-east", DataResidency: "cn", Ready: true, AvailableCPU: 2000, AvailableMemory: 4096, AvailableLLMSlots: 4, CostWeight: 0.2},
		{ID: "local", RuntimeClass: "oci", RuntimeInstanceID: "worker-1", Region: "cn-east", DataResidency: "cn", Ready: true, AvailableCPU: 2000, AvailableMemory: 4096, AvailableLLMSlots: 4, ArtifactRegions: []string{"cn-east"}, CostWeight: 0.2},
		{ID: "wrong-region", RuntimeClass: "oci", RuntimeInstanceID: "worker-3", Region: "us-west", DataResidency: "us", Ready: true, AvailableCPU: 4000, AvailableMemory: 8192, AvailableLLMSlots: 8},
	}
	result, err := Select(spec, pools)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if result.Placement.Pool.ID != "local" {
		t.Fatalf("selected %q, want local: %+v", result.Placement.Pool.ID, result.Placement)
	}
	if len(result.Placement.Components) != 6 || result.Placement.Components[1].Name != "artifactLocality" || result.Placement.Components[1].Value != 20 {
		t.Fatalf("placement explanation is incomplete: %+v", result.Placement.Components)
	}
	if len(result.Rejected) != 2 {
		t.Fatalf("rejections = %+v, want two", result.Rejected)
	}
}

func TestStaticPoolSourceEnforcesTenantAllowlist(t *testing.T) {
	source := StaticPoolSource{
		{ID: "tenant-a", TenantIDs: []string{"tenant-a"}},
		{ID: "tenant-b", TenantIDs: []string{"tenant-b"}},
	}
	pools, err := source.ListRuntimePools(context.Background(), "tenant-a")
	if err != nil || len(pools) != 1 || pools[0].ID != "tenant-a" {
		t.Fatalf("tenant filtered pools=%+v err=%v", pools, err)
	}
}

func TestSelectIsDeterministicAndReportsNoFit(t *testing.T) {
	spec := workload.Spec{Placement: workload.Placement{
		RuntimeClasses: []string{"oci"}, Region: "cn-east", CPU: 100, Memory: 128, LLMConcurrency: 1,
	}}
	equal := []RuntimePool{
		{ID: "pool-b", RuntimeClass: "oci", RuntimeInstanceID: "b", Region: "cn-east", Ready: true, AvailableCPU: 1000, AvailableMemory: 1024, AvailableLLMSlots: 2},
		{ID: "pool-a", RuntimeClass: "oci", RuntimeInstanceID: "a", Region: "cn-east", Ready: true, AvailableCPU: 1000, AvailableMemory: 1024, AvailableLLMSlots: 2},
	}
	result, err := Select(spec, equal)
	if err != nil || result.Placement.Pool.ID != "pool-a" {
		t.Fatalf("deterministic selection failed: result=%+v err=%v", result, err)
	}

	_, err = Select(spec, []RuntimePool{{ID: "full", RuntimeClass: "oci", RuntimeInstanceID: "worker-full", Region: "cn-east", Ready: true}})
	if !errors.Is(err, ErrNoPlacement) {
		t.Fatalf("expected ErrNoPlacement, got %v", err)
	}
}

func TestSelectRejectsAmbiguousPoolIdentity(t *testing.T) {
	spec := workload.Spec{Placement: workload.Placement{
		RuntimeClasses: []string{"oci"}, Region: "cn-east", CPU: 1, Memory: 1, LLMConcurrency: 1,
	}}
	duplicate := RuntimePool{ID: "same", RuntimeClass: "oci", RuntimeInstanceID: "worker", Region: "cn-east", Ready: true, AvailableCPU: 10, AvailableMemory: 10, AvailableLLMSlots: 10}
	_, err := Select(spec, []RuntimePool{duplicate, duplicate})
	if !errors.Is(err, ErrInvalidPoolSet) {
		t.Fatalf("expected invalid pool set, got %v", err)
	}
}
