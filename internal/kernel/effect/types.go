package effect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// EffectStatus represents the lifecycle state of an external side effect.
type EffectStatus string

const (
	// EffectStatusPending indicates the effect request has been registered but execution has not begun.
	EffectStatusPending EffectStatus = "PENDING"

	// EffectStatusExecuting indicates the effect is currently being dispatched to the external provider.
	EffectStatusExecuting EffectStatus = "EXECUTING"

	// EffectStatusCommitted indicates the effect was successfully executed and acknowledged by the external provider.
	// It is terminal and safe for idempotent replay (logical effect = 1).
	EffectStatusCommitted EffectStatus = "COMMITTED"

	// EffectStatusFailed indicates the external provider definitively rejected or failed the request.
	EffectStatusFailed EffectStatus = "FAILED"

	// EffectStatusUnknown indicates the request may or may not have reached or completed at the external provider
	// (e.g. timeout, connection reset, ambiguous ack).
	// CRITICAL: UNKNOWN must NEVER be automatically replayed or retried blindly.
	EffectStatusUnknown EffectStatus = "UNKNOWN"
)

// IsTerminal returns true if the status is a completed terminal state.
func (s EffectStatus) IsTerminal() bool {
	switch s {
	case EffectStatusCommitted, EffectStatusFailed, EffectStatusUnknown:
		return true
	default:
		return false
	}
}

// EffectRequest defines a kernel-level request to produce an external side effect.
type EffectRequest struct {
	ID             string     `json:"effect_id"`
	TenantID       string     `json:"tenant_id"`
	AgentID        string     `json:"agent_id"`
	RunID          string     `json:"run_id,omitempty"`
	AttemptID      string     `json:"attempt_id,omitempty"`
	FencingToken   int64      `json:"fencing_token"`
	Provider       string     `json:"provider"`
	Operation      string     `json:"operation"`
	IdempotencyKey string     `json:"idempotency_key"`
	PayloadHash    string     `json:"payload_hash"`
	Payload        []byte     `json:"payload,omitempty"`
	Deadline       *time.Time `json:"deadline,omitempty"`
	TraceID        string     `json:"trace_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Validate checks essential constraints on the EffectRequest and populates ID and PayloadHash if missing.
func (r *EffectRequest) Validate() error {
	if strings.TrimSpace(r.TenantID) == "" {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(r.AgentID) == "" {
		return fmt.Errorf("%w: agent_id is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(r.Provider) == "" {
		return fmt.Errorf("%w: provider is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(r.Operation) == "" {
		return fmt.Errorf("%w: operation is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return fmt.Errorf("%w: idempotency_key is required", ErrInvalidRequest)
	}
	if r.FencingToken < 0 {
		return fmt.Errorf("%w: fencing_token must be non-negative", ErrInvalidRequest)
	}
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}

	computedHash := ComputePayloadHash(r.Payload)
	if r.PayloadHash == "" {
		r.PayloadHash = computedHash
	} else if r.PayloadHash != computedHash && len(r.Payload) > 0 {
		return fmt.Errorf("%w: payload_hash %q does not match computed %q", ErrInvalidRequest, r.PayloadHash, computedHash)
	}
	return nil
}

// ComputePayloadHash calculates a standard SHA-256 hex digest of the payload bytes.
func ComputePayloadHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// EffectReceipt contains the durable execution outcome and proof of an external side effect.
type EffectReceipt struct {
	EffectID        string       `json:"effect_id"`
	TenantID        string       `json:"tenant_id"`
	Status          EffectStatus `json:"status"`
	ProviderReceipt []byte       `json:"provider_receipt,omitempty"`
	StartedAt       time.Time    `json:"started_at"`
	CompletedAt     time.Time    `json:"completed_at"`
	AttemptID       string       `json:"attempt_id,omitempty"`
	FencingToken    int64        `json:"fencing_token"`
	ResultHash      string       `json:"result_hash,omitempty"`
	ErrorMessage    string       `json:"error_message,omitempty"`
}

// EffectRecord represents the persistent entity stored in the EffectStore.
type EffectRecord struct {
	Request   EffectRequest  `json:"request"`
	Receipt   *EffectReceipt `json:"receipt,omitempty"`
	Status    EffectStatus   `json:"status"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Version   int64          `json:"version"`
}
