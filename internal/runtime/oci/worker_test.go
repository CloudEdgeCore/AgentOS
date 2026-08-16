package oci

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	runtimev1alpha1 "github.com/bian-cloud-skill/agentos/gen/go/agentos/runtime/v1alpha1"
	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/bian-cloud-skill/agentos/internal/platform/artifact"
	runtimecontrol "github.com/bian-cloud-skill/agentos/internal/runtime/control"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestWorkerCompletesAssignmentThroughRuntimeProtocol(t *testing.T) {
	repository := newFakeRuntimeStore()
	executor := &fakeExecutor{result: RunResult{ExitCode: 0, UsageMillis: 42}}
	worker := newTestWorker(t, repository, executor, "worker-1")

	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("RunOnce() = %v, %v", processed, err)
	}
	if repository.attempt.Phase != domain.AttemptCompleted || repository.task.Phase != domain.TaskSucceeded ||
		repository.checkpoint == nil || repository.task.ResultRef == "" {
		t.Fatalf("worker did not complete execution: attempt=%+v task=%+v checkpoint=%+v", repository.attempt, repository.task, repository.checkpoint)
	}
	if len(executor.prepared) != 1 || len(executor.destroyed) != 1 {
		t.Fatalf("executor lifecycle: prepared=%v destroyed=%v", executor.prepared, executor.destroyed)
	}
	if spec := executor.prepared[0]; spec.AttemptID != repository.attempt.ID.String() ||
		spec.AgentVersionRef != repository.task.AgentVersionRef || spec.ImageRef != "example.com/agent@sha256:abcd" {
		t.Fatalf("execution spec does not match assignment: %+v", spec)
	}
	if repository.checkpoint.Provider != ProviderName || repository.checkpoint.RuntimeABI != RuntimeABI ||
		repository.checkpoint.SchemaVersion != CheckpointSchema {
		t.Fatalf("checkpoint envelope is not provider-compatible: %+v", repository.checkpoint)
	}

	processed, err = worker.RunOnce(context.Background())
	if err != nil || processed {
		t.Fatalf("completed assignment was polled again: %v, %v", processed, err)
	}
}

func TestWorkerFailsAttemptOnPrepareError(t *testing.T) {
	repository := newFakeRuntimeStore()
	executor := &fakeExecutor{prepareErr: errors.New("runsc unavailable")}
	worker := newTestWorker(t, repository, executor, "worker-1")

	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("RunOnce() = %v, %v", processed, err)
	}
	if repository.attempt.Phase != domain.AttemptFailed {
		t.Fatalf("attempt phase = %s, want ATTEMPT_FAILED", repository.attempt.Phase)
	}
	if repository.attempt.FailureCode != "execution_prepare_failed" {
		t.Fatalf("failure code = %q, want execution_prepare_failed", repository.attempt.FailureCode)
	}
	if repository.task.Phase != domain.TaskRunning || repository.checkpoint != nil {
		t.Fatalf("task must stay non-terminal with no checkpoint: task=%s checkpoint=%+v", repository.task.Phase, repository.checkpoint)
	}
}

func TestWorkerFailsAttemptOnNonZeroExit(t *testing.T) {
	repository := newFakeRuntimeStore()
	executor := &fakeExecutor{result: RunResult{ExitCode: 7, UsageMillis: 100}}
	worker := newTestWorker(t, repository, executor, "worker-1")

	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("RunOnce() = %v, %v", processed, err)
	}
	if repository.attempt.Phase != domain.AttemptFailed || repository.attempt.FailureCode != "exit_code_7" {
		t.Fatalf("attempt = %s (%s), want ATTEMPT_FAILED exit_code_7", repository.attempt.Phase, repository.attempt.FailureCode)
	}
	if len(executor.destroyed) != 1 {
		t.Fatalf("sandbox must be destroyed after exit: %v", executor.destroyed)
	}
}

