package effect

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Service coordinates external effect execution, fencing, deduplication, and durable receipts.
type Service struct {
	store     Store
	fencer    Fencer
	metrics   Metrics
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewService constructs an Effect Service.
func NewService(store Store, fencer Fencer, metrics Metrics) *Service {
	if metrics == nil {
		metrics = &NoopMetrics{}
	}
	return &Service{
		store:     store,
		fencer:    fencer,
		metrics:   metrics,
		providers: make(map[string]Provider),
	}
}

// RegisterProvider registers an external effect provider.
func (s *Service) RegisterProvider(provider Provider) error {
	if provider == nil {
		return fmt.Errorf("provider cannot be nil")
	}
	name := provider.Name()
	if name == "" {
		return fmt.Errorf("provider name cannot be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.providers[name] = provider
	return nil
}

func (s *Service) getProvider(name string) (Provider, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.providers[name]
	return p, ok
}

// ExecuteEffect executes an external side effect with strict fencing, idempotency, and receipt recording.
func (s *Service) ExecuteEffect(ctx context.Context, req *EffectRequest) (*EffectReceipt, error) {
	if req == nil {
		return nil, ErrInvalidRequest
	}

	if err := req.Validate(); err != nil {
		return nil, err
	}

	// 1. Check deadline if specified
	if req.Deadline != nil && time.Now().UTC().After(*req.Deadline) {
		return nil, ErrEffectExpired
	}

	// 2. Fencing Token Validation
	// Stale attempts (lower token than active lease/fencing manager) are unconditionally rejected.
	if s.fencer != nil {
		if err := s.fencer.ValidateFencingToken(ctx, req.TenantID, req.AgentID, req.RunID, req.AttemptID, req.FencingToken); err != nil {
			s.metrics.RecordFenced(ctx, req.TenantID, req.Provider)
			return nil, err
		}
	}

	// 3. Idempotency Check
	existing, err := s.store.GetByIdempotencyKey(ctx, req.TenantID, req.AgentID, req.IdempotencyKey)
	if err == nil && existing != nil {
		// Verify payload hash consistency
		if existing.Request.PayloadHash != req.PayloadHash {
			return nil, fmt.Errorf("%w: existing hash %s != requested %s",
				ErrPayloadMismatch, existing.Request.PayloadHash, req.PayloadHash)
		}

		switch existing.Status {
		case EffectStatusCommitted:
			// Idempotent hit: return cached receipt without re-invoking the provider (logical effect = 1).
			s.metrics.RecordIdempotentHit(ctx, req.TenantID, req.Provider, req.Operation)
			return existing.Receipt, nil

		case EffectStatusUnknown:
			// CRITICAL: UNKNOWN outcome cannot be auto-replayed under any circumstances!
			s.metrics.RecordUnknown(ctx, req.TenantID, req.Provider, req.Operation)
			return existing.Receipt, fmt.Errorf("%w: previous attempt %s status is unknown; automatic replay forbidden",
				ErrEffectUnknown, existing.Request.AttemptID)

		case EffectStatusExecuting:
			// In-flight conflict
			return existing.Receipt, fmt.Errorf("%w: previous attempt %s is still executing",
				ErrEffectExecuting, existing.Request.AttemptID)

		case EffectStatusFailed:
			errMsg := "previous attempt failed"
			if existing.Receipt != nil && existing.Receipt.ErrorMessage != "" {
				errMsg = existing.Receipt.ErrorMessage
			}
			return existing.Receipt, fmt.Errorf("%w: %s", ErrProviderFailed, errMsg)

		default:
			// Status pending or unhandled
			return existing.Receipt, fmt.Errorf("%w: existing effect in %s status", ErrEffectExecuting, existing.Status)
		}
	} else if err != nil && !errors.Is(err, ErrEffectNotFound) {
		return nil, err
	}

	// 4. Initial Creation: persist record in PENDING status
	record := &EffectRecord{
		Request:   *req,
		Status:    EffectStatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	if err := s.store.CreateRecord(ctx, record); err != nil {
		if errors.Is(err, ErrEffectExecuting) {
			// Concurrent creation race: re-invoke ExecuteEffect to safely resolve against created record
			return s.ExecuteEffect(ctx, req)
		}
		return nil, err
	}

	// 5. Transition to EXECUTING
	if _, err := s.store.TransitionStatus(ctx, req.TenantID, req.ID, EffectStatusPending, EffectStatusExecuting, nil); err != nil {
		return nil, err
	}

	s.metrics.RecordExecuted(ctx, req.TenantID, req.Provider, req.Operation)

	// 6. Resolve Provider
	provider, ok := s.getProvider(req.Provider)
	if !ok {
		failedReceipt := &EffectReceipt{
			EffectID:     req.ID,
			TenantID:     req.TenantID,
			Status:       EffectStatusFailed,
			StartedAt:    time.Now().UTC(),
			CompletedAt:  time.Now().UTC(),
			AttemptID:    req.AttemptID,
			FencingToken: req.FencingToken,
			ErrorMessage: fmt.Sprintf("provider %q not found", req.Provider),
		}
		s.store.TransitionStatus(ctx, req.TenantID, req.ID, EffectStatusExecuting, EffectStatusFailed, failedReceipt)
		s.metrics.RecordFailed(ctx, req.TenantID, req.Provider, req.Operation, "provider_not_found")
		return failedReceipt, fmt.Errorf("%w: %s", ErrProviderNotFound, req.Provider)
	}

	// 7. Execute Provider
	startTime := time.Now().UTC()
	providerReceipt, execErr := provider.Execute(ctx, req)
	completedTime := time.Now().UTC()

	if execErr == nil {
		// COMMITTED: successfully acknowledged
		committedReceipt := &EffectReceipt{
			EffectID:        req.ID,
			TenantID:        req.TenantID,
			Status:          EffectStatusCommitted,
			ProviderReceipt: providerReceipt,
			StartedAt:       startTime,
			CompletedAt:     completedTime,
			AttemptID:       req.AttemptID,
			FencingToken:    req.FencingToken,
			ResultHash:      ComputePayloadHash(providerReceipt),
		}

		if _, err := s.store.TransitionStatus(ctx, req.TenantID, req.ID, EffectStatusExecuting, EffectStatusCommitted, committedReceipt); err != nil {
			return committedReceipt, err
		}
		s.metrics.RecordCommitted(ctx, req.TenantID, req.Provider, req.Operation)
		return committedReceipt, nil
	}

	// Handle execution failure
	if IsAmbiguous(execErr) {
		// UNKNOWN: outcome cannot be determined definitively (timeout / connection reset / etc.)
		unknownReceipt := &EffectReceipt{
			EffectID:     req.ID,
			TenantID:     req.TenantID,
			Status:       EffectStatusUnknown,
			StartedAt:    startTime,
			CompletedAt:  completedTime,
			AttemptID:    req.AttemptID,
			FencingToken: req.FencingToken,
			ErrorMessage: fmt.Sprintf("ambiguous provider execution: %v", execErr),
		}

		s.store.TransitionStatus(ctx, req.TenantID, req.ID, EffectStatusExecuting, EffectStatusUnknown, unknownReceipt)
		s.metrics.RecordUnknown(ctx, req.TenantID, req.Provider, req.Operation)
		return unknownReceipt, fmt.Errorf("%w: %v", ErrEffectUnknown, execErr)
	}

	// FAILED: definitive failure
	failedReceipt := &EffectReceipt{
		EffectID:     req.ID,
		TenantID:     req.TenantID,
		Status:       EffectStatusFailed,
		StartedAt:    startTime,
		CompletedAt:  completedTime,
		AttemptID:    req.AttemptID,
		FencingToken: req.FencingToken,
		ErrorMessage: execErr.Error(),
	}

	s.store.TransitionStatus(ctx, req.TenantID, req.ID, EffectStatusExecuting, EffectStatusFailed, failedReceipt)
	s.metrics.RecordFailed(ctx, req.TenantID, req.Provider, req.Operation, execErr.Error())
	return failedReceipt, fmt.Errorf("%w: %v", ErrProviderFailed, execErr)
}

// GetEffect retrieves an effect record by tenant and effect ID.
func (s *Service) GetEffect(ctx context.Context, tenantID, effectID string) (*EffectRecord, error) {
	return s.store.GetEffect(ctx, tenantID, effectID)
}

// GetEffectByIdempotencyKey retrieves an effect record by idempotency key.
func (s *Service) GetEffectByIdempotencyKey(ctx context.Context, tenantID, agentID, idempotencyKey string) (*EffectRecord, error) {
	return s.store.GetByIdempotencyKey(ctx, tenantID, agentID, idempotencyKey)
}

// ResolveUnknownEffect allows manual or administrative resolution of an effect in UNKNOWN status.
// This is used when external verification (e.g. contacting the external payment/webhook provider)
// has determined the actual outcome.
func (s *Service) ResolveUnknownEffect(ctx context.Context, tenantID, effectID string, forceStatus EffectStatus, providerReceipt []byte, reason string) (*EffectReceipt, error) {
	record, err := s.store.GetEffect(ctx, tenantID, effectID)
	if err != nil {
		return nil, err
	}

	if record.Status != EffectStatusUnknown {
		return nil, fmt.Errorf("cannot force-resolve effect in status %s (only UNKNOWN effects can be resolved)", record.Status)
	}

	if forceStatus != EffectStatusCommitted && forceStatus != EffectStatusFailed {
		return nil, fmt.Errorf("forceStatus must be COMMITTED or FAILED, got %s", forceStatus)
	}

	receipt := &EffectReceipt{
		EffectID:        effectID,
		TenantID:        tenantID,
		Status:          forceStatus,
		ProviderReceipt: providerReceipt,
		StartedAt:       record.CreatedAt,
		CompletedAt:     time.Now().UTC(),
		AttemptID:       record.Request.AttemptID,
		FencingToken:    record.Request.FencingToken,
		ResultHash:      ComputePayloadHash(providerReceipt),
		ErrorMessage:    reason,
	}

	if _, err := s.store.ForceStatus(ctx, tenantID, effectID, forceStatus, receipt); err != nil {
		return nil, err
	}

	return receipt, nil
}
