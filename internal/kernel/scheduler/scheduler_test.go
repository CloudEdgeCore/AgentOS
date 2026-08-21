package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/bian-cloud-skill/agentos/internal/kernel/workload"
	"github.com/google/uuid"
)

// fakeSchedulingStore satisfies store.ControlStore with only the scheduling
// surface implemented; any other method panics, which is a test failure.
type fakeSchedulingStore struct {
	store.ControlStore
	claims     []store.TaskClaim
	scheduled  []uuid.UUID
	deferred   []store.DeferTaskScheduleInput
	released   int
	poisonSpec bool
	deferErr   error
}

func (f *fakeSchedulingStore) ClaimTasks(context.Context, store.ClaimTasksInput) ([]store.TaskClaim, error) {
	return f.claims, nil
}

func (f *fakeSchedulingStore) ReleaseTaskClaim(context.Context, store.TaskClaim) error {
	f.released++
	return nil
}

func (f *fakeSchedulingStore) DeferTaskSchedule(_ context.Context, in store.DeferTaskScheduleInput) (store.Task, error) {
	if f.deferErr != nil {
		return store.Task{}, f.deferErr
	}
	f.deferred = append(f.deferred, in)
	return store.Task{}, nil
}

func (f *fakeSchedulingStore) ScheduleTask(_ context.Context, in store.ScheduleTaskInput) (store.AttemptLease, error) {
	if f.poisonSpec {
		// The first claim's task carries the poisoned spec; decode happens
		// before ScheduleTask, so poisoning here simulates a commit failure.
		return store.AttemptLease{}, errors.New("schedule commit failure")
	}
	f.scheduled = append(f.scheduled, in.TaskID)
	return store.AttemptLease{}, nil
}

func (f *fakeSchedulingStore) GetAgentVersionByRef(context.Context, string, string) (store.AgentVersion, error) {
	return store.AgentVersion{}, store.ErrNotFound
}

// TestControllerIsolatesPoisonedTask proves the scheduler continues past a
// task it cannot schedule instead of starving the whole batch.
func TestControllerIsolatesPoisonedTask(t *testing.T) {
	repository := &fakeSchedulingStore{claims: []store.TaskClaim{
		{Task: admittedTask("poisoned", `not-json`)},
		{Task: admittedTask("healthy", `{
			"placement":{"runtimeClasses":["oci"],"region":"cn-east","cpuMillis":100,"memoryMiB":128,"llmConcurrency":1}
		}`)},
	}}
	controller := NewController(repository, StaticPoolSource{{
		ID: "pool-1", TenantIDs: []string{"tenant-a"}, RuntimeClass: "oci",
		RuntimeInstanceID: "worker-1", Region: "cn-east", Ready: true,
		AvailableCPU: 2000, AvailableMemory: 4096, AvailableLLMSlots: 4,
	}}, "scheduler-1", 10, time.Minute, time.Minute)

	processed, err := controller.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v, want nil (poisoned task must be isolated)", err)
	}
	if processed != 1 {
		t.Fatalf("processed = %d, want 1 (only the healthy task)", processed)
	}
	if len(repository.scheduled) != 1 {
		t.Fatalf("scheduled = %d, want 1", len(repository.scheduled))
	}
	if repository.released != 1 {
		t.Fatalf("released claims = %d, want 1 (poisoned claim released)", repository.released)
	}
}

func admittedTask(key, spec string) store.Task {
	return store.Task{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: "agent@1",
		Goal: key, Spec: json.RawMessage(spec), Phase: domain.TaskAdmitted, ResourceVersion: 1,
	}
}

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

// --- O6: no-placement claim release with backoff ---

// TestControllerDefersTaskOnNoPlacement proves O6: when no pool can place an
// admitted task, the claim is released immediately and the next attempt is
// deferred with the exponential backoff instead of pinning the claim until
// its TTL.
func TestControllerDefersTaskOnNoPlacement(t *testing.T) {
	repository := &fakeSchedulingStore{claims: []store.TaskClaim{{
		Task: admittedTask("nofit", `{
			"placement":{"runtimeClasses":["oci"],"region":"us-west","cpuMillis":100,"memoryMiB":128,"llmConcurrency":1}
		}`),
	}}}
	controller := NewController(repository, StaticPoolSource{{
		ID: "pool-1", TenantIDs: []string{"tenant-a"}, RuntimeClass: "oci",
		RuntimeInstanceID: "worker-1", Region: "cn-east", Ready: true,
		AvailableCPU: 2000, AvailableMemory: 4096, AvailableLLMSlots: 4,
	}}, "scheduler-1", 10, time.Minute, time.Minute)

	processed, err := controller.Reconcile(context.Background())
	if err != nil || processed != 0 {
		t.Fatalf("Reconcile() = %d, %v; want 0 processed", processed, err)
	}
	if len(repository.deferred) != 1 {
		t.Fatalf("deferrals = %d, want 1", len(repository.deferred))
	}
	deferred := repository.deferred[0]
	if deferred.TaskID != repository.claims[0].Task.ID {
		t.Fatalf("deferred task = %s, want %s", deferred.TaskID, repository.claims[0].Task.ID)
	}
	if deferred.Until.Before(time.Now().Add(4*time.Second)) || deferred.Until.After(time.Now().Add(6*time.Second)) {
		t.Fatalf("first deferral = %v, want ~5s backoff", deferred.Until)
	}
}

