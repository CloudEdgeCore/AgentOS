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
	status, err := scanBudgetStatus(s.pool.QueryRow(ctx, `SELECT `+budgetStatusColumns+`
		FROM task_budget_ledgers WHERE tenant_id = $1 AND task_id = $2`, tenantID, taskID.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return kernelstore.TaskBudgetStatus{}, kernelstore.ErrBudgetNotReserved
	}
	if err != nil {
		return kernelstore.TaskBudgetStatus{}, classify(err)
	}
	consumed, err := s.sumSettlements(ctx, s.pool, tenantID, taskID)
	if err != nil {
		return kernelstore.TaskBudgetStatus{}, err
	}
	status.Consumed = consumed
	return status, nil
}

func (s *Store) SettleTaskUsage(ctx context.Context, in kernelstore.SettleTaskUsageInput) (kernelstore.TaskBudgetStatus, error) {
	var zero kernelstore.TaskBudgetStatus
	if !in.Valid() {
		return zero, fmt.Errorf("valid tenant, task, idempotency key, and positive usage are required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return zero, err
	}
	defer rollback(ctx, tx)

	status, err := scanBudgetStatus(tx.QueryRow(ctx, `SELECT `+budgetStatusColumns+`
		FROM task_budget_ledgers WHERE tenant_id = $1 AND task_id = $2 FOR UPDATE`,
		in.TenantID, in.TaskID.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, kernelstore.ErrBudgetNotReserved
	}
	if err != nil {
		return zero, classify(err)
	}

	var existing string
	err = tx.QueryRow(ctx, `SELECT id::text FROM task_budget_settlements
		WHERE tenant_id = $1 AND task_id = $2 AND idempotency_key = $3`,
		in.TenantID, in.TaskID.String(), in.IdempotencyKey).Scan(&existing)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return zero, classify(err)
		}
		consumed, err := s.sumSettlements(ctx, s.pool, in.TenantID, in.TaskID)
		if err != nil {
			return zero, err
		}
		status.Consumed = consumed
		return status, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return zero, classify(err)
	}

	// Hard stop: once a settlement was rejected as over-budget, no further
	// consumption may be recorded even if the numbers would now fit.
	if status.Exhausted {
		if err := tx.Commit(ctx); err != nil {
			return zero, classify(err)
		}
		return status, fmt.Errorf("%w: task=%s ledger is exhausted", kernelstore.ErrBudgetExceeded, in.TaskID)
	}

	consumed, err := s.sumSettlements(ctx, tx, in.TenantID, in.TaskID)
	if err != nil {
		return zero, err
	}
	now := s.now()
	if exceeded(status.Reserved, consumed, in.Usage) {
		updated, err := scanBudgetStatus(tx.QueryRow(ctx, `UPDATE task_budget_ledgers
			SET exhausted = true, updated_at = $1, resource_version = resource_version + 1
			WHERE tenant_id = $2 AND task_id = $3 RETURNING `+budgetStatusColumns,
			now, in.TenantID, in.TaskID.String()))
		if err != nil {
			return zero, classify(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return zero, classify(err)
		}
		updated.Consumed = consumed
		return updated, fmt.Errorf("%w: task=%s reserved=%+v consumed=%+v additional=%+v",
			kernelstore.ErrBudgetExceeded, in.TaskID, status.Reserved, consumed, in.Usage)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO task_budget_settlements (
		id, tenant_id, task_id, idempotency_key, tokens, cost_usd, tool_calls, wall_seconds, occurred_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	ON CONFLICT (tenant_id, task_id, idempotency_key) DO NOTHING`,
		s.newID().String(), in.TenantID, in.TaskID.String(), in.IdempotencyKey,
		in.Usage.Tokens, in.Usage.CostUSD, in.Usage.ToolCalls, in.Usage.WallSeconds, now); err != nil {
		return zero, classify(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, classify(err)
	}
	status.Consumed = taskBudgetAdd(consumed, in.Usage)
	return status, nil
}

// sumSettlements reads cumulative consumption under the caller's transaction
// or directly from the pool.
func (s *Store) sumSettlements(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, tenantID string, taskID uuid.UUID) (kernelstore.TaskBudget, error) {
	var consumed kernelstore.TaskBudget
	err := q.QueryRow(ctx, `SELECT COALESCE(SUM(tokens), 0), COALESCE(SUM(cost_usd), 0),
		COALESCE(SUM(tool_calls), 0), COALESCE(SUM(wall_seconds), 0)
		FROM task_budget_settlements WHERE tenant_id = $1 AND task_id = $2`,
		tenantID, taskID.String()).Scan(&consumed.Tokens, &consumed.CostUSD, &consumed.ToolCalls, &consumed.WallSeconds)
	if err != nil {
		return consumed, classify(err)
	}
	return consumed, nil
}

func exceeded(reserved, consumed, additional kernelstore.TaskBudget) bool {
	return (reserved.Tokens > 0 && consumed.Tokens+additional.Tokens > reserved.Tokens) ||
		(reserved.CostUSD > 0 && consumed.CostUSD+additional.CostUSD > reserved.CostUSD) ||
		(reserved.ToolCalls > 0 && consumed.ToolCalls+additional.ToolCalls > reserved.ToolCalls) ||
		(reserved.WallSeconds > 0 && consumed.WallSeconds+additional.WallSeconds > reserved.WallSeconds)
}

func taskBudgetAdd(a, b kernelstore.TaskBudget) kernelstore.TaskBudget {
	return kernelstore.TaskBudget{
		Tokens: a.Tokens + b.Tokens, CostUSD: a.CostUSD + b.CostUSD,
		ToolCalls: a.ToolCalls + b.ToolCalls, WallSeconds: a.WallSeconds + b.WallSeconds,
	}
}

const budgetStatusColumns = `
	tenant_id, task_id::text, reserved_tokens, reserved_cost_usd,
	reserved_tool_calls, reserved_wall_seconds, exhausted, resource_version, updated_at`

func scanBudgetStatus(row scanner) (kernelstore.TaskBudgetStatus, error) {
	var status kernelstore.TaskBudgetStatus
	var taskID string
	if err := row.Scan(&status.TenantID, &taskID, &status.Reserved.Tokens, &status.Reserved.CostUSD,
		&status.Reserved.ToolCalls, &status.Reserved.WallSeconds, &status.Exhausted,
		&status.ResourceVersion, &status.UpdatedAt); err != nil {
		return status, err
	}
	parsed, err := uuid.Parse(taskID)
	if err != nil {
		return status, fmt.Errorf("parse task id: %w", err)
	}
	status.TaskID = parsed
	return status, nil
}
