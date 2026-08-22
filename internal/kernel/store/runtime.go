package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNoAssignment    = fmt.Errorf("no runtime assignment available")
	ErrLeaseNotExpired = fmt.Errorf("runtime lease has not expired")
)

type ArtifactReference struct {
	ID        uuid.UUID
	URI       string
	SHA256    [sha256.Size]byte
	SizeBytes int64
	MediaType string
}

func (a ArtifactReference) Validate() error {
	if strings.TrimSpace(a.URI) == "" || strings.TrimSpace(a.MediaType) == "" {
		return fmt.Errorf("artifact URI and media type are required")
	}
	if a.SizeBytes < 0 {
		return fmt.Errorf("artifact size must not be negative")
	}
	if a.SHA256 == ([sha256.Size]byte{}) {
		return fmt.Errorf("artifact SHA-256 is required")
	}
	return nil
}

func (a ArtifactReference) DigestHex() string { return hex.EncodeToString(a.SHA256[:]) }

type Checkpoint struct {
	ID                  uuid.UUID
	TenantID            string
	RunID               uuid.UUID
	AttemptID           uuid.UUID
	Ordinal             int
	FencingToken        int64
	AgentVersionRef     string
	RuntimeClass        string
	Provider            string
	RuntimeABI          string
	SchemaVersion       string
	State               ArtifactReference
	ConfirmedReceiptIDs []string
	EnvelopeSHA256      [sha256.Size]byte
	CreatedAt           time.Time
}

type RuntimeAssignment struct {
	Task Task
	// AgentVersion is the immutable publication resolved during admission.
	// Runtime providers receive this declaration so adapter selection and
	// capability grants never come from mutable Task input.
	AgentVersion     *AgentVersion
	Run              Run
	Attempt          Attempt
	Lease            Lease
	ResumeCheckpoint *Checkpoint
	// PendingApprovalID is the approval a WAITING_APPROVAL attempt must
	// re-present to the Tool Gateway to resume execution.
	PendingApprovalID *uuid.UUID
}

type CommitCheckpointInput struct {
	TenantID               string
	AttemptID              uuid.UUID
	FencingToken           int64
	ExpectedAttemptVersion int64
	IdempotencyKey         string
	CheckpointID           uuid.UUID
	AgentVersionRef        string
	Provider               string
	RuntimeABI             string
	SchemaVersion          string
	State                  ArtifactReference
	ConfirmedReceiptIDs    []string
}

func (in CommitCheckpointInput) Normalize() (CommitCheckpointInput, [sha256.Size]byte, error) {
	if strings.TrimSpace(in.TenantID) == "" || in.AttemptID == uuid.Nil || in.FencingToken <= 0 || in.ExpectedAttemptVersion <= 0 {
		return in, [sha256.Size]byte{}, fmt.Errorf("tenant, attempt, fencing token, and expected version are required")
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" || in.CheckpointID == uuid.Nil {
		return in, [sha256.Size]byte{}, fmt.Errorf("idempotency key and checkpoint ID are required")
	}
	if strings.TrimSpace(in.AgentVersionRef) == "" || strings.TrimSpace(in.Provider) == "" ||
		strings.TrimSpace(in.RuntimeABI) == "" || strings.TrimSpace(in.SchemaVersion) == "" {
		return in, [sha256.Size]byte{}, fmt.Errorf("checkpoint compatibility fields are required")
	}
	if err := in.State.Validate(); err != nil {
		return in, [sha256.Size]byte{}, err
	}
	in.ConfirmedReceiptIDs = append([]string(nil), in.ConfirmedReceiptIDs...)
	for _, id := range in.ConfirmedReceiptIDs {
		if strings.TrimSpace(id) == "" {
			return in, [sha256.Size]byte{}, fmt.Errorf("confirmed receipt IDs must not be empty")
		}
	}
	slices.Sort(in.ConfirmedReceiptIDs)
	in.ConfirmedReceiptIDs = slices.Compact(in.ConfirmedReceiptIDs)
	if in.ConfirmedReceiptIDs == nil {
		in.ConfirmedReceiptIDs = []string{}
	}
	encoded, err := json.Marshal(struct {
		AttemptID           string   `json:"attemptId"`
		FencingToken        int64    `json:"fencingToken"`
		CheckpointID        string   `json:"checkpointId"`
		AgentVersionRef     string   `json:"agentVersionRef"`
		Provider            string   `json:"provider"`
		RuntimeABI          string   `json:"runtimeAbi"`
		SchemaVersion       string   `json:"schemaVersion"`
		StateURI            string   `json:"stateUri"`
		StateSHA256         string   `json:"stateSha256"`
		StateSize           int64    `json:"stateSize"`
		StateMediaType      string   `json:"stateMediaType"`
		ConfirmedReceiptIDs []string `json:"confirmedReceiptIds"`
	}{in.AttemptID.String(), in.FencingToken, in.CheckpointID.String(), in.AgentVersionRef,
		in.Provider, in.RuntimeABI, in.SchemaVersion, in.State.URI, in.State.DigestHex(),
		in.State.SizeBytes, in.State.MediaType, in.ConfirmedReceiptIDs})
	if err != nil {
		return in, [sha256.Size]byte{}, fmt.Errorf("encode checkpoint request: %w", err)
	}
	return in, sha256.Sum256(encoded), nil
}