// TestControllerDeferralBackoffEscalatesExponentially proves the retry count
// drives the backoff: 5s, 10s, 20s, 40s, capped at 5 minutes.
func TestControllerDeferralBackoffEscalatesExponentially(t *testing.T) {
	expected := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 300 * time.Second, 300 * time.Second}
	for retries, want := range expected {
		if got := scheduleBackoff(int64(retries)); got != want {
			t.Fatalf("scheduleBackoff(%d) = %v, want %v", retries, got, want)
		}
	}
	if got := scheduleBackoff(50); got != 5*time.Minute {
		t.Fatalf("scheduleBackoff(50) = %v, want capped 5m", got)
	}
	if got := scheduleBackoff(-1); got != 5*time.Second {
		t.Fatalf("scheduleBackoff(-1) = %v, want base", got)
	}
}

// TestControllerDefersUsingTheTaskRetryCount proves the controller reads the
// task's persisted retry count when computing the backoff (multi-instance
// consistent: the progression lives on the task row, not in controller
// memory).
func TestControllerDefersUsingTheTaskRetryCount(t *testing.T) {
	repository := &fakeSchedulingStore{claims: []store.TaskClaim{{
		Task: admittedTask("nofit-retry3", `{
			"placement":{"runtimeClasses":["oci"],"region":"us-west","cpuMillis":100,"memoryMiB":128,"llmConcurrency":1}
		}`),
	}}}
	repository.claims[0].Task.ScheduleRetryCount = 3
	controller := NewController(repository, StaticPoolSource{{
		ID: "pool-1", TenantIDs: []string{"tenant-a"}, RuntimeClass: "oci",
		RuntimeInstanceID: "worker-1", Region: "cn-east", Ready: true,
		AvailableCPU: 2000, AvailableMemory: 4096, AvailableLLMSlots: 4,
	}}, "scheduler-1", 10, time.Minute, time.Minute)

	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(repository.deferred) != 1 {
		t.Fatalf("deferrals = %d, want 1", len(repository.deferred))
	}
	until := repository.deferred[0].Until
	if until.Before(time.Now().Add(39*time.Second)) || until.After(time.Now().Add(41*time.Second)) {
		t.Fatalf("deferral with retry count 3 = %v, want ~40s backoff", until)
	}
}

// TestControllerIsolatesDeferralFailure proves a failed deferral commit
// releases the claim and continues instead of starving the batch, while a
// retryable deferral failure aborts it.
func TestControllerIsolatesDeferralFailure(t *testing.T) {
	claim := store.TaskClaim{Task: admittedTask("defer-fail", `{
		"placement":{"runtimeClasses":["oci"],"region":"us-west","cpuMillis":100,"memoryMiB":128,"llmConcurrency":1}
	}`)}
	repository := &fakeSchedulingStore{claims: []store.TaskClaim{claim}}
	repository.deferErr = errors.New("deferral commit failure")
	controller := NewController(repository, StaticPoolSource{{
		ID: "pool-1", TenantIDs: []string{"tenant-a"}, RuntimeClass: "oci",
		RuntimeInstanceID: "worker-1", Region: "cn-east", Ready: true,
		AvailableCPU: 2000, AvailableMemory: 4096, AvailableLLMSlots: 4,
	}}, "scheduler-1", 10, time.Minute, time.Minute)

	processed, err := controller.Reconcile(context.Background())
	if err != nil || processed != 0 {
		t.Fatalf("Reconcile() = %d, %v", processed, err)
	}
	if repository.released != 1 {
		t.Fatalf("released = %d, want 1 (failed deferral releases the claim)", repository.released)
	}

	// A retryable deferral failure aborts the batch like any retryable error.
	repository2 := &fakeSchedulingStore{claims: []store.TaskClaim{claim}}
	repository2.deferErr = fmt.Errorf("%w: conflict", store.ErrRetryableTransaction)
	controller2 := NewController(repository2, StaticPoolSource{{
		ID: "pool-1", TenantIDs: []string{"tenant-a"}, RuntimeClass: "oci",
		RuntimeInstanceID: "worker-1", Region: "cn-east", Ready: true,
		AvailableCPU: 2000, AvailableMemory: 4096, AvailableLLMSlots: 4,
	}}, "scheduler-1", 10, time.Minute, time.Minute)
	if _, err := controller2.Reconcile(context.Background()); !store.IsRetryableTransaction(err) {
		t.Fatalf("Reconcile() error = %v, want retryable", err)
	}
}
