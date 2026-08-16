package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	kernelstore "github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var _ kernelstore.BudgetStore = (*Store)(nil)

func (s *Store) GetTaskBudget(ctx context.Context, tenantID string, taskID uuid.UUID) (kernelstore.TaskBudgetStatus, error) {
	if strings.TrimSpace(tenantID) == "" || taskID == uuid.Nil {
		return kernelstore.TaskBudgetStatus{}, kernelstore.ErrBudgetNotReserved
	}
	// The ledger row carries rolling consumed counters (migration 000009),
	// so a budget read is a single row scan instead of a full re-SUM of the
	// append-only settlement ledger.
	status, err := scanBudgetStatus(s.pool.QueryRow(ctx, `SELECT `+budgetStatusColumns+`
		FROM task_budget_ledgers WHERE tenant_id = $1 AND task_id = $2`, tenantID, taskID.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return kernelstore.TaskBudgetStatus{}, kernelstore.ErrBudgetNotReserved
	}
	if err != nil {
		return kernelstore.TaskBudgetStatus{}, classify(err)
	}
	return status, nil
}

func (s *Store) SettleTaskUsage(ctx context.Context, in kernelstore.SettleTaskUsageInput) (kernelstore.TaskBudgetStatus, error) {
	var zero kernelstore.TaskBudgetStatus
	if !in.Valid() {
		return zero, fmt.Errorf("valid tenant, task, idempotency key, and positive usage are required")
	}
	// Budget settlement is a cross-row invariant (ADR-002): the idempotency
	// replay, exhaustion check and append share one transaction under the
	// ledger row lock.
	tx, err := s.begin(ctx)
	if err != nil {
		return zero, err
	}
	defer rollback(ctx, tx)
	status, consumed, err := s.settleUsageInTx(ctx, tx, in.TenantID, in.TaskID, in.IdempotencyKey, in.Usage)
	if err == nil || errors.Is(err, kernelstore.ErrBudgetExceeded) {
		// The hard-stop marker (exhausted = true) must be durable even when
		// the settlement itself is rejected.
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return zero, classify(commitErr)
		}
		status.Consumed = consumed
		return status, err
	}
	return zero, err
}

