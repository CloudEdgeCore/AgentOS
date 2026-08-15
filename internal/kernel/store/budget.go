package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrBudgetExceeded reports a usage settlement that would push a task past
	// its reserved ceiling. The settlement is rejected and the ledger is
	// marked exhausted: new consumption must stop.
	ErrBudgetExceeded = errors.New("task budget would be exceeded")
	// ErrBudgetNotReserved reports a settlement or read for a task without a
	// budget reservation.
	ErrBudgetNotReserved = errors.New("task has no budget reservation")
)

// TaskBudget is the reserved or consumed four-dimensional budget of a task.
// All fields are non-negative; a zero budget means the dimension is unlimited.
type TaskBudget struct {
	Tokens      int64
	CostUSD     float64
	ToolCalls   int64
	WallSeconds int64
}

func (b TaskBudget) Zero() bool {
	return b.Tokens == 0 && b.CostUSD == 0 && b.ToolCalls == 0 && b.WallSeconds == 0
}

func (b TaskBudget) Valid() bool {
	return b.Tokens >= 0 && b.CostUSD >= 0 && b.ToolCalls >= 0 && b.WallSeconds >= 0
}

// TaskBudgetStatus is the current reservation and cumulative consumption of a
// task, as derived from the append-only settlement ledger.
type TaskBudgetStatus struct {
	TaskID          uuid.UUID
	TenantID        string
	Reserved        TaskBudget
	Consumed        TaskBudget
	Exhausted       bool
	ResourceVersion int64
	UpdatedAt       time.Time
}

type SettleTaskUsageInput struct {
	TenantID       string
	TaskID         uuid.UUID
	IdempotencyKey string
	Usage          TaskBudget
}

func (in SettleTaskUsageInput) Valid() bool {
	return strings.TrimSpace(in.TenantID) != "" && in.TaskID != uuid.Nil &&
		strings.TrimSpace(in.IdempotencyKey) != "" && in.Usage.Valid() && !in.Usage.Zero()
}

type BudgetStore interface {
	// GetTaskBudget returns the reservation and cumulative consumption of a
	// task, or ErrBudgetNotReserved when the task carries no budget.
	GetTaskBudget(context.Context, string, uuid.UUID) (TaskBudgetStatus, error)
	// SettleTaskUsage appends one idempotent usage settlement. A settlement
	// that would exceed the reservation is rejected with ErrBudgetExceeded and
	// marks the ledger exhausted without being recorded. Replays of an
	// already-recorded settlement return the current status.
	SettleTaskUsage(context.Context, SettleTaskUsageInput) (TaskBudgetStatus, error)
}
