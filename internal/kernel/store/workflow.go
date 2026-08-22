package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// v1.2 Agent Orchestration: durable WorkflowRun and Step state. The
// orchestrator owns who executes when; the scheduler (unchanged) owns where
// a Task runs. Workflows never bypass the Task pipeline — steps create
// ordinary fenced Tasks.

var (
	// ErrWorkflowNotFound reports a workflow outside the tenant's scope.
	ErrWorkflowNotFound = errors.New("workflow not found")
	// ErrStepNotFound reports a step that does not belong to the workflow.
	ErrStepNotFound = errors.New("workflow step not found")
)

// WorkflowStatus is the lifecycle of one workflow run.
type WorkflowStatus string

const (
	WorkflowPending   WorkflowStatus = "PENDING"
	WorkflowRunning   WorkflowStatus = "RUNNING"
	WorkflowSucceeded WorkflowStatus = "SUCCEEDED"
	WorkflowFailed    WorkflowStatus = "FAILED"
	WorkflowCancelled WorkflowStatus = "CANCELLED"
)

func (s WorkflowStatus) Terminal() bool {
	switch s {
	case WorkflowSucceeded, WorkflowFailed, WorkflowCancelled:
		return true
	default:
		return false
	}
}

// CanTransitionWorkflow defines the workflow state machine.
func CanTransitionWorkflow(from, to WorkflowStatus) bool {
	switch from {
	case WorkflowPending:
		return to == WorkflowRunning || to == WorkflowCancelled
	case WorkflowRunning:
		return to == WorkflowSucceeded || to == WorkflowFailed || to == WorkflowCancelled
	default:
		return false
	}
}

// StepStatus is the lifecycle of one workflow step.
type StepStatus string

const (
	StepPending         StepStatus = "PENDING"
	StepWaitingApproval StepStatus = "WAITING_APPROVAL"
	StepRunning         StepStatus = "RUNNING"
	StepSucceeded       StepStatus = "SUCCEEDED"
	StepFailed          StepStatus = "FAILED"
	StepSkipped         StepStatus = "SKIPPED"
	StepCancelled       StepStatus = "CANCELLED"
)

func (s StepStatus) Terminal() bool {
	switch s {
	case StepSucceeded, StepFailed, StepSkipped, StepCancelled:
		return true
	default:
		return false
	}
}

// CanTransitionStep defines the step state machine. RUNNING → PENDING is the
// single-step retry path; it never touches sibling steps.
func CanTransitionStep(from, to StepStatus) bool {
	switch from {
	case StepPending:
		return to == StepWaitingApproval || to == StepRunning || to == StepSkipped || to == StepCancelled
	case StepWaitingApproval:
		return to == StepRunning || to == StepSkipped || to == StepCancelled
	case StepRunning:
		return to == StepSucceeded || to == StepFailed || to == StepCancelled || to == StepPending
	default:
		return false
	}
}

