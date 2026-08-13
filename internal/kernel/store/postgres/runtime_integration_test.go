//go:build integration

package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/admission"
	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	"github.com/bian-cloud-skill/agentos/internal/kernel/scheduler"
	kernelstore "github.com/bian-cloud-skill/agentos/internal/kernel/store"
	postgresstore "github.com/bian-cloud-skill/agentos/internal/kernel/store/postgres"
	"github.com/google/uuid"
)

func TestRuntimeCheckpointCompletionAndIdempotency(t *testing.T) {
	clock := newFakeClock()
	_, repository := prepare(t, clock.Now)
	ctx := context.Background()
	assignment := scheduleRuntimeTask(t, ctx, repository, "runtime-complete", 3)

	starting, err := repository.TransitionAttempt(ctx, kernelstore.TransitionAttemptInput{
		AttemptID: assignment.Attempt.ID, FencingToken: 1, ExpectedAttemptVersion: 1, To: domain.AttemptStarting,
	})
	if err != nil {
		t.Fatal(err)
	}
	running, err := repository.TransitionAttempt(ctx, kernelstore.TransitionAttemptInput{
		AttemptID: assignment.Attempt.ID, FencingToken: 1, ExpectedAttemptVersion: starting.ResourceVersion, To: domain.AttemptRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpointInput := kernelstore.CommitCheckpointInput{
		TenantID: "tenant-a", AttemptID: assignment.Attempt.ID, FencingToken: 1,
		ExpectedAttemptVersion: running.ResourceVersion, IdempotencyKey: "checkpoint-1", CheckpointID: uuid.New(),
		AgentVersionRef: "agent:v1", Provider: "reference-go", RuntimeABI: "agentos.reference/v1",
		SchemaVersion: "state/v1", State: artifactReference("artifact://tenant-a/sha256/checkpoint", "checkpoint-state"),
		ConfirmedReceiptIDs: []string{"receipt-b", "receipt-a", "receipt-a"},
	}
	checkpoint, checkpointedAttempt, err := repository.CommitCheckpoint(ctx, checkpointInput)
	if err != nil {
		t.Fatalf("commit checkpoint: %v", err)
	}
	if checkpoint.Ordinal != 1 || checkpointedAttempt.ResourceVersion != running.ResourceVersion+1 || len(checkpoint.ConfirmedReceiptIDs) != 2 {
		t.Fatalf("unexpected checkpoint: %+v attempt=%+v", checkpoint, checkpointedAttempt)
	}
	retriedCheckpoint, retriedAttempt, err := repository.CommitCheckpoint(ctx, checkpointInput)
	if err != nil || retriedCheckpoint.ID != checkpoint.ID || retriedAttempt.ResourceVersion != checkpointedAttempt.ResourceVersion {
		t.Fatalf("idempotent checkpoint retry: checkpoint=%+v attempt=%+v err=%v", retriedCheckpoint, retriedAttempt, err)
	}
	conflict := checkpointInput
	conflict.SchemaVersion = "state/v2"
	if _, _, err := repository.CommitCheckpoint(ctx, conflict); !errors.Is(err, kernelstore.ErrIdempotencyConflict) {
		t.Fatalf("expected checkpoint idempotency conflict, got %v", err)
	}

	completion := kernelstore.CompleteAttemptInput{
		TenantID: "tenant-a", AttemptID: assignment.Attempt.ID, FencingToken: 1,
		ExpectedAttemptVersion: checkpointedAttempt.ResourceVersion, IdempotencyKey: "complete-1",
		Result: artifactReference("artifact://tenant-a/sha256/result", "durable-result"),
	}
	completed, err := repository.CompleteAttempt(ctx, completion)
	if err != nil {
		t.Fatalf("complete attempt: %v", err)
	}
	if completed.Attempt.Phase != domain.AttemptCompleted || completed.Run.Phase != domain.RunCompleted ||
		completed.Task.Phase != domain.TaskSucceeded || completed.Task.ResultRef != completion.Result.URI {
		t.Fatalf("completion did not converge atomically: %+v", completed)
	}
	retriedCompletion, err := repository.CompleteAttempt(ctx, completion)
	if err != nil || retriedCompletion.Task.ID != completed.Task.ID || retriedCompletion.Task.Phase != domain.TaskSucceeded {
		t.Fatalf("idempotent completion retry: %+v err=%v", retriedCompletion, err)
	}
	if _, err := repository.HeartbeatLease(ctx, kernelstore.HeartbeatLeaseInput{
		AttemptID: assignment.Attempt.ID, FencingToken: 1, ExpectedLeaseVersion: 1, TTL: time.Minute,
	}); !errors.Is(err, kernelstore.ErrFenced) {
		t.Fatalf("completed owner retained a lease: %v", err)
	}
}

func TestExpiredRuntimeRecoversCheckpointWithHigherFence(t *testing.T) {
	clock := newFakeClock()
	_, repository := prepare(t, clock.Now)
	ctx := context.Background()
	assignment := scheduleRuntimeTask(t, ctx, repository, "runtime-recovery", 2)
	starting, err := repository.TransitionAttempt(ctx, kernelstore.TransitionAttemptInput{
		AttemptID: assignment.Attempt.ID, FencingToken: 1, ExpectedAttemptVersion: 1, To: domain.AttemptStarting,
	})
	if err != nil {
		t.Fatal(err)
	}
	running, err := repository.TransitionAttempt(ctx, kernelstore.TransitionAttemptInput{
		AttemptID: assignment.Attempt.ID, FencingToken: 1, ExpectedAttemptVersion: starting.ResourceVersion, To: domain.AttemptRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _, err := repository.CommitCheckpoint(ctx, kernelstore.CommitCheckpointInput{
		TenantID: "tenant-a", AttemptID: assignment.Attempt.ID, FencingToken: 1,
		ExpectedAttemptVersion: running.ResourceVersion, IdempotencyKey: "recoverable-checkpoint", CheckpointID: uuid.New(),
		AgentVersionRef: "agent:v1", Provider: "reference-go", RuntimeABI: "agentos.reference/v1",
		SchemaVersion: "state/v1", State: artifactReference("artifact://tenant-a/sha256/recovery", "recoverable-state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(31 * time.Second)
	candidates, err := repository.ListExpiredAttempts(ctx, clock.Now(), 10)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("expired candidates=%+v err=%v", candidates, err)
	}
	recovered, err := repository.RecoverExpiredAttempt(ctx, kernelstore.RecoverExpiredAttemptInput{
		TenantID: "tenant-a", AttemptID: assignment.Attempt.ID, FencingToken: 1,
		NewAttemptID: uuid.New(), NewLeaseID: uuid.New(), LeaseTTL: 30 * time.Second, MaxAttempts: 2,
	})
	if err != nil || !recovered.Retried || recovered.Lease.Attempt.FencingToken != 2 || recovered.Lease.Attempt.Ordinal != 2 {
		t.Fatalf("recovery=%+v err=%v", recovered, err)
	}
	resumed, err := repository.GetRuntimeAssignment(ctx, "tenant-a", recovered.Lease.Attempt.ID, 2)
	if err != nil || resumed.ResumeCheckpoint == nil || resumed.ResumeCheckpoint.ID != checkpoint.ID {
		t.Fatalf("resumed assignment=%+v err=%v", resumed, err)
	}
	if _, err := repository.GetRuntimeAssignment(ctx, "tenant-a", assignment.Attempt.ID, 1); !errors.Is(err, kernelstore.ErrFenced) {
		t.Fatalf("old owner was not fenced: %v", err)
	}

	clock.Advance(31 * time.Second)
	exhausted, err := repository.RecoverExpiredAttempt(ctx, kernelstore.RecoverExpiredAttemptInput{
		TenantID: "tenant-a", AttemptID: recovered.Lease.Attempt.ID, FencingToken: 2,
		NewAttemptID: uuid.New(), NewLeaseID: uuid.New(), LeaseTTL: 30 * time.Second, MaxAttempts: 2,
	})
	if err != nil || exhausted.Retried {
		t.Fatalf("exhausted recovery=%+v err=%v", exhausted, err)
	}
	task, err := repository.GetTask(ctx, "tenant-a", assignment.Task.ID)
	if err != nil || task.Phase != domain.TaskFailed {
		t.Fatalf("exhausted task=%+v err=%v", task, err)
	}
}

func TestRunningTaskCancellationConvergesThroughRuntime(t *testing.T) {
	clock := newFakeClock()
	_, repository := prepare(t, clock.Now)
	ctx := context.Background()
	assignment := scheduleRuntimeTask(t, ctx, repository, "runtime-cancel", 3)
	cancelRequested, err := repository.RequestTaskCancellation(ctx, "tenant-a", assignment.Task.ID, assignment.Task.ResourceVersion)
	if err != nil {
		t.Fatalf("request cancellation: %v", err)
	}
	if cancelRequested.Phase != domain.TaskRunning || cancelRequested.CancelRequestedAt == nil {
		t.Fatalf("cancellation was not durably requested: %+v", cancelRequested)
	}
	current, err := repository.GetRuntimeAssignment(ctx, "tenant-a", assignment.Attempt.ID, 1)
	if err != nil || current.Attempt.Phase != domain.AttemptCancelRequested {
		t.Fatalf("cancelled assignment=%+v err=%v", current, err)
	}
	input := kernelstore.CancelAttemptInput{
		TenantID: "tenant-a", AttemptID: current.Attempt.ID, FencingToken: 1,
		ExpectedAttemptVersion: current.Attempt.ResourceVersion, IdempotencyKey: "cancel-ack-1",
	}
	cancelled, err := repository.AcknowledgeCancellation(ctx, input)
	if err != nil {
		t.Fatalf("acknowledge cancellation: %v", err)
	}
	if cancelled.Attempt.Phase != domain.AttemptCancelled || cancelled.Run.Phase != domain.RunCancelled || cancelled.Task.Phase != domain.TaskCancelled {
		t.Fatalf("cancellation did not converge atomically: %+v", cancelled)
	}
	retried, err := repository.AcknowledgeCancellation(ctx, input)
	if err != nil || retried.Task.Phase != domain.TaskCancelled {
		t.Fatalf("idempotent cancellation retry=%+v err=%v", retried, err)
	}
}

func scheduleRuntimeTask(t *testing.T, ctx context.Context, repository *postgresstore.Store, key string, maxAttempts int) kernelstore.RuntimeAssignment {
	t.Helper()
	created, err := repository.CreateTask(ctx, kernelstore.CreateTaskInput{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: "agent:v1", Goal: key,
		IdempotencyKey: key, Spec: []byte(`{
			"priority":70,"budget":{"tokens":500,"costUsd":2,"toolCalls":10,"wallSeconds":60},
			"placement":{"runtimeClasses":["oci"],"preferredClass":"oci","region":"cn-east","cpuMillis":100,"memoryMiB":128,"llmConcurrency":1},
			"retryPolicy":{"maxAttempts":` + strconv.Itoa(maxAttempts) + `}
		}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := admission.New(admission.Limits{
		RuntimeClasses: []string{"oci"}, MaxTokens: 1000, MaxCostUSD: 10, MaxToolCalls: 100,
		MaxWallSeconds: 3600, MaxCPU: 2000, MaxMemory: 4096, MaxLLMConcurrency: 4,
	})
	if count, err := admission.NewController(repository, engine, "admission-"+key, 10, time.Minute).Reconcile(ctx); err != nil || count != 1 {
		t.Fatalf("admission count=%d err=%v", count, err)
	}
	pools := staticPools{{
		ID: "pool-cn-1", TenantIDs: []string{"tenant-a"}, RuntimeClass: "oci", RuntimeInstanceID: "worker-cn-1",
		Region: "cn-east", Ready: true, AvailableCPU: 2000, AvailableMemory: 4096, AvailableLLMSlots: 4,
	}}
	if count, err := scheduler.NewController(repository, pools, "scheduler-"+key, 10, time.Minute, 30*time.Second).Reconcile(ctx); err != nil || count != 1 {
		t.Fatalf("scheduler count=%d err=%v", count, err)
	}
	assignment, err := repository.PollRuntimeAssignment(ctx, "tenant-a", "worker-cn-1")
	if err != nil || assignment.Task.ID != created.Task.ID {
		t.Fatalf("poll assignment=%+v err=%v", assignment, err)
	}
	return assignment
}

func artifactReference(uri, content string) kernelstore.ArtifactReference {
	digest := sha256.Sum256([]byte(content))
	return kernelstore.ArtifactReference{URI: uri, SHA256: digest, SizeBytes: int64(len(content)), MediaType: "application/octet-stream"}
}