type CompleteAttemptInput struct {
	TenantID               string
	AttemptID              uuid.UUID
	FencingToken           int64
	ExpectedAttemptVersion int64
	IdempotencyKey         string
	Result                 ArtifactReference
}

func (in CompleteAttemptInput) RequestHash() ([sha256.Size]byte, error) {
	if strings.TrimSpace(in.TenantID) == "" || in.AttemptID == uuid.Nil || in.FencingToken <= 0 || in.ExpectedAttemptVersion <= 0 {
		return [sha256.Size]byte{}, fmt.Errorf("tenant, attempt, fencing token, and expected version are required")
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return [sha256.Size]byte{}, fmt.Errorf("idempotency key is required")
	}
	if err := in.Result.Validate(); err != nil {
		return [sha256.Size]byte{}, err
	}
	encoded, err := json.Marshal(struct {
		AttemptID    string `json:"attemptId"`
		FencingToken int64  `json:"fencingToken"`
		URI          string `json:"uri"`
		SHA256       string `json:"sha256"`
		SizeBytes    int64  `json:"sizeBytes"`
		MediaType    string `json:"mediaType"`
	}{in.AttemptID.String(), in.FencingToken, in.Result.URI, in.Result.DigestHex(), in.Result.SizeBytes, in.Result.MediaType})
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode completion request: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

type CompleteAttemptResult struct {
	Attempt Attempt
	Run     Run
	Task    Task
}

type CancelAttemptInput struct {
	TenantID               string
	AttemptID              uuid.UUID
	FencingToken           int64
	ExpectedAttemptVersion int64
	IdempotencyKey         string
}

func (in CancelAttemptInput) RequestHash() ([sha256.Size]byte, error) {
	if strings.TrimSpace(in.TenantID) == "" || in.AttemptID == uuid.Nil || in.FencingToken <= 0 ||
		in.ExpectedAttemptVersion <= 0 || strings.TrimSpace(in.IdempotencyKey) == "" {
		return [sha256.Size]byte{}, fmt.Errorf("tenant, attempt, fencing token, expected version, and idempotency key are required")
	}
	return sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00CANCEL", in.AttemptID, in.FencingToken))), nil
}

type CancelAttemptResult struct {
	Attempt Attempt
	Run     Run
	Task    Task
}

type RecoveryCandidate struct {
	TenantID     string
	AttemptID    uuid.UUID
	FencingToken int64
	TaskSpec     json.RawMessage
}

type RecoverExpiredAttemptInput struct {
	TenantID     string
	AttemptID    uuid.UUID
	FencingToken int64
	NewAttemptID uuid.UUID
	NewLeaseID   uuid.UUID
	LeaseTTL     time.Duration
	MaxAttempts  int
}

type RecoveryResult struct {
	Retried bool
	Lease   AttemptLease
}

// HeartbeatStatus is the narrow state a lease renewal needs: whether the task
// has a pending cancellation and the attempt's current resource version. It
// lets the Runtime Protocol heartbeat answer cancel checks without
// re-materializing the full assignment.
type HeartbeatStatus struct {
	CancelRequested bool
	AttemptVersion  int64
}

type RuntimeStore interface {
	KernelStore
	// WorkflowLineage resolves the workflow origin of one task from its
	// idempotency key (v1.3); ok=false marks standalone tasks. The version is
	// the workflow's current resource version at read time.
	WorkflowLineage(context.Context, string, string) (workflowID uuid.UUID, stepName string, version int64, ok bool, err error)
	PollRuntimeAssignment(context.Context, string, string) (RuntimeAssignment, error)
	GetRuntimeAssignment(context.Context, string, uuid.UUID, int64) (RuntimeAssignment, error)
	GetHeartbeatStatus(context.Context, string, uuid.UUID, int64) (HeartbeatStatus, error)
	CommitCheckpoint(context.Context, CommitCheckpointInput) (Checkpoint, Attempt, error)
	CompleteAttempt(context.Context, CompleteAttemptInput) (CompleteAttemptResult, error)
	AcknowledgeCancellation(context.Context, CancelAttemptInput) (CancelAttemptResult, error)
	ListExpiredAttempts(context.Context, time.Time, int) ([]RecoveryCandidate, error)
	RecoverExpiredAttempt(context.Context, RecoverExpiredAttemptInput) (RecoveryResult, error)
}

// PoolHealthStore reports runtime instance liveness derived from lease
// heartbeats (v0.6): placement consults it before scheduling so that pools
// whose worker stopped renewing its lease are rejected instead of stranding
// a new attempt until lease-expiry recovery.
type PoolHealthStore interface {
	// PoolInstanceHealth reports whether each runtime instance is presumed
	// alive at now. An instance is unhealthy while it holds an unreleased
	// lease whose heartbeat is stale: the lease expired (expires_at <= now)
	// or was not renewed within the freshness window (heartbeat_at <= now -
	// freshness). An instance with no unreleased leases is healthy — idling
	// is not a failure signal. Every requested instance ID appears in the
	// result; callers treat an ID missing from the map as unhealthy
	// (fail-closed).
	PoolInstanceHealth(context.Context, []string, time.Time, time.Duration) (map[string]bool, error)
}
