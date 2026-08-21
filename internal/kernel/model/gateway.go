// Package model implements the Model Gateway decision chain of the Agent OS
// kernel: versioned model descriptors with price tables, policy checks,
// incremental token/cost settlement against the task budget ledger, and the
// hard stop when a reservation is exhausted (tech baseline §12.1; invariant
// 12). Prompt and completion content never enters kernel telemetry.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/policy"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
)

var (
	// ErrModelDenied reports a model reference outside the tenant's policy.
	ErrModelDenied = errors.New("model is not allowed by tenant policy")
	// ErrBudgetExhausted is the hard-stop outcome: the task budget is
	// exhausted and new consumption must stop (invariant 12).
	ErrBudgetExhausted = errors.New("task budget exhausted: new consumption stopped")
)

// PolicyChecker decides model calls outside the LLM. The engine is
// fail-closed: missing tenant data and evaluation errors deny by default.
type PolicyChecker interface {
	EvaluateModel(context.Context, string, policy.ModelContext) policy.Decision
}

// ModelStore is the model registry and call ledger. The postgres Store
// satisfies it.
type ModelStore interface {
	GetModelDescriptor(context.Context, string, string, string) (store.ModelDescriptor, error)
	CreateModelCall(context.Context, store.CreateModelCallInput) (store.CreateModelCallResult, error)
	GetModelCall(context.Context, string, uuid.UUID) (store.ModelCall, error)
	FinishModelCall(context.Context, store.FinishModelCallInput) (store.ModelCall, error)
}

// BeginInput starts one fenced model invocation.
type BeginInput struct {
	TenantID        string
	TaskID          uuid.UUID
	RunID           uuid.UUID
	AttemptID       uuid.UUID
	FencingToken    int64
	AgentVersionRef string
	// ModelRef is the canonical provider/model reference.
	ModelRef       string
	IdempotencyKey string
}

func (in BeginInput) validate() error {
	if strings.TrimSpace(in.TenantID) == "" || in.TaskID == uuid.Nil || in.RunID == uuid.Nil ||
		in.AttemptID == uuid.Nil || strings.TrimSpace(in.AgentVersionRef) == "" ||
		strings.TrimSpace(in.IdempotencyKey) == "" {
		return fmt.Errorf("tenant, task, run, attempt, agent version, and idempotency key are required")
	}
	return store.ValidateModelRef(in.ModelRef)
}

// BeginResult is the outcome of starting an invocation.
type BeginResult struct {
	Call           store.ModelCall
	Descriptor     store.ModelDescriptor
	PolicyRevision string
	DenyReasons    []string
}

// Usage is one settlement step of a streaming call.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

func (u Usage) valid() bool { return u.InputTokens >= 0 && u.OutputTokens >= 0 }

// Gateway orchestrates the model call lifecycle: Begin (policy + budget
// preflight + ledger row), Settle (idempotent incremental settlement with the
// ledger enforcing the hard stop), Finish (final settlement, terminal state
// and durable audit receipt). Every settlement carries its own idempotency
// key derived from the call and sequence, so retries never double-charge.
type Gateway struct {
	policy   PolicyChecker
	models   ModelStore
	budget   store.BudgetStore
	receipts ReceiptStore
	now      func() time.Time
	newID    func() uuid.UUID
}

// ReceiptStore persists the model call outcome for audit. Content is never
// stored — only metadata.
type ReceiptStore interface {
	WriteRuntimeReceipt(context.Context, store.WriteRuntimeReceiptInput) error
}

func NewGateway(policyChecker PolicyChecker, models ModelStore, budget store.BudgetStore, receipts ReceiptStore) *Gateway {
	return &Gateway{
		policy: policyChecker, models: models, budget: budget, receipts: receipts,
		now: time.Now, newID: func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New()
			}
			return id
		},
	}
}

// GetModelCall reads one ledger row for fenced transports.
func (g *Gateway) GetModelCall(ctx context.Context, tenantID string, callID uuid.UUID) (store.ModelCall, error) {
	return g.models.GetModelCall(ctx, tenantID, callID)
}