// settleUsageInTx records one idempotent usage settlement inside the caller's
// transaction: it locks the ledger row, deduplicates by idempotency key,
// enforces the hard stop, appends the settlement, and bumps the rolling
// consumed counters on the ledger row (migration 000009). On
// ErrBudgetExceeded the ledger is marked exhausted and nothing is recorded;
// the caller owns the commit so the marker is durable. ErrBudgetNotReserved
// reports a task without a reservation. The returned consumption is the
// ledger row's counter state (before any new usage on the error paths).
func (s *Store) settleUsageInTx(ctx context.Context, tx pgx.Tx, tenantID string, taskID uuid.UUID, idempotencyKey string, usage kernelstore.TaskBudget) (kernelstore.TaskBudgetStatus, kernelstore.TaskBudget, error) {
	status, err := scanBudgetStatus(tx.QueryRow(ctx, `SELECT `+budgetStatusColumns+`
		FROM task_budget_ledgers WHERE tenant_id = $1 AND task_id = $2 FOR UPDATE`,
		tenantID, taskID.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return status, kernelstore.TaskBudget{}, kernelstore.ErrBudgetNotReserved
	}
	if err != nil {
		return status, kernelstore.TaskBudget{}, classify(err)
	}
	consumed := status.Consumed

	var existing string
	err = tx.QueryRow(ctx, `SELECT id::text FROM task_budget_settlements
		WHERE tenant_id = $1 AND task_id = $2 AND idempotency_key = $3`,
		tenantID, taskID.String(), idempotencyKey).Scan(&existing)
	if err == nil {
		// Idempotent replay: the settlement is already recorded; the caller
		// sees the current cumulative consumption.
		return status, consumed, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return status, consumed, classify(err)
	}

	// Hard stop: once a settlement was rejected as over-budget, no further
	// consumption may be recorded even if the numbers would now fit.
	if status.Exhausted {
		return status, consumed, fmt.Errorf("%w: task=%s ledger is exhausted", kernelstore.ErrBudgetExceeded, taskID)
	}
	if exceeded(status.Reserved, consumed, usage) {
		updated, updateErr := scanBudgetStatus(tx.QueryRow(ctx, `UPDATE task_budget_ledgers
			SET exhausted = true, updated_at = $1, resource_version = resource_version + 1
			WHERE tenant_id = $2 AND task_id = $3 RETURNING `+budgetStatusColumns,
			s.now(), tenantID, taskID.String()))
		if updateErr != nil {
			return status, consumed, classify(updateErr)
		}
		return updated, consumed, fmt.Errorf("%w: task=%s reserved=%+v consumed=%+v additional=%+v",
			kernelstore.ErrBudgetExceeded, taskID, status.Reserved, consumed, usage)
	}

	now := s.now()
	if _, err := tx.Exec(ctx, `INSERT INTO task_budget_settlements (
		id, tenant_id, task_id, idempotency_key, tokens, cost_usd, tool_calls, wall_seconds, occurred_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	ON CONFLICT (tenant_id, task_id, idempotency_key) DO NOTHING`,
		s.newID().String(), tenantID, taskID.String(), idempotencyKey,
		usage.Tokens, usage.CostUSD, usage.ToolCalls, usage.WallSeconds, now); err != nil {
		return status, consumed, classify(err)
	}
	// Keep the rolling counters exact with the append, under the same row
	// lock that serializes every settlement for this task.
	updated, err := scanBudgetStatus(tx.QueryRow(ctx, `UPDATE task_budget_ledgers
		SET consumed_tokens = consumed_tokens + $1, consumed_cost_usd = consumed_cost_usd + $2,
			consumed_tool_calls = consumed_tool_calls + $3, consumed_wall_seconds = consumed_wall_seconds + $4,
			updated_at = $5, resource_version = resource_version + 1
		WHERE tenant_id = $6 AND task_id = $7 RETURNING `+budgetStatusColumns,
		usage.Tokens, usage.CostUSD, usage.ToolCalls, usage.WallSeconds, now, tenantID, taskID.String()))
	if err != nil {
		return status, consumed, classify(err)
	}
	return updated, updated.Consumed, nil
}

// SettleTaskUsageDelta settles the remainder between a usage family (all
// settlements whose key is FamilyPrefix + ":<suffix>") and a cumulative
// Target, under the ledger row lock so concurrent settlements cannot race the
// delta computation. A family that already settled at least the target
// settles nothing; a task without a reservation is not gated.
func (s *Store) SettleTaskUsageDelta(ctx context.Context, in kernelstore.SettleTaskUsageDeltaInput) (kernelstore.TaskBudgetStatus, error) {
	var zero kernelstore.TaskBudgetStatus
	if !in.Valid() {
		return zero, fmt.Errorf("valid tenant, task, family prefix, idempotency key, and target are required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return zero, err
	}
	defer rollback(ctx, tx)

	// Take the ledger row lock before reading the family sum: every
	// settlement for this task serializes on the same row, so the delta is
	// computed against a stable snapshot.
	status, err := scanBudgetStatus(tx.QueryRow(ctx, `SELECT `+budgetStatusColumns+`
		FROM task_budget_ledgers WHERE tenant_id = $1 AND task_id = $2 FOR UPDATE`,
		in.TenantID, in.TaskID.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, kernelstore.ErrBudgetNotReserved
	}
	if err != nil {
		return zero, classify(err)
	}

	familySettled, err := s.sumFamilySettlements(ctx, tx, in.TenantID, in.TaskID, in.FamilyPrefix)
	if err != nil {
		return zero, err
	}
	delta := taskBudgetRemainder(in.Target, familySettled)
	if delta.Zero() {
		// The family already reached (or overshot) the target: nothing to
		// settle, including a retried Finish after a crash between the delta
		// settlement and the call row update.
		if err := tx.Commit(ctx); err != nil {
			return zero, classify(err)
		}
		return status, nil
	}
	updated, consumed, err := s.settleUsageInTx(ctx, tx, in.TenantID, in.TaskID, in.IdempotencyKey, delta)
	if err == nil || errors.Is(err, kernelstore.ErrBudgetExceeded) {
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return zero, classify(commitErr)
		}
		updated.Consumed = consumed
		return updated, err
	}
	return zero, err
}

// sumFamilySettlements sums the usage a family already settled under the
// caller's transaction. The prefix is validated to contain no wildcards, so
// the pattern matches exactly FamilyPrefix + ":<suffix>".
func (s *Store) sumFamilySettlements(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, tenantID string, taskID uuid.UUID, familyPrefix string) (kernelstore.TaskBudget, error) {
	var settled kernelstore.TaskBudget
	err := q.QueryRow(ctx, `SELECT COALESCE(SUM(tokens), 0), COALESCE(SUM(cost_usd), 0),
		COALESCE(SUM(tool_calls), 0), COALESCE(SUM(wall_seconds), 0)
		FROM task_budget_settlements
		WHERE tenant_id = $1 AND task_id = $2 AND idempotency_key LIKE $3`,
		tenantID, taskID.String(), familyPrefix+":%").Scan(
		&settled.Tokens, &settled.CostUSD, &settled.ToolCalls, &settled.WallSeconds)
	if err != nil {
		return settled, classify(err)
	}
	return settled, nil
}

// taskBudgetRemainder returns the usage still owed to reach target, per
// dimension, never negative: a family that overshot settles nothing.
func taskBudgetRemainder(target, settled kernelstore.TaskBudget) kernelstore.TaskBudget {
	return kernelstore.TaskBudget{
		Tokens:      max64(0, target.Tokens-settled.Tokens),
		CostUSD:     maxFloat(0, target.CostUSD-settled.CostUSD),
		ToolCalls:   max64(0, target.ToolCalls-settled.ToolCalls),
		WallSeconds: max64(0, target.WallSeconds-settled.WallSeconds),
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// exceeded reports whether additional usage would push the task past its
// reserved ceiling on any dimension.
func exceeded(reserved, consumed, additional kernelstore.TaskBudget) bool {
	return (reserved.Tokens > 0 && consumed.Tokens+additional.Tokens > reserved.Tokens) ||
		(reserved.CostUSD > 0 && consumed.CostUSD+additional.CostUSD > reserved.CostUSD) ||
		(reserved.ToolCalls > 0 && consumed.ToolCalls+additional.ToolCalls > reserved.ToolCalls) ||
		(reserved.WallSeconds > 0 && consumed.WallSeconds+additional.WallSeconds > reserved.WallSeconds)
}

const budgetStatusColumns = `
	tenant_id, task_id::text, reserved_tokens, reserved_cost_usd,
	reserved_tool_calls, reserved_wall_seconds, exhausted, resource_version, updated_at,
	consumed_tokens, consumed_cost_usd, consumed_tool_calls, consumed_wall_seconds`

func scanBudgetStatus(row scanner) (kernelstore.TaskBudgetStatus, error) {
	var status kernelstore.TaskBudgetStatus
	var taskID string
	if err := row.Scan(&status.TenantID, &taskID, &status.Reserved.Tokens, &status.Reserved.CostUSD,
		&status.Reserved.ToolCalls, &status.Reserved.WallSeconds, &status.Exhausted,
		&status.ResourceVersion, &status.UpdatedAt,
		&status.Consumed.Tokens, &status.Consumed.CostUSD,
		&status.Consumed.ToolCalls, &status.Consumed.WallSeconds); err != nil {
		return status, err
	}
	parsed, err := uuid.Parse(taskID)
	if err != nil {
		return status, fmt.Errorf("parse task id: %w", err)
	}
	status.TaskID = parsed
	return status, nil
}