// Workflow is one durable workflow run.
type Workflow struct {
	ID                uuid.UUID
	TenantID          string
	Namespace         string
	IdempotencyKey    string
	Goal              string
	Spec              json.RawMessage
	Status            WorkflowStatus
	FailureCode       string
	CancelRequestedAt *time.Time
	ResourceVersion   int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// WorkflowStep is one step of a workflow: a declared DAG node plus its
// dispatch state. AttemptCount drives the single-step retry budget.
type WorkflowStep struct {
	ID              uuid.UUID
	TenantID        string
	WorkflowID      uuid.UUID
	Name            string
	Ordinal         int
	Status          StepStatus
	AttemptCount    int
	TaskID          *uuid.UUID
	ResultSummary   json.RawMessage
	FailureCode     string
	DecidedBy       string
	DecidedAt       *time.Time
	ResourceVersion int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

var stepNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,63})$`)

// ValidateStepName rejects step names that are not canonical tokens.
func ValidateStepName(name string) error {
	if !stepNamePattern.MatchString(name) {
		return fmt.Errorf("step name must match %s", stepNamePattern)
	}
	return nil
}

// CreateWorkflowStepInput is one declared step of a new workflow.
type CreateWorkflowStepInput struct {
	Name              string
	Spec              json.RawMessage // per-step workload spec overlay
	AgentVersionRef   string
	Goal              string
	DependsOn         []string
	ConditionStep     string
	ConditionContains string
	ConditionEquals   string
	RequiresApproval  bool
	MaxAttempts       int
}

// CreateWorkflowInput creates a workflow and its steps atomically.
type CreateWorkflowInput struct {
	ID             uuid.UUID
	TenantID       string
	Namespace      string
	IdempotencyKey string
	Goal           string
	Spec           json.RawMessage
	Steps          []CreateWorkflowStepInput
}

// RequestHash fingerprints the tenant-scoped workflow definition so an
// idempotency-key replay of a different definition is rejected.
func (in CreateWorkflowInput) RequestHash() ([sha256.Size]byte, error) {
	if strings.TrimSpace(in.TenantID) == "" || strings.TrimSpace(in.Goal) == "" ||
		len(in.Steps) == 0 || strings.TrimSpace(in.IdempotencyKey) == "" {
		return [sha256.Size]byte{}, fmt.Errorf("tenant, goal, idempotency key, and steps are required")
	}
	encoded, err := json.Marshal(struct {
		Goal  string                    `json:"goal"`
		Spec  json.RawMessage           `json:"spec"`
		Steps []CreateWorkflowStepInput `json:"steps"`
	}{in.Goal, json.RawMessage(canonicalJSON(in.Spec)), in.Steps})
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode workflow request: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

func canonicalJSON(raw json.RawMessage) []byte {
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return raw
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return raw
	}
	return encoded
}

// CreateWorkflowResult reports the created workflow and whether an
// idempotency-key replay returned the existing definition.
type CreateWorkflowResult struct {
	Workflow Workflow
	Existing bool
}

// TransitionWorkflowInput CAS-transitions one workflow.
type TransitionWorkflowInput struct {
	TenantID        string
	WorkflowID      uuid.UUID
	ExpectedVersion int64
	To              WorkflowStatus
	FailureCode     string
}

// DecideWorkflowStepApprovalInput records a human decision on one step.
type DecideWorkflowStepApprovalInput struct {
	TenantID        string
	WorkflowID      uuid.UUID
	StepName        string
	ExpectedVersion int64
	Approved        bool
	DecidedBy       string
}

// TransitionWorkflowStepInput CAS-transitions one step. Zero fields are
// left unchanged; TaskID/AttemptCount set when provided.
type TransitionWorkflowStepInput struct {
	TenantID        string
	WorkflowID      uuid.UUID
	StepName        string
	ExpectedVersion int64
	To              StepStatus
	TaskID          *uuid.UUID
	AttemptCount    *int
	ResultSummary   json.RawMessage
	FailureCode     string
}

// WorkflowStore is the durable workflow surface. The postgres Store
// satisfies it.
type WorkflowStore interface {
	CreateWorkflow(context.Context, CreateWorkflowInput) (CreateWorkflowResult, error)
	GetWorkflow(context.Context, string, uuid.UUID) (Workflow, error)
	// ListActiveWorkflows returns non-terminal workflows oldest-first for
	// the orchestrator reconcile loop (all tenants; creation is where
	// tenant scoping is enforced).
	ListActiveWorkflows(context.Context, int) ([]Workflow, error)
	ListWorkflowSteps(context.Context, string, uuid.UUID) ([]WorkflowStep, error)
	TransitionWorkflow(context.Context, TransitionWorkflowInput) (Workflow, error)
	TransitionWorkflowStep(context.Context, TransitionWorkflowStepInput) (WorkflowStep, error)
	DecideWorkflowStepApproval(context.Context, DecideWorkflowStepApprovalInput) (WorkflowStep, error)
	// RequestWorkflowCancellation records the durable cancellation intent;
	// the orchestrator propagates it to active steps and finalizes.
	RequestWorkflowCancellation(context.Context, string, uuid.UUID, int64) (Workflow, error)
	// ArtifactMetadata reads the digest, size and media type of one stored
	// artifact URI so the orchestrator can open task result documents.
	ArtifactMetadata(context.Context, string, string) ([]byte, int64, string, error)
}
