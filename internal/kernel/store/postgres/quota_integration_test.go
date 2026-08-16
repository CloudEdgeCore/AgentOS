//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	kernelstore "github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
)

// TestTenantQuotaConfigurationAndWindowAccounting covers the store contract:
// configure/replace/remove a quota, read the current window's consumption,
// and prove settlements bump the window exactly once (idempotent replays and
// delta settlements never double-count).
func TestTenantQuotaConfigurationAndWindowAccounting(t *testing.T) {
	clock := newFakeClock()
	pool, repository := prepare(t, clock.Now)
	ctx := context.Background()

	configured, err := repository.SetTenantQuota(ctx, kernelstore.SetTenantQuotaInput{
		TenantID: "tenant-a", WindowSeconds: 3600,
		Limits: kernelstore.TaskBudget{Tokens: 1000, CostUSD: 10, ToolCalls: 100, WallSeconds: 3600},
	})
	if err != nil {
		t.Fatalf("set quota: %v", err)
	}
	if configured.WindowSeconds != 3600 || configured.Limits.Tokens != 1000 || configured.ResourceVersion != 1 {
		t.Fatalf("unexpected quota: %+v", configured)
	}

	quota, err := repository.GetTenantQuota(ctx, "tenant-a")
	if err != nil || quota.Limits.CostUSD != 10 {
		t.Fatalf("get quota: %+v err=%v", quota, err)
	}

	// A fresh tenant has zero consumption in the current window.
	usage, err := repository.GetTenantQuotaUsage(ctx, "tenant-a", clock.Now())
	if err != nil {
		t.Fatalf("get usage: %v", err)
	}
	if !usage.Consumed.Zero() || usage.WindowStart != clock.Now().Truncate(time.Hour) {
		t.Fatalf("unexpected fresh usage: %+v", usage)
	}

	// Settling task usage bumps the window exactly once.
	task := createBudgetedTask(t, ctx, repository, "quota-accounting", 200, 20, 120, 2.0)
	if _, err := repository.SettleTaskUsage(ctx, kernelstore.SettleTaskUsageInput{
		TenantID: "tenant-a", TaskID: task.ID, IdempotencyKey: "quota-usage-1",
		Usage: kernelstore.TaskBudget{Tokens: 40, CostUSD: 0.5, ToolCalls: 4, WallSeconds: 60},
	}); err != nil {
		t.Fatalf("settle usage: %v", err)
	}
	usage, err = repository.GetTenantQuotaUsage(ctx, "tenant-a", clock.Now())
	if err != nil {
		t.Fatalf("get usage after settlement: %v", err)
	}
	if usage.Consumed.Tokens != 40 || usage.Consumed.CostUSD != 0.5 ||
		usage.Consumed.ToolCalls != 4 || usage.Consumed.WallSeconds != 60 {
		t.Fatalf("unexpected window consumption: %+v", usage.Consumed)
	}

	// Replaying the same settlement must not double-count the window.
	if _, err := repository.SettleTaskUsage(ctx, kernelstore.SettleTaskUsageInput{
		TenantID: "tenant-a", TaskID: task.ID, IdempotencyKey: "quota-usage-1",
		Usage: kernelstore.TaskBudget{Tokens: 40, CostUSD: 0.5, ToolCalls: 4, WallSeconds: 60},
	}); err != nil {
		t.Fatalf("replay settlement: %v", err)
	}
	usage, _ = repository.GetTenantQuotaUsage(ctx, "tenant-a", clock.Now())
	if usage.Consumed.Tokens != 40 {
		t.Fatalf("replay double-counted the window: %+v", usage.Consumed)
	}

	// The delta settlement path also bumps the window exactly once.
	if _, err := repository.SettleTaskUsageDelta(ctx, kernelstore.SettleTaskUsageDeltaInput{
		TenantID: "tenant-a", TaskID: task.ID, FamilyPrefix: "model:call-1",
		IdempotencyKey: "model:call-1:finish", Target: kernelstore.TaskBudget{Tokens: 30},
	}); err != nil {
		t.Fatalf("delta settlement: %v", err)
	}
	if _, err := repository.SettleTaskUsageDelta(ctx, kernelstore.SettleTaskUsageDeltaInput{
		TenantID: "tenant-a", TaskID: task.ID, FamilyPrefix: "model:call-1",
		IdempotencyKey: "model:call-1:finish", Target: kernelstore.TaskBudget{Tokens: 30},
	}); err != nil {
		t.Fatalf("delta replay: %v", err)
	}
	usage, _ = repository.GetTenantQuotaUsage(ctx, "tenant-a", clock.Now())
	if usage.Consumed.Tokens != 70 {
		t.Fatalf("delta settlement counted %d times: %+v", usage.Consumed.Tokens/30, usage.Consumed)
	}

	// Replacing the quota with the same window length preserves window
	// consumption (only limits change). A different window length re-anchors
	// future windows: historical rows stay at the old granularity, so the
	// current window reads fresh.
	replaced, err := repository.SetTenantQuota(ctx, kernelstore.SetTenantQuotaInput{
		TenantID: "tenant-a", WindowSeconds: 3600,
		Limits: kernelstore.TaskBudget{Tokens: 5000},
	})
	if err != nil || replaced.ResourceVersion != 2 {
		t.Fatalf("replace quota: %+v err=%v", replaced, err)
	}
	usage, _ = repository.GetTenantQuotaUsage(ctx, "tenant-a", clock.Now())
	if usage.Consumed.Tokens != 70 {
		t.Fatalf("replacement lost window consumption: %+v", usage.Consumed)
	}

	// Removing the quota makes usage reads report not-found and stops
	// tracking new windows (existing rows are inert).
	if err := repository.DeleteTenantQuota(ctx, "tenant-a"); err != nil {
		t.Fatalf("delete quota: %v", err)
	}
	if _, err := repository.GetTenantQuota(ctx, "tenant-a"); !errors.Is(err, kernelstore.ErrNotFound) {
		t.Fatalf("deleted quota still readable: %v", err)
	}
	if _, err := repository.GetTenantQuotaUsage(ctx, "tenant-a", clock.Now()); !errors.Is(err, kernelstore.ErrNotFound) {
		t.Fatalf("deleted quota still tracks usage: %v", err)
	}
	clock.Advance(2 * time.Hour)
	if _, err := repository.SettleTaskUsage(ctx, kernelstore.SettleTaskUsageInput{
		TenantID: "tenant-a", TaskID: task.ID, IdempotencyKey: "quota-usage-2",
		Usage: kernelstore.TaskBudget{Tokens: 5},
	}); err != nil {
		t.Fatalf("settle after quota removal: %v", err)
	}
	var windowRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_consumption_windows
		WHERE tenant_id = $1`, "tenant-a").Scan(&windowRows); err != nil {
		t.Fatalf("count windows: %v", err)
	}
	if windowRows != 1 {
		t.Fatalf("windows tracked after quota removal: %d", windowRows)
	}
}

// TestTenantQuotaAdmissionGate proves the atomic admission gate: an admit is
// rejected with ErrTenantQuotaExceeded when the current window's consumption
// plus the task's ceiling exceeds any limit, and the rejection records
// nothing (task stays queued). Window rollover reopens the tenant.
func TestTenantQuotaAdmissionGate(t *testing.T) {
	clock := newFakeClock()
	pool, repository := prepare(t, clock.Now)
	ctx := context.Background()
	if _, err := repository.SetTenantQuota(ctx, kernelstore.SetTenantQuotaInput{
		TenantID: "tenant-a", WindowSeconds: 86400,
		Limits: kernelstore.TaskBudget{Tokens: 100},
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	admitWith := func(key string, budget *kernelstore.TaskBudget) (kernelstore.Task, error) {
		created, err := repository.CreateTask(ctx, kernelstore.CreateTaskInput{
			ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: "agent:v1",
			Goal: key, Spec: []byte(`{}`), IdempotencyKey: key,
		})
		if err != nil {
			t.Fatalf("create task %s: %v", key, err)
		}
		// Older queued tasks (gate-rejected ones with expired claims) may be
		// claimed first, so scan for the claim of the task we just created.
		claims, err := repository.ClaimTasks(ctx, kernelstore.ClaimTasksInput{
			Kind: kernelstore.ControllerAdmission, Phase: domain.TaskQueued, OwnerID: "admission", Limit: 50, TTL: time.Minute,
		})
		if err != nil {
			t.Fatalf("claim tasks for %s: %v", key, err)
		}
		var claim *kernelstore.TaskClaim
		for i := range claims {
			if claims[i].Task.ID == created.Task.ID {
				claim = &claims[i]
				break
			}
		}
		if claim == nil {
			t.Fatalf("no claim for task %s among %d claims", key, len(claims))
		}
		return repository.DecideAdmission(ctx, kernelstore.DecideAdmissionInput{
			TaskID: created.Task.ID, TenantID: "tenant-a", OwnerID: "admission",
			ClaimFencingToken: claim.FencingToken, ExpectedTaskVersion: created.Task.ResourceVersion,
			Admit: true, ReasonCode: "ADMISSION_PASSED", EvaluatorVersion: "test/v1", Budget: budget,
		})
	}

	// The gate is exact against settled consumption: three budgeted tasks of
	// 60 are all admitted while nothing is settled (the burst may overshoot;
	// the window closes once consumption catches up).
	t1, err := admitWith("quota-gate-t1", &kernelstore.TaskBudget{Tokens: 60})
	if err != nil {
		t.Fatalf("admit t1: %v", err)
	}
	t2, err := admitWith("quota-gate-t2", &kernelstore.TaskBudget{Tokens: 60})
	if err != nil {
		t.Fatalf("admit t2: %v", err)
	}
	if _, err := admitWith("quota-gate-t3", &kernelstore.TaskBudget{Tokens: 60}); err != nil {
		t.Fatalf("admit t3: %v", err)
	}

	// Settle 40 on t1 (its ledger ceiling is 60): the window now shows 40.
	if _, err := repository.SettleTaskUsage(ctx, kernelstore.SettleTaskUsageInput{
		TenantID: "tenant-a", TaskID: t1.ID, IdempotencyKey: "quota-gate-u1",
		Usage: kernelstore.TaskBudget{Tokens: 40},
	}); err != nil {
		t.Fatalf("settle t1: %v", err)
	}

	// 40 + 60 = 100: exactly at the limit, still admitted.
	if _, err := admitWith("quota-gate-t4", &kernelstore.TaskBudget{Tokens: 60}); err != nil {
		t.Fatalf("admit t4 at the limit: %v", err)
	}
	// A 61-ceiling task no longer fits (40+61 > 100): rejected at the gate,
	// task stays queued, nothing recorded.
	if _, err := admitWith("quota-gate-t3b", &kernelstore.TaskBudget{Tokens: 61}); !errors.Is(err, kernelstore.ErrTenantQuotaExceeded) {
		t.Fatalf("expected quota rejection for t3b, got %v", err)
	}
	var rejectedID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM tasks WHERE tenant_id = $1 AND idempotency_key = $2`,
		"tenant-a", "quota-gate-t3b").Scan(&rejectedID); err != nil {
		t.Fatalf("read rejected task: %v", err)
	}
	rejected, err := repository.GetTask(ctx, "tenant-a", rejectedID)
	if err != nil || rejected.Phase != domain.TaskQueued {
		t.Fatalf("rejected task state: %+v err=%v", rejected, err)
	}
	var decisions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admission_decisions
		WHERE tenant_id = $1 AND task_id = $2`, "tenant-a", rejectedID.String()).Scan(&decisions); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if decisions != 0 {
		t.Fatalf("quota rejection recorded %d decisions", decisions)
	}

	// Settle t2 to its ceiling (window 100): an unbudgeted task is still
	// admitted at the limit, but any positive ceiling is not.
	if _, err := repository.SettleTaskUsage(ctx, kernelstore.SettleTaskUsageInput{
		TenantID: "tenant-a", TaskID: t2.ID, IdempotencyKey: "quota-gate-u2",
		Usage: kernelstore.TaskBudget{Tokens: 60},
	}); err != nil {
		t.Fatalf("settle t2: %v", err)
	}
	if _, err := admitWith("quota-gate-t5", nil); err != nil {
		t.Fatalf("unbudgeted task rejected at the limit: %v", err)
	}
	if _, err := admitWith("quota-gate-t6", &kernelstore.TaskBudget{Tokens: 1}); !errors.Is(err, kernelstore.ErrTenantQuotaExceeded) {
		t.Fatalf("expected quota rejection for t6, got %v", err)
	}

	// Settle t1 to its ceiling (window 110, over the limit): now even an
	// unbudgeted task is rejected.
	if _, err := repository.SettleTaskUsage(ctx, kernelstore.SettleTaskUsageInput{
		TenantID: "tenant-a", TaskID: t1.ID, IdempotencyKey: "quota-gate-u3",
		Usage: kernelstore.TaskBudget{Tokens: 20},
	}); err != nil {
		t.Fatalf("settle t1 to ceiling: %v", err)
	}
	if _, err := admitWith("quota-gate-t7", nil); !errors.Is(err, kernelstore.ErrTenantQuotaExceeded) {
		t.Fatalf("expected quota rejection for over-limit unbudgeted task, got %v", err)
	}

	// Window rollover reopens the tenant with a fresh window.
	clock.Advance(24 * time.Hour)
	if _, err := admitWith("quota-gate-t8", &kernelstore.TaskBudget{Tokens: 60}); err != nil {
		t.Fatalf("admit t8 in the new window: %v", err)
	}
	usage, err := repository.GetTenantQuotaUsage(ctx, "tenant-a", clock.Now())
	if err != nil {
		t.Fatalf("get new window usage: %v", err)
	}
	if usage.WindowStart != clock.Now().Truncate(24*time.Hour) || !usage.Consumed.Zero() {
		t.Fatalf("unexpected new window usage: %+v", usage)
	}
}

// TestTenantQuotaDoesNotGateUnconfiguredTenants proves that a tenant without
// a quota row is admitted and settled without any window tracking.
func TestTenantQuotaDoesNotGateUnconfiguredTenants(t *testing.T) {
	clock := newFakeClock()
	_, repository := prepare(t, clock.Now)
	ctx := context.Background()
	task := createBudgetedTask(t, ctx, repository, "quota-none", 100, 10, 60, 1.0)
	if _, err := repository.SettleTaskUsage(ctx, kernelstore.SettleTaskUsageInput{
		TenantID: "tenant-a", TaskID: task.ID, IdempotencyKey: "quota-none-u1",
		Usage: kernelstore.TaskBudget{Tokens: 10},
	}); err != nil {
		t.Fatalf("settle usage: %v", err)
	}
	if _, err := repository.GetTenantQuotaUsage(ctx, "tenant-a", clock.Now()); !errors.Is(err, kernelstore.ErrNotFound) {
		t.Fatalf("unconfigured tenant tracked usage: %v", err)
	}
}
