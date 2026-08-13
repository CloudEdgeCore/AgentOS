//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	kernelstore "github.com/bian-cloud-skill/agentos/internal/kernel/store"
	postgresstore "github.com/bian-cloud-skill/agentos/internal/kernel/store/postgres"
	"github.com/bian-cloud-skill/agentos/internal/platform/migrate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseEnvironment = "AGENTOS_TEST_DATABASE_URL"

func TestTaskIdempotencyAndCAS(t *testing.T) {
	clock := newFakeClock()
	pool, store := prepare(t, clock.Now)
	ctx := context.Background()
	input := kernelstore.CreateTaskInput{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default",
		AgentVersionRef: "agent:v1", Goal: "prove idempotency",
		Spec: []byte(`{"limits":{"tokens":1000},"model":"test"}`), IdempotencyKey: "request-1",
	}

	created, err := store.CreateTask(ctx, input)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if created.Existing || created.Task.Phase != domain.TaskQueued || created.Task.ResourceVersion != 1 {
		t.Fatalf("unexpected created task: %+v", created)
	}

	retry := input
	retry.ID = uuid.New()
	retry.Spec = []byte(`{"model":"test","limits":{"tokens":1000}}`)
	existing, err := store.CreateTask(ctx, retry)
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if !existing.Existing || existing.Task.ID != created.Task.ID {
		t.Fatalf("retry did not return original task: %+v", existing)
	}

	conflict := retry
	conflict.Goal = "different request"
	if _, err := store.CreateTask(ctx, conflict); !errors.Is(err, kernelstore.ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}

	admitted, err := store.TransitionTask(ctx, created.Task.ID, 1, domain.TaskAdmitted)
	if err != nil {
		t.Fatalf("admit task: %v", err)
	}
	if admitted.ResourceVersion != 2 {
		t.Fatalf("task version = %d, want 2", admitted.ResourceVersion)
	}
	if _, err := store.TransitionTask(ctx, created.Task.ID, 1, domain.TaskCancelled); !errors.Is(err, kernelstore.ErrVersionConflict) {
		t.Fatalf("expected stale CAS rejection, got %v", err)
	}
	if _, err := store.TransitionTask(ctx, created.Task.ID, 2, domain.TaskSucceeded); !errors.Is(err, kernelstore.ErrResultRequired) {
		t.Fatalf("expected direct-success rejection, got %v", err)
	}

	var outboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if outboxCount != 2 {
		t.Fatalf("outbox count = %d, want 2", outboxCount)
	}
}

func TestConcurrentTaskCreationConvergesOnOneIdentity(t *testing.T) {
	clock := newFakeClock()
	_, store := prepare(t, clock.Now)
	ctx := context.Background()
	const callers = 8
	start := make(chan struct{})
	results := make(chan kernelstore.CreateTaskResult, callers)
	errorsFound := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			result, err := store.CreateTask(ctx, kernelstore.CreateTaskInput{
				ID: uuid.New(), TenantID: "tenant-a", Namespace: "default",
				AgentVersionRef: "agent:v1", Goal: "concurrent idempotency",
				Spec: []byte(`{"model":"test"}`), IdempotencyKey: "concurrent-request",
			})
			if err != nil {
				errorsFound <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent create: %v", err)
	}
	var identity uuid.UUID
	created := 0
	count := 0
	for result := range results {
		count++
		if identity == uuid.Nil {
			identity = result.Task.ID
		}
		if result.Task.ID != identity {
			t.Errorf("tasks did not converge: got %s and %s", identity, result.Task.ID)
		}
		if !result.Existing {
			created++
		}
	}
	if count != callers || created != 1 {
		t.Fatalf("successful callers=%d new creations=%d, want %d and 1", count, created, callers)
	}
}