func newTestWorker(t *testing.T, repository store.RuntimeStore, executor Executor, instanceID string) *Worker {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	runtimev1alpha1.RegisterRuntimeControlServiceServer(server, runtimecontrol.NewService(repository, "tenant-a", time.Minute))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	connection, err := grpc.NewClient("passthrough:///bufconn", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	artifacts, err := artifact.NewFilesystem(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return NewWorker(runtimev1alpha1.NewRuntimeControlServiceClient(connection), artifacts, executor,
		"tenant-a", instanceID, 30*time.Second, "example.com/agent@sha256:abcd")
}

// fakeExecutor records the lifecycle and returns scripted outcomes.
type fakeExecutor struct {
	prepared   []ExecutionSpec
	destroyed  []string
	prepareErr error
	waitErr    error
	result     RunResult
}

func (f *fakeExecutor) Prepare(_ context.Context, spec ExecutionSpec) (Execution, error) {
	f.prepared = append(f.prepared, spec)
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	return &fakeExecution{executor: f, id: "agentos-" + spec.AttemptID}, nil
}

func (f *fakeExecutor) Destroy(_ context.Context, execution Execution) error {
	f.destroyed = append(f.destroyed, execution.ID())
	return nil
}

type fakeExecution struct {
	executor *fakeExecutor
	id       string
}

func (e *fakeExecution) ID() string { return e.id }

func (e *fakeExecution) Wait(context.Context) (RunResult, error) {
	if e.executor.waitErr != nil {
		return RunResult{}, e.executor.waitErr
	}
	return e.executor.result, nil
}

// fakeRuntimeStore implements the store methods the Runtime Protocol service
// touches by embedding the interface; any other method panics on the nil
// embedded value, which is a test failure, not a production path.
type fakeRuntimeStore struct {
	store.RuntimeStore
	task       store.Task
	run        store.Run
	attempt    store.Attempt
	lease      store.Lease
	checkpoint *store.Checkpoint
}

func newFakeRuntimeStore() *fakeRuntimeStore {
	taskID, runID, attemptID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	return &fakeRuntimeStore{
		task: store.Task{ID: taskID, TenantID: "tenant-a", AgentVersionRef: "agent:v1", Goal: "deterministic result",
			Spec: []byte(`{"retryPolicy":{"maxAttempts":3}}`), Phase: domain.TaskRunning, ActiveRunID: &runID, ResourceVersion: 3},
		run: store.Run{ID: runID, TenantID: "tenant-a", TaskID: taskID, Phase: domain.RunRunning,
			ActiveAttemptID: &attemptID, CurrentFencingToken: 1, ResourceVersion: 1},
		attempt: store.Attempt{ID: attemptID, TenantID: "tenant-a", RunID: runID, Phase: domain.AttemptPlaced,
			RuntimeClass: "oci", RuntimePoolID: "pool-1", RuntimeInstanceID: "worker-1", FencingToken: 1, ResourceVersion: 1},
		lease: store.Lease{ID: uuid.New(), TenantID: "tenant-a", RunID: runID, AttemptID: attemptID,
			FencingToken: 1, ResourceVersion: 1, AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Minute)},
	}
}

func (f *fakeRuntimeStore) PollRuntimeAssignment(_ context.Context, tenantID, instanceID string) (store.RuntimeAssignment, error) {
	if tenantID != f.task.TenantID || instanceID != f.attempt.RuntimeInstanceID || f.attempt.Phase != domain.AttemptPlaced {
		return store.RuntimeAssignment{}, store.ErrNoAssignment
	}
	return f.assignment(), nil
}

func (f *fakeRuntimeStore) GetRuntimeAssignment(_ context.Context, tenantID string, attemptID uuid.UUID, token int64) (store.RuntimeAssignment, error) {
	if tenantID != f.task.TenantID || attemptID != f.attempt.ID || token != f.attempt.FencingToken {
		return store.RuntimeAssignment{}, store.ErrFenced
	}
	return f.assignment(), nil
}

func (f *fakeRuntimeStore) assignment() store.RuntimeAssignment {
	return store.RuntimeAssignment{Task: f.task, Run: f.run, Attempt: f.attempt, Lease: f.lease, ResumeCheckpoint: f.checkpoint}
}

func (f *fakeRuntimeStore) TransitionAttempt(_ context.Context, input store.TransitionAttemptInput) (store.Attempt, error) {
	if input.AttemptID != f.attempt.ID || input.FencingToken != f.attempt.FencingToken {
		return store.Attempt{}, store.ErrFenced
	}
	if input.ExpectedAttemptVersion != f.attempt.ResourceVersion {
		return store.Attempt{}, store.ErrVersionConflict
	}
	if err := domain.ValidateAttemptTransition(f.attempt.Phase, input.To); err != nil {
		return store.Attempt{}, errors.Join(store.ErrInvalidTransition, err)
	}
	f.attempt.Phase = input.To
	f.attempt.FailureCode, f.attempt.FailureMessage = input.FailureCode, input.FailureMessage
	f.attempt.ResourceVersion++
	return f.attempt, nil
}

func (f *fakeRuntimeStore) HeartbeatLease(_ context.Context, input store.HeartbeatLeaseInput) (store.Lease, error) {
	if input.FencingToken != f.lease.FencingToken || input.ExpectedLeaseVersion != f.lease.ResourceVersion {
		return store.Lease{}, store.ErrFenced
	}
	f.lease.ResourceVersion++
	f.lease.ExpiresAt = time.Now().Add(input.TTL)
	return f.lease, nil
}

func (f *fakeRuntimeStore) GetHeartbeatStatus(_ context.Context, tenantID string, attemptID uuid.UUID, token int64) (store.HeartbeatStatus, error) {
	if tenantID != f.task.TenantID || attemptID != f.attempt.ID || token != f.attempt.FencingToken {
		return store.HeartbeatStatus{}, store.ErrFenced
	}
	return store.HeartbeatStatus{CancelRequested: f.task.CancelRequestedAt != nil, AttemptVersion: f.attempt.ResourceVersion}, nil
}

func (f *fakeRuntimeStore) CommitCheckpoint(_ context.Context, input store.CommitCheckpointInput) (store.Checkpoint, store.Attempt, error) {
	if input.ExpectedAttemptVersion != f.attempt.ResourceVersion || input.FencingToken != f.attempt.FencingToken {
		return store.Checkpoint{}, store.Attempt{}, store.ErrVersionConflict
	}
	normalized, digest, err := input.Normalize()
	if err != nil {
		return store.Checkpoint{}, store.Attempt{}, err
	}
	checkpoint := store.Checkpoint{ID: normalized.CheckpointID, TenantID: normalized.TenantID, RunID: f.run.ID,
		AttemptID: f.attempt.ID, Ordinal: 1, FencingToken: input.FencingToken, AgentVersionRef: input.AgentVersionRef,
		RuntimeClass: f.attempt.RuntimeClass, Provider: input.Provider, RuntimeABI: input.RuntimeABI,
		SchemaVersion: input.SchemaVersion, State: input.State, EnvelopeSHA256: digest, CreatedAt: time.Now().UTC()}
	f.checkpoint = &checkpoint
	f.attempt.ResourceVersion++
	return checkpoint, f.attempt, nil
}

func (f *fakeRuntimeStore) CompleteAttempt(_ context.Context, input store.CompleteAttemptInput) (store.CompleteAttemptResult, error) {
	if input.ExpectedAttemptVersion != f.attempt.ResourceVersion || input.FencingToken != f.attempt.FencingToken {
		return store.CompleteAttemptResult{}, store.ErrVersionConflict
	}
	f.attempt.Phase = domain.AttemptCompleted
	f.attempt.ResourceVersion++
	f.run.Phase, f.run.ResourceVersion, f.run.ResultRef = domain.RunCompleted, f.run.ResourceVersion+1, input.Result.URI
	f.task.Phase, f.task.ResourceVersion, f.task.ResultRef = domain.TaskSucceeded, f.task.ResourceVersion+1, input.Result.URI
	return store.CompleteAttemptResult{Attempt: f.attempt, Run: f.run, Task: f.task}, nil
}
