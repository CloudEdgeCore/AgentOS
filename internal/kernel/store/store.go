// Package store defines the transactional persistence contract used by kernel
// controllers. Infrastructure implementations must preserve these semantics.
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	"github.com/google/uuid"
)

var (
	ErrNotFound            = errors.New("kernel object not found")
	ErrVersionConflict     = errors.New("resource version conflict")
	ErrInvalidTransition   = errors.New("invalid lifecycle transition")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different request")
	ErrLeaseHeld           = errors.New("run already has an active lease")
	ErrFenced              = errors.New("attempt fencing token is stale")
	ErrCompletionPending   = errors.New("completed attempt is awaiting result commit")
	ErrResultRequired      = errors.New("durable result reference is required")
	// ErrRetryableTransaction marks a transient serialization failure that
	// callers must retry with bounded backoff (ADR-002: SERIALIZABLE with
	// bounded retries).
	ErrRetryableTransaction = errors.New("retryable transaction conflict")
)

// IsRetryableTransaction reports whether err is a transient transaction
// failure that may be retried with backoff.
func IsRetryableTransaction(err error) bool {
	return errors.Is(err, ErrRetryableTransaction)
}

type Task struct {
	ID                  uuid.UUID
	TenantID            string
	Namespace           string
	AgentVersionRef     string
	AgentVersionID      *uuid.UUID
	Goal                string
	Spec                json.RawMessage
	RequestHash         [32]byte
	IdempotencyKey      string
	Phase               domain.TaskPhase
	AdmissionReasonCode string
	AdmittedAt          *time.Time
	CancelRequestedAt   *time.Time
	ActiveRunID         *uuid.UUID
	ResultRef           string
	ResourceVersion     int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type Run struct {
	ID                  uuid.UUID
	TenantID            string
	TaskID              uuid.UUID
	Ordinal             int
	Phase               domain.RunPhase
	ActiveAttemptID     *uuid.UUID
	CurrentFencingToken int64
	ResultRef           string
	ResourceVersion     int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
	CompletedAt         *time.Time
}

type Attempt struct {
	ID                uuid.UUID
	TenantID          string
	RunID             uuid.UUID
	Ordinal           int
	Phase             domain.AttemptPhase
	RuntimeClass      string
	RuntimePoolID     string
	RuntimeInstanceID string
	FencingToken      int64
	FailureCode       string
	FailureMessage    string
	ResourceVersion   int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
}

type Lease struct {
	ID              uuid.UUID
	TenantID        string
	RunID           uuid.UUID
	AttemptID       uuid.UUID
	FencingToken    int64
	ResourceVersion int64
	AcquiredAt      time.Time
	HeartbeatAt     time.Time
	ExpiresAt       time.Time
}

type AttemptLease struct {
	Attempt Attempt
	Lease   Lease
	Run     Run
}

type CreateTaskInput struct {
	ID              uuid.UUID
	TenantID        string
	Namespace       string
	AgentVersionRef string
	Goal            string
	Spec            json.RawMessage
	IdempotencyKey  string
}

func (in CreateTaskInput) ValidateAndHash() (json.RawMessage, [32]byte, error) {
	var zero [32]byte
	if strings.TrimSpace(in.TenantID) == "" || strings.TrimSpace(in.Namespace) == "" {
		return nil, zero, fmt.Errorf("tenant and namespace are required")
	}
	if strings.TrimSpace(in.AgentVersionRef) == "" || strings.TrimSpace(in.Goal) == "" {
		return nil, zero, fmt.Errorf("agent version and goal are required")
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return nil, zero, fmt.Errorf("idempotency key is required")
	}

	decoder := json.NewDecoder(bytes.NewReader(in.Spec))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, zero, fmt.Errorf("decode task spec: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, zero, err
	}
	normalized, err := json.Marshal(document)
	if err != nil {
		return nil, zero, fmt.Errorf("normalize task spec: %w", err)
	}
	payload, err := json.Marshal(struct {
		AgentVersionRef string          `json:"agentVersionRef"`
		Goal            string          `json:"goal"`
		Spec            json.RawMessage `json:"spec"`
	}{in.AgentVersionRef, in.Goal, normalized})
	if err != nil {
		return nil, zero, fmt.Errorf("hash task request: %w", err)
	}
	return normalized, sha256.Sum256(payload), nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("task spec contains more than one JSON value")
	}
	return fmt.Errorf("decode trailing task spec data: %w", err)
}

type CreateTaskResult struct {
	Task     Task
	Existing bool
}

type CreateRunInput struct {
	ID                  uuid.UUID
	TaskID              uuid.UUID
	ExpectedTaskVersion int64
}

type AcquireAttemptInput struct {
	AttemptID          uuid.UUID
	LeaseID            uuid.UUID
	RunID              uuid.UUID
	ExpectedRunVersion int64
	RuntimeClass       string
	RuntimeInstanceID  string
	TTL                time.Duration
}

type TransitionAttemptInput struct {
	AttemptID              uuid.UUID
	FencingToken           int64
	ExpectedAttemptVersion int64
	To                     domain.AttemptPhase
	FailureCode            string
	FailureMessage         string
}

type HeartbeatLeaseInput struct {
	AttemptID            uuid.UUID
	FencingToken         int64
	ExpectedLeaseVersion int64
	TTL                  time.Duration
}

type CompleteRunInput struct {
	RunID              uuid.UUID
	AttemptID          uuid.UUID
	FencingToken       int64
	ExpectedRunVersion int64
	ResultRef          string
}

type KernelStore interface {
	CreateTask(context.Context, CreateTaskInput) (CreateTaskResult, error)
	TransitionTask(context.Context, uuid.UUID, int64, domain.TaskPhase) (Task, error)
	CreateRun(context.Context, CreateRunInput) (Run, error)
	AcquireAttempt(context.Context, AcquireAttemptInput) (AttemptLease, error)
	TransitionAttempt(context.Context, TransitionAttemptInput) (Attempt, error)
	HeartbeatLease(context.Context, HeartbeatLeaseInput) (Lease, error)
	CompleteRun(context.Context, CompleteRunInput) (Run, Task, error)
}
