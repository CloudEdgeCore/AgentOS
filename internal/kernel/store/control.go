package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	"github.com/google/uuid"
)

type ControllerKind string

const (
	ControllerAdmission  ControllerKind = "ADMISSION"
	ControllerScheduling ControllerKind = "SCHEDULING"
)

type TaskClaim struct {
	Task         Task
	Kind         ControllerKind
	OwnerID      string
	FencingToken int64
	ExpiresAt    time.Time
}

type ClaimTasksInput struct {
	Kind    ControllerKind
	Phase   domain.TaskPhase
	OwnerID string
	Limit   int
	TTL     time.Duration
}

type AdmissionReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

type DecideAdmissionInput struct {
	TaskID              uuid.UUID
	TenantID            string
	OwnerID             string
	ClaimFencingToken   int64
	ExpectedTaskVersion int64
	Admit               bool
	ReasonCode          string
	Reasons             []AdmissionReason
	EvaluatorVersion    string
}

type ScheduleTaskInput struct {
	TaskID              uuid.UUID
	TenantID            string
	OwnerID             string
	ClaimFencingToken   int64
	ExpectedTaskVersion int64
	RunID               uuid.UUID
	AttemptID           uuid.UUID
	LeaseID             uuid.UUID
	RuntimePoolID       string
	RuntimeClass        string
	RuntimeInstanceID   string
	LeaseTTL            time.Duration
}

type OutboxEvent struct {
	ID               uuid.UUID
	TenantID         string
	AggregateType    string
	AggregateID      uuid.UUID
	AggregateVersion int64
	EventType        string
	Payload          json.RawMessage
	OccurredAt       time.Time
	PublishAttempts  int
	LockedBy         string
	LockedUntil      time.Time
	LockFencingToken int64
}

type ClaimOutboxInput struct {
	DispatcherID string
	Limit        int
	LockTTL      time.Duration
}

type ControlStore interface {
	GetTask(context.Context, string, uuid.UUID) (Task, error)
	ClaimTasks(context.Context, ClaimTasksInput) ([]TaskClaim, error)
	ReleaseTaskClaim(context.Context, TaskClaim) error
	DecideAdmission(context.Context, DecideAdmissionInput) (Task, error)
	ScheduleTask(context.Context, ScheduleTaskInput) (AttemptLease, error)
	ClaimOutbox(context.Context, ClaimOutboxInput) ([]OutboxEvent, error)
	MarkOutboxPublished(context.Context, uuid.UUID, string, int64, time.Time) error
	MarkOutboxFailed(context.Context, uuid.UUID, string, int64, string, time.Time) error
}

type TaskCancellationStore interface {
	RequestTaskCancellation(context.Context, string, uuid.UUID, int64) (Task, error)
}
