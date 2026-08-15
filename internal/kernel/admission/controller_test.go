package admission

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/agentversion"
	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
)

func TestControllerRejectsUnpublishedAgentVersion(t *testing.T) {
	repository := &fakeControlStore{}
	controller := NewController(repository, New(testLimits()), "admission-1", 10, time.Minute)
	repository.claims = []store.TaskClaim{{Task: taskClaim("agent@1", `{
		"priority":70,"budget":{"tokens":500,"costUsd":2,"toolCalls":10,"wallSeconds":60},
		"placement":{"runtimeClasses":["oci"],"region":"cn-east","cpuMillis":100,"memoryMiB":128,"llmConcurrency":1}
	}`)}}

	processed, err := controller.Reconcile(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("Reconcile() = %d, %v", processed, err)
	}
	decision := repository.lastDecision()
	if decision.Admit || decision.ReasonCode != "AGENT_VERSION_NOT_FOUND" {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	if decision.AgentVersionID != nil {
		t.Fatalf("unpublished version was bound: %v", decision.AgentVersionID)
	}
}

func TestControllerRejectsRuntimeClassNotAllowedByVersion(t *testing.T) {
	repository := newFakeWithVersion("tenant-a", "agent@1", `{"runtimeClassPolicy":{"allowed":["wasm"]}}`)
	controller := NewController(repository, New(testLimits()), "admission-1", 10, time.Minute)
	repository.claims = []store.TaskClaim{{Task: taskClaim("agent@1", `{
		"priority":70,"budget":{"tokens":500,"costUsd":2,"toolCalls":10,"wallSeconds":60},
		"placement":{"runtimeClasses":["oci"],"region":"cn-east","cpuMillis":100,"memoryMiB":128,"llmConcurrency":1}
	}`)}}

	processed, err := controller.Reconcile(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("Reconcile() = %d, %v", processed, err)
	}
	decision := repository.lastDecision()
	if decision.Admit || decision.ReasonCode != "RUNTIME_CLASS_NOT_ALLOWED" {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	if decision.AgentVersionID == nil {
		t.Fatal("resolved version was not bound to the task")
	}
}

func TestControllerAdmitsAndBindsResolvedAgentVersion(t *testing.T) {
	repository := newFakeWithVersion("tenant-a", "agent@1", `{"runtimeClassPolicy":{"allowed":["oci","wasm"]}}`)
	controller := NewController(repository, New(testLimits()), "admission-1", 10, time.Minute)
	repository.claims = []store.TaskClaim{{Task: taskClaim("agent@1", `{
		"priority":70,"deadline":"2099-08-14T12:00:00Z",
		"budget":{"tokens":500,"costUsd":2,"toolCalls":10,"wallSeconds":60},
		"placement":{"runtimeClasses":["oci"],"preferredClass":"oci","region":"cn-east","cpuMillis":100,"memoryMiB":128,"llmConcurrency":1}
	}`)}}

	processed, err := controller.Reconcile(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("Reconcile() = %d, %v", processed, err)
	}
	decision := repository.lastDecision()
	if !decision.Admit || decision.ReasonCode != "ADMISSION_PASSED" {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	if decision.AgentVersionID == nil || decision.AgentVersionID.String() != repository.versionID("agent@1") {
		t.Fatalf("version was not bound correctly: %v", decision.AgentVersionID)
	}
}

func taskClaim(ref, spec string) store.Task {
	return store.Task{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: ref,
		Goal: "test", Spec: json.RawMessage(spec), Phase: domain.TaskQueued, ResourceVersion: 1,
	}
}

type fakeControlStore struct {
	mu        sync.Mutex
	versions  map[string]store.AgentVersion
	claims    []store.TaskClaim
	decisions []store.DecideAdmissionInput
}

func newFakeWithVersion(tenantID, ref, spec string) *fakeControlStore {
	repository := &fakeControlStore{versions: map[string]store.AgentVersion{}}
	name, version, err := agentversion.ParseRef(ref)
	if err != nil {
		panic(err)
	}
	repository.versions[tenantID+"/"+ref] = store.AgentVersion{
		ID: uuid.New(), TenantID: tenantID, Name: name, Version: version,
		Spec: json.RawMessage(spec), ResourceVersion: 1,
	}
	return repository
}

func (f *fakeControlStore) versionID(ref string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.versions["tenant-a/"+ref].ID.String()
}

func (f *fakeControlStore) lastDecision() store.DecideAdmissionInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.decisions[len(f.decisions)-1]
}

func (f *fakeControlStore) GetAgentVersionByRef(_ context.Context, tenantID, ref string) (store.AgentVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	version, ok := f.versions[tenantID+"/"+ref]
	if !ok {
		return store.AgentVersion{}, store.ErrNotFound
	}
	return version, nil
}

func (f *fakeControlStore) ClaimTasks(context.Context, store.ClaimTasksInput) ([]store.TaskClaim, error) {
	return f.claims, nil
}

func (f *fakeControlStore) DecideAdmission(_ context.Context, in store.DecideAdmissionInput) (store.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = append(f.decisions, in)
	return store.Task{Phase: domain.TaskAdmitted, ResourceVersion: in.ExpectedTaskVersion + 1}, nil
}

func (f *fakeControlStore) ReleaseTaskClaim(context.Context, store.TaskClaim) error { return nil }

func (f *fakeControlStore) GetTask(context.Context, string, uuid.UUID) (store.Task, error) {
	return store.Task{}, store.ErrNotFound
}

func (f *fakeControlStore) ScheduleTask(context.Context, store.ScheduleTaskInput) (store.AttemptLease, error) {
	return store.AttemptLease{}, nil
}

func (f *fakeControlStore) ClaimOutbox(context.Context, store.ClaimOutboxInput) ([]store.OutboxEvent, error) {
	return nil, nil
}

func (f *fakeControlStore) MarkOutboxPublished(context.Context, uuid.UUID, string, int64, time.Time) error {
	return nil
}

func (f *fakeControlStore) MarkOutboxFailed(context.Context, uuid.UUID, string, int64, string, time.Time) error {
	return nil
}
