package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrTenantQuotaExceeded reports an admission that would push a tenant's
// windowed aggregate consumption past its configured limits. The admission is
// rejected and nothing is recorded; the task stays queued and is rejected
// with a recorded TENANT_QUOTA_EXCEEDED decision on the next claim round.
var ErrTenantQuotaExceeded = errors.New("tenant aggregate consumption quota would be exceeded")

// TenantQuota is a tenant's windowed aggregate consumption limits. A zero
// dimension means that dimension is unlimited, matching task budget
// semantics; a tenant without a quota row is never gated.
type TenantQuota struct {
	TenantID        string
	WindowSeconds   int64
	Limits          TaskBudget
	ResourceVersion int64
	UpdatedAt       time.Time
}

// SetTenantQuotaInput configures a tenant's aggregate consumption quota.
// WindowSeconds must be at least 60; limits must be non-negative.
type SetTenantQuotaInput struct {
	TenantID      string
	WindowSeconds int64
	Limits        TaskBudget
}

func (in SetTenantQuotaInput) Valid() bool {
	return strings.TrimSpace(in.TenantID) != "" && in.WindowSeconds >= 60 && in.Limits.Valid()
}

// TenantWindowUsage is the settled and reserved aggregate consumption of one
// tenant in one fixed window (v0.8: reservation holds the ceilings of
// admitted-but-not-terminal tasks).
type TenantWindowUsage struct {
	TenantID        string
	WindowStart     time.Time
	Consumed        TaskBudget
	Reserved        TaskBudget
	ResourceVersion int64
	UpdatedAt       time.Time
}

// QuotaExceeded reports whether additional usage would push a tenant's
// windowed consumption past any configured limit. A limit of 0 means the
// dimension is unlimited.
func QuotaExceeded(limits, consumed, additional TaskBudget) bool {
	return (limits.Tokens > 0 && consumed.Tokens+additional.Tokens > limits.Tokens) ||
		(limits.CostMicroUSD > 0 && consumed.CostMicroUSD+additional.CostMicroUSD > limits.CostMicroUSD) ||
		(limits.ToolCalls > 0 && consumed.ToolCalls+additional.ToolCalls > limits.ToolCalls) ||
		(limits.WallSeconds > 0 && consumed.WallSeconds+additional.WallSeconds > limits.WallSeconds)
}

// QuotaReservationExceeded is the v0.8 admission gate: additional usage is
// rejected when consumed plus the reserved ceilings of in-flight tasks plus
// the additional ceiling would exceed any limit. Reservation closes the
// concurrent-admission overshoot of the v0.6 consumed-only gate.
func QuotaReservationExceeded(limits, consumed, reserved, additional TaskBudget) bool {
	return (limits.Tokens > 0 && consumed.Tokens+reserved.Tokens+additional.Tokens > limits.Tokens) ||
		(limits.CostMicroUSD > 0 && consumed.CostMicroUSD+reserved.CostMicroUSD+additional.CostMicroUSD > limits.CostMicroUSD) ||
		(limits.ToolCalls > 0 && consumed.ToolCalls+reserved.ToolCalls+additional.ToolCalls > limits.ToolCalls) ||
		(limits.WallSeconds > 0 && consumed.WallSeconds+reserved.WallSeconds+additional.WallSeconds > limits.WallSeconds)
}

// TenantQuotaStore is the persistence contract for tenant aggregate
// consumption quotas. GetTenantQuota reports ErrNotFound for a tenant without
// a configured quota; GetTenantQuotaUsage reports the current window's
// consumption (zero usage when the tenant settled nothing in it yet) or
// ErrNotFound when no quota is configured.
type TenantQuotaStore interface {
	// SetTenantQuota configures or replaces the tenant's quota. Existing
	// window consumption is preserved; only the limits and window length
	// change.
	SetTenantQuota(context.Context, SetTenantQuotaInput) (TenantQuota, error)
	// GetTenantQuota returns the tenant's configured quota or ErrNotFound.
	GetTenantQuota(context.Context, string) (TenantQuota, error)
	// DeleteTenantQuota removes the tenant's quota configuration. Window
	// consumption rows become inert; future admissions are unlimited until a
	// new quota is configured.
	DeleteTenantQuota(context.Context, string) error
	// GetTenantQuotaUsage returns the consumption of the window containing at.
	GetTenantQuotaUsage(context.Context, string, time.Time) (TenantWindowUsage, error)
}