func TestCompletionCommitsResultBeforeTaskSuccess(t *testing.T) {
	clock := newFakeClock()
	pool, store := prepare(t, clock.Now)
	ctx := context.Background()
	task, run := createAdmittedRun(t, ctx, store, "completion")

	owned, err := store.AcquireAttempt(ctx, kernelstore.AcquireAttemptInput{
		AttemptID: uuid.New(), LeaseID: uuid.New(), RunID: run.ID,
		ExpectedRunVersion: run.ResourceVersion, RuntimeClass: "oci", RuntimeInstanceID: "worker-1", TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("acquire attempt: %v", err)
	}
	starting, err := store.TransitionAttempt(ctx, kernelstore.TransitionAttemptInput{
		AttemptID: owned.Attempt.ID, FencingToken: owned.Attempt.FencingToken,
		ExpectedAttemptVersion: owned.Attempt.ResourceVersion, To: domain.AttemptStarting,
	})
	if err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	running, err := store.TransitionAttempt(ctx, kernelstore.TransitionAttemptInput{
		AttemptID: starting.ID, FencingToken: starting.FencingToken,
		ExpectedAttemptVersion: starting.ResourceVersion, To: domain.AttemptRunning,
	})
	if err != nil {
		t.Fatalf("run attempt: %v", err)
	}
	lease, err := store.HeartbeatLease(ctx, kernelstore.HeartbeatLeaseInput{
		AttemptID: running.ID, FencingToken: running.FencingToken,
		ExpectedLeaseVersion: owned.Lease.ResourceVersion, TTL: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if lease.ResourceVersion != 2 {
		t.Fatalf("lease version = %d, want 2", lease.ResourceVersion)
	}
	completed, err := store.TransitionAttempt(ctx, kernelstore.TransitionAttemptInput{
		AttemptID: running.ID, FencingToken: running.FencingToken,
		ExpectedAttemptVersion: running.ResourceVersion, To: domain.AttemptCompleted,
	})
	if err != nil {
		t.Fatalf("complete attempt: %v", err)
	}
	clock.Advance(3 * time.Minute)
	if _, err := store.AcquireAttempt(ctx, kernelstore.AcquireAttemptInput{
		AttemptID: uuid.New(), LeaseID: uuid.New(), RunID: run.ID,
		ExpectedRunVersion: owned.Run.ResourceVersion, RuntimeClass: "oci", RuntimeInstanceID: "worker-2", TTL: time.Minute,
	}); !errors.Is(err, kernelstore.ErrCompletionPending) {
		t.Fatalf("expected completed attempt to retain result-commit ownership, got %v", err)
	}

	var phase domain.TaskPhase
	var result *string
	if err := pool.QueryRow(ctx, `SELECT phase, result_ref FROM tasks WHERE id = $1`, task.ID.String()).Scan(&phase, &result); err != nil {
		t.Fatalf("read task before result commit: %v", err)
	}
	if phase != domain.TaskRunning || result != nil {
		t.Fatalf("task became terminal before result commit: phase=%s result=%v", phase, result)
	}

	finalRun, finalTask, err := store.CompleteRun(ctx, kernelstore.CompleteRunInput{
		RunID: run.ID, AttemptID: completed.ID, FencingToken: completed.FencingToken,
		ExpectedRunVersion: owned.Run.ResourceVersion, ResultRef: "cas://sha256/result-1",
	})
	if err != nil {
		t.Fatalf("commit run result: %v", err)
	}
	if finalRun.Phase != domain.RunCompleted || finalTask.Phase != domain.TaskSucceeded {
		t.Fatalf("unexpected completion: run=%s task=%s", finalRun.Phase, finalTask.Phase)
	}
	if finalRun.ResultRef != "cas://sha256/result-1" || finalTask.ResultRef != finalRun.ResultRef {
		t.Fatalf("result references diverged: run=%q task=%q", finalRun.ResultRef, finalTask.ResultRef)
	}
}

func TestExpiredLeaseIssuesNewFenceAndRejectsOldOwner(t *testing.T) {
	clock := newFakeClock()
	_, store := prepare(t, clock.Now)
	ctx := context.Background()
	_, run := createAdmittedRun(t, ctx, store, "fencing")

	oldOwner, err := store.AcquireAttempt(ctx, kernelstore.AcquireAttemptInput{
		AttemptID: uuid.New(), LeaseID: uuid.New(), RunID: run.ID,
		ExpectedRunVersion: run.ResourceVersion, RuntimeClass: "oci", RuntimeInstanceID: "worker-old", TTL: time.Second,
	})
	if err != nil {
		t.Fatalf("acquire old owner: %v", err)
	}
	clock.Advance(2 * time.Second)
	newOwner, err := store.AcquireAttempt(ctx, kernelstore.AcquireAttemptInput{
		AttemptID: uuid.New(), LeaseID: uuid.New(), RunID: run.ID,
		ExpectedRunVersion: oldOwner.Run.ResourceVersion, RuntimeClass: "oci", RuntimeInstanceID: "worker-new", TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("reacquire expired run: %v", err)
	}
	if newOwner.Attempt.FencingToken <= oldOwner.Attempt.FencingToken {
		t.Fatalf("fencing token did not increase: old=%d new=%d", oldOwner.Attempt.FencingToken, newOwner.Attempt.FencingToken)
	}
	if _, err := store.HeartbeatLease(ctx, kernelstore.HeartbeatLeaseInput{
		AttemptID: oldOwner.Attempt.ID, FencingToken: oldOwner.Attempt.FencingToken,
		ExpectedLeaseVersion: oldOwner.Lease.ResourceVersion, TTL: time.Minute,
	}); !errors.Is(err, kernelstore.ErrFenced) {
		t.Fatalf("expected stale heartbeat fencing, got %v", err)
	}
	if _, err := store.TransitionAttempt(ctx, kernelstore.TransitionAttemptInput{
		AttemptID: oldOwner.Attempt.ID, FencingToken: oldOwner.Attempt.FencingToken,
		ExpectedAttemptVersion: oldOwner.Attempt.ResourceVersion, To: domain.AttemptStarting,
	}); !errors.Is(err, kernelstore.ErrFenced) {
		t.Fatalf("expected stale transition fencing, got %v", err)
	}
}

func createAdmittedRun(t *testing.T, ctx context.Context, store *postgresstore.Store, key string) (kernelstore.Task, kernelstore.Run) {
	t.Helper()
	created, err := store.CreateTask(ctx, kernelstore.CreateTaskInput{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: "agent:v1",
		Goal: key, Spec: []byte(`{"model":"test"}`), IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	admitted, err := store.TransitionTask(ctx, created.Task.ID, created.Task.ResourceVersion, domain.TaskAdmitted)
	if err != nil {
		t.Fatalf("admit task: %v", err)
	}
	run, err := store.CreateRun(ctx, kernelstore.CreateRunInput{
		ID: uuid.New(), TaskID: admitted.ID, ExpectedTaskVersion: admitted.ResourceVersion,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return admitted, run
}

func prepare(t *testing.T, clock func() time.Time) (*pgxpool.Pool, *postgresstore.Store) {
	t.Helper()
	url := os.Getenv(testDatabaseEnvironment)
	if url == "" {
		t.Skipf("%s is not set", testDatabaseEnvironment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}
	migrations := filepath.Join("..", "..", "..", "..", "db", "migrations")
	if _, err := migrate.Apply(ctx, pool, migrations); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE inbox_receipts, outbox_events, runtime_leases, attempts, runs, tasks RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset database: %v", err)
	}
	return pool, postgresstore.NewWithClock(pool, clock)
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}