// Begin validates policy and budget headroom and opens the call ledger row.
func (g *Gateway) Begin(ctx context.Context, in BeginInput) (BeginResult, error) {
	var result BeginResult
	if err := in.validate(); err != nil {
		return result, err
	}
	provider, modelName, ok := strings.Cut(in.ModelRef, "/")
	if !ok {
		return result, fmt.Errorf("model reference must be provider/model")
	}
	descriptor, err := g.models.GetModelDescriptor(ctx, in.TenantID, provider, modelName)
	if err != nil {
		return result, fmt.Errorf("resolve model %s: %w", in.ModelRef, err)
	}
	decision := g.policy.EvaluateModel(ctx, in.TenantID, policy.ModelContext{Name: descriptor.Ref()})
	if !decision.Allow {
		return result, fmt.Errorf("%w: %v", ErrModelDenied, decision.DenyReasons)
	}
	result.Descriptor, result.PolicyRevision = descriptor, policy.Revision
	result.DenyReasons = decision.DenyReasons

	if err := g.enforceBudgetHeadroom(ctx, in.TenantID, in.TaskID); err != nil {
		return result, err
	}
	created, err := g.models.CreateModelCall(ctx, store.CreateModelCallInput{
		ID: g.newID(), TenantID: in.TenantID, TaskID: in.TaskID, RunID: in.RunID,
		AttemptID: in.AttemptID, ModelRef: descriptor.Ref(), PriceRevision: descriptor.PriceRevision,
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return result, err
	}
	result.Call = created.ModelCall
	return result, nil
}

// Settle appends one idempotent usage step to the ledger. A settlement that
// would exceed the reservation is rejected and the ledger is marked
// exhausted: the caller must stop the stream within bounded time. A task
// without a reservation carries no ceiling and is not gated (mirroring the
// Tool Gateway).
func (g *Gateway) Settle(ctx context.Context, call store.ModelCall, sequence int64, usage Usage) error {
	if !usage.valid() {
		return fmt.Errorf("usage must not be negative")
	}
	if sequence < 1 {
		return fmt.Errorf("settlement sequence must be positive")
	}
	_, err := g.budget.SettleTaskUsage(ctx, store.SettleTaskUsageInput{
		TenantID: call.TenantID, TaskID: call.TaskID,
		IdempotencyKey: fmt.Sprintf("model:%s:%d", call.ID, sequence),
		Usage:          store.TaskBudget{Tokens: usage.InputTokens + usage.OutputTokens},
	})
	if err != nil {
		if errors.Is(err, store.ErrBudgetNotReserved) {
			return nil
		}
		if errors.Is(err, store.ErrBudgetExceeded) {
			return ErrBudgetExhausted
		}
		return fmt.Errorf("settle model usage: %w", err)
	}
	return nil
}

// FinishInput finalizes a model invocation. Usage is reported by the caller;
// cost is computed by the gateway from the descriptor's price table (pinned
// price revision), never trusted from the caller.
type FinishInput struct {
	TenantID          string
	ModelCallID       uuid.UUID
	ExpectedVersion   int64
	Status            store.ModelCallStatus
	InputTokens       int64
	OutputTokens      int64
	ProviderRequestID string
	FinishReason      string
}

// Finish finalizes the invocation: the final usage is settled idempotently,
// cost is computed from the pinned price table, the ledger row reaches its
// terminal state, and an audit receipt records the metadata (never content).
func (g *Gateway) Finish(ctx context.Context, call store.ModelCall, in FinishInput) (store.ModelCall, error) {
	if in.ModelCallID != call.ID || in.TenantID != call.TenantID {
		return store.ModelCall{}, fmt.Errorf("finish input does not match the call")
	}
	if !in.Status.Terminal() {
		return store.ModelCall{}, fmt.Errorf("finish status must be terminal")
	}
	if in.InputTokens < 0 || in.OutputTokens < 0 {
		return store.ModelCall{}, fmt.Errorf("usage must not be negative")
	}
	provider, modelName, ok := strings.Cut(call.ModelRef, "/")
	if !ok {
		return store.ModelCall{}, fmt.Errorf("call model reference is malformed")
	}
	descriptor, err := g.models.GetModelDescriptor(ctx, call.TenantID, provider, modelName)
	if err != nil {
		return store.ModelCall{}, fmt.Errorf("resolve model %s: %w", call.ModelRef, err)
	}
	status := in.Status
	total := in.InputTokens + in.OutputTokens
	if total > 0 {
		// Exact settlement: the final usage is the cumulative target of this
		// call's settlement family (model:<callID>:*), so the gateway charges
		// only the remainder after the per-step Settle records — a stream
		// that settled steps and finalizes with the cumulative total is
		// charged exactly once, and a retried Finish after a crash converges.
		if _, err := g.budget.SettleTaskUsageDelta(ctx, store.SettleTaskUsageDeltaInput{
			TenantID: call.TenantID, TaskID: call.TaskID,
			FamilyPrefix: "model:" + call.ID.String(), Target: store.TaskBudget{Tokens: total},
			IdempotencyKey: fmt.Sprintf("model:%s:finish", call.ID),
		}); err != nil {
			if errors.Is(err, store.ErrBudgetNotReserved) {
				// No reservation: no ceiling, nothing to meter.
			} else if errors.Is(err, store.ErrBudgetExceeded) {
				status = store.ModelCallStopped
			} else {
				return store.ModelCall{}, fmt.Errorf("settle final model usage: %w", err)
			}
		}
	}
	cost := costUSD(descriptor, in.InputTokens, in.OutputTokens)
	updated, err := g.models.FinishModelCall(ctx, store.FinishModelCallInput{
		TenantID: call.TenantID, ModelCallID: call.ID, ExpectedVersion: in.ExpectedVersion,
		Status: status, InputTokens: in.InputTokens, OutputTokens: in.OutputTokens,
		CostUSD: cost, PriceRevision: descriptor.PriceRevision,
		ProviderRequestID: in.ProviderRequestID, FinishReason: in.FinishReason,
	})
	if err != nil {
		return store.ModelCall{}, err
	}
	receipt, err := json.Marshal(map[string]any{
		"modelRef": updated.ModelRef, "status": updated.Status,
		"inputTokens": updated.InputTokens, "outputTokens": updated.OutputTokens,
		"costUsd": updated.CostUSD, "priceRevision": updated.PriceRevision,
		"finishReason": updated.FinishReason, "providerRequestId": updated.ProviderRequestID,
	})
	if err != nil {
		return store.ModelCall{}, err
	}
	if err := g.receipts.WriteRuntimeReceipt(ctx, store.WriteRuntimeReceiptInput{
		TenantID: call.TenantID, AttemptID: call.AttemptID,
		Operation: "MODEL:" + updated.ModelRef, IdempotencyKey: fmt.Sprintf("model:%s:finish", call.ID),
		RequestHash: call.RequestHash, Response: receipt,
	}); err != nil {
		return store.ModelCall{}, fmt.Errorf("write model receipt: %w", err)
	}
	if status == store.ModelCallStopped {
		return updated, ErrBudgetExhausted
	}
	return updated, nil
}

// costUSD computes the dollar cost of a call against the descriptor's price
// table (per million tokens).
func costUSD(descriptor store.ModelDescriptor, inputTokens, outputTokens int64) float64 {
	inputCost := float64(inputTokens) / 1_000_000 * descriptor.InputPricePerMillion
	outputCost := float64(outputTokens) / 1_000_000 * descriptor.OutputPricePerMillion
	return inputCost + outputCost
}

// enforceBudgetHeadroom applies the hard-stop invariant before any stream
// starts: an exhausted ledger must not open new consumption.
func (g *Gateway) enforceBudgetHeadroom(ctx context.Context, tenantID string, taskID uuid.UUID) error {
	status, err := g.budget.GetTaskBudget(ctx, tenantID, taskID)
	if err != nil {
		if errors.Is(err, store.ErrBudgetNotReserved) {
			return nil
		}
		return fmt.Errorf("read task budget: %w", err)
	}
	if status.Exhausted {
		return ErrBudgetExhausted
	}
	return nil
}
