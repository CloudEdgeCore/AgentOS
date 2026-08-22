package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const workflowColumns = `id, tenant_id, namespace, idempotency_key, goal, spec, status,
	COALESCE(failure_code, ''), cancel_requested_at, resource_version, created_at, updated_at`

const workflowStepColumns = `id, tenant_id, workflow_id, name, ordinal, status, attempt_count, task_id,
	result_summary, COALESCE(failure_code, ''), COALESCE(decided_by, ''), decided_at, resource_version, created_at, updated_at`

func scanWorkflow(row pgx.Row) (kernelstore.Workflow, error) {
	var workflow kernelstore.Workflow
	err := row.Scan(&workflow.ID, &workflow.TenantID, &workflow.Namespace, &workflow.IdempotencyKey,
		&workflow.Goal, &workflow.Spec, &workflow.Status, &workflow.FailureCode, &workflow.CancelRequestedAt,
		&workflow.ResourceVersion, &workflow.CreatedAt, &workflow.UpdatedAt)
	if err != nil {
		return kernelstore.Workflow{}, classify(err)
	}
	return workflow, nil
}

func scanWorkflowStep(row pgx.Row) (kernelstore.WorkflowStep, error) {
	var step kernelstore.WorkflowStep
	var summary []byte
	err := row.Scan(&step.ID, &step.TenantID, &step.WorkflowID, &step.Name, &step.Ordinal, &step.Status,
		&step.AttemptCount, &step.TaskID, &summary, &step.FailureCode, &step.DecidedBy, &step.DecidedAt,
		&step.ResourceVersion, &step.CreatedAt, &step.UpdatedAt)
	if summary != nil {
		step.ResultSummary = summary
	}
	if err != nil {
		return kernelstore.WorkflowStep{}, classify(err)
	}
	return step, nil
}

func jsonRawNull(raw []byte) []byte { return raw }

// CreateWorkflow inserts the workflow and its steps atomically. An
// idempotency-key replay returns the existing definition; a conflicting
// definition under the same key is rejected.
func (s *Store) CreateWorkflow(ctx context.Context, in kernelstore.CreateWorkflowInput) (kernelstore.CreateWorkflowResult, error) {
	hash, err := in.RequestHash()
	if err != nil {
		return kernelstore.CreateWorkflowResult{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return kernelstore.CreateWorkflowResult{}, err
	}
	defer rollback(ctx, tx)
	now := s.now()
	created, err := scanWorkflow(tx.QueryRow(ctx, `
		INSERT INTO workflows (id, tenant_id, namespace, idempotency_key, goal, spec, request_hash,
			status, resource_version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING', 1, $8, $8)
		ON CONFLICT (tenant_id, idempotency_key) DO UPDATE SET updated_at = workflows.updated_at
		RETURNING `+workflowColumns,
		in.ID, in.TenantID, in.Namespace, in.IdempotencyKey, in.Goal, in.Spec, hash[:], now))
	if err != nil {
		return kernelstore.CreateWorkflowResult{}, err
	}
	// Replay: the returned ID differs from the input when the idempotency
	// key already existed; the definition must match canonically (jsonb
	// normalizes key order).
	if created.ID != in.ID {
		if canonicalJSON(created.Spec) != canonicalJSON(in.Spec) || created.Goal != in.Goal {
			return kernelstore.CreateWorkflowResult{}, fmt.Errorf("%w: idempotency key replays a different definition",
				kernelstore.ErrIdempotencyConflict)
		}
		return kernelstore.CreateWorkflowResult{Workflow: created, Existing: true}, nil
	}
	for ordinal, step := range in.Steps {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workflow_steps (id, tenant_id, workflow_id, name, ordinal, status, attempt_count,
				resource_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'PENDING', 0, 1, $6, $6)`,
			uuid.New(), in.TenantID, in.ID, step.Name, ordinal, now); err != nil {
			return kernelstore.CreateWorkflowResult{}, classify(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return kernelstore.CreateWorkflowResult{}, classify(err)
	}
	return kernelstore.CreateWorkflowResult{Workflow: created}, nil
}

// GetWorkflow reads one workflow inside the tenant scope.
func (s *Store) GetWorkflow(ctx context.Context, tenantID string, id uuid.UUID) (kernelstore.Workflow, error) {
	workflow, err := scanWorkflow(s.pool.QueryRow(ctx,
		`SELECT `+workflowColumns+` FROM workflows WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if errors.Is(err, kernelstore.ErrNotFound) {
		return kernelstore.Workflow{}, kernelstore.ErrWorkflowNotFound
	}
	return workflow, err
}

// ListActiveWorkflows returns non-terminal workflows oldest-first.
func (s *Store) ListActiveWorkflows(ctx context.Context, batch int) ([]kernelstore.Workflow, error) {
	if batch <= 0 {
		batch = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+workflowColumns+` FROM workflows WHERE status IN ('PENDING','RUNNING')
		ORDER BY created_at ASC LIMIT $1`, batch)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	workflows := []kernelstore.Workflow{}
	for rows.Next() {
		workflow, err := scanWorkflowRow(rows)
		if err != nil {
			return nil, err
		}
		workflows = append(workflows, workflow)
	}
	return workflows, rows.Err()
}

func scanWorkflowRow(rows pgx.Rows) (kernelstore.Workflow, error) {
	var workflow kernelstore.Workflow
	var spec []byte
	err := rows.Scan(&workflow.ID, &workflow.TenantID, &workflow.Namespace, &workflow.IdempotencyKey,
		&workflow.Goal, &spec, &workflow.Status, &workflow.FailureCode, &workflow.CancelRequestedAt,
		&workflow.ResourceVersion, &workflow.CreatedAt, &workflow.UpdatedAt)
	workflow.Spec = spec
	if err != nil {
		return kernelstore.Workflow{}, classify(err)
	}
	return workflow, nil
}

// ListWorkflowSteps returns the workflow's steps in declaration order.
func (s *Store) ListWorkflowSteps(ctx context.Context, tenantID string, workflowID uuid.UUID) ([]kernelstore.WorkflowStep, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+workflowStepColumns+` FROM workflow_steps WHERE tenant_id = $1 AND workflow_id = $2
		ORDER BY ordinal ASC`, tenantID, workflowID)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	steps := []kernelstore.WorkflowStep{}
	for rows.Next() {
		var step kernelstore.WorkflowStep
		var summary []byte
		if err := rows.Scan(&step.ID, &step.TenantID, &step.WorkflowID, &step.Name, &step.Ordinal,
			&step.Status, &step.AttemptCount, &step.TaskID, &summary, &step.FailureCode, &step.DecidedBy,
			&step.DecidedAt, &step.ResourceVersion, &step.CreatedAt, &step.UpdatedAt); err != nil {
			return nil, classify(err)
		}
		step.ResultSummary = summary
		steps = append(steps, step)
	}
	return steps, rows.Err()
}

// TransitionWorkflow CAS-transitions the workflow status.
func (s *Store) TransitionWorkflow(ctx context.Context, in kernelstore.TransitionWorkflowInput) (kernelstore.Workflow, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return kernelstore.Workflow{}, err
	}
	defer rollback(ctx, tx)
	current, err := scanWorkflow(tx.QueryRow(ctx,
		`SELECT `+workflowColumns+` FROM workflows WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		in.TenantID, in.WorkflowID))
	if errors.Is(err, kernelstore.ErrWorkflowNotFound) {
		return kernelstore.Workflow{}, err
	}
	if err != nil {
		return kernelstore.Workflow{}, err
	}
	if current.ResourceVersion != in.ExpectedVersion {
		return kernelstore.Workflow{}, versionConflict("workflow", in.WorkflowID, in.ExpectedVersion, current.ResourceVersion)
	}
	if !kernelstore.CanTransitionWorkflow(current.Status, in.To) {
		return kernelstore.Workflow{}, fmt.Errorf("%w: workflow %s -> %s",
			kernelstore.ErrInvalidTransition, current.Status, in.To)
	}
	updated, err := scanWorkflow(tx.QueryRow(ctx, `
		UPDATE workflows SET status = $3, failure_code = NULLIF($4, ''), resource_version = resource_version + 1,
			updated_at = $5 WHERE id = $1 AND resource_version = $2 RETURNING `+workflowColumns,
		in.WorkflowID, in.ExpectedVersion, in.To, in.FailureCode, s.now()))
	if err != nil {
		return kernelstore.Workflow{}, classify(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return kernelstore.Workflow{}, classify(err)
	}
	return updated, nil
}

// TransitionWorkflowStep CAS-transitions one step, applying the optional
// task id, attempt count, result summary and failure code.
func (s *Store) TransitionWorkflowStep(ctx context.Context, in kernelstore.TransitionWorkflowStepInput) (kernelstore.WorkflowStep, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return kernelstore.WorkflowStep{}, err
	}
	defer rollback(ctx, tx)
	current, err := scanWorkflowStep(tx.QueryRow(ctx,
		`SELECT `+workflowStepColumns+` FROM workflow_steps WHERE tenant_id = $1 AND workflow_id = $2 AND name = $3 FOR UPDATE`,
		in.TenantID, in.WorkflowID, in.StepName))
	if err != nil {
		if errors.Is(err, kernelstore.ErrNotFound) {
			return kernelstore.WorkflowStep{}, kernelstore.ErrStepNotFound
		}
		return kernelstore.WorkflowStep{}, err
	}
	if current.ResourceVersion != in.ExpectedVersion {
		return kernelstore.WorkflowStep{}, versionConflict("workflow step", current.ID, in.ExpectedVersion, current.ResourceVersion)
	}
	if !kernelstore.CanTransitionStep(current.Status, in.To) {
		return kernelstore.WorkflowStep{}, fmt.Errorf("%w: step %s -> %s",
			kernelstore.ErrInvalidTransition, current.Status, in.To)
	}
	taskID := current.TaskID
	if in.TaskID != nil {
		taskID = in.TaskID
	}
	attemptCount := current.AttemptCount
	if in.AttemptCount != nil {
		attemptCount = *in.AttemptCount
	}
	summary := current.ResultSummary
	if in.ResultSummary != nil {
		summary = in.ResultSummary
	}
	failureCode := current.FailureCode
	if in.FailureCode != "" {
		failureCode = in.FailureCode
	} else if in.To == kernelstore.StepSucceeded || in.To == kernelstore.StepRunning {
		failureCode = ""
	}
	updated, err := scanWorkflowStep(tx.QueryRow(ctx, `
		UPDATE workflow_steps SET status = $4, task_id = $5, attempt_count = $6, result_summary = $7,
			failure_code = NULLIF($8, ''), resource_version = resource_version + 1, updated_at = $9
		WHERE id = $1 AND resource_version = $2 AND tenant_id = $3 RETURNING `+workflowStepColumns,
		current.ID, in.ExpectedVersion, in.TenantID, in.To, taskID, attemptCount, summary, failureCode, s.now()))
	if err != nil {
		return kernelstore.WorkflowStep{}, classify(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return kernelstore.WorkflowStep{}, classify(err)
	}
	return updated, nil
}

// DecideWorkflowStepApproval records a human decision (approve/reject) on a
// waiting step; the decided identity is durable audit state.
func (s *Store) DecideWorkflowStepApproval(ctx context.Context, in kernelstore.DecideWorkflowStepApprovalInput) (kernelstore.WorkflowStep, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return kernelstore.WorkflowStep{}, err
	}
	defer rollback(ctx, tx)
	current, err := scanWorkflowStep(tx.QueryRow(ctx,
		`SELECT `+workflowStepColumns+` FROM workflow_steps WHERE tenant_id = $1 AND workflow_id = $2 AND name = $3 FOR UPDATE`,
		in.TenantID, in.WorkflowID, in.StepName))
	if err != nil {
		if errors.Is(err, kernelstore.ErrNotFound) {
			return kernelstore.WorkflowStep{}, kernelstore.ErrStepNotFound
		}
		return kernelstore.WorkflowStep{}, err
	}
	if current.ResourceVersion != in.ExpectedVersion {
		return kernelstore.WorkflowStep{}, versionConflict("workflow step", current.ID, in.ExpectedVersion, current.ResourceVersion)
	}
	if current.Status != kernelstore.StepWaitingApproval {
		return kernelstore.WorkflowStep{}, fmt.Errorf("%w: only WAITING_APPROVAL steps can be decided",
			kernelstore.ErrInvalidTransition)
	}
	if current.DecidedBy != "" {
		return kernelstore.WorkflowStep{}, fmt.Errorf("%w: step already decided", kernelstore.ErrInvalidTransition)
	}
	// decided_by carries the machine decision (approved|rejected); the
	// deciding principal rides the control API audit surface.
	decision := "approved"
	if !in.Approved {
		decision = "rejected"
	}
	updated, err := scanWorkflowStep(tx.QueryRow(ctx, `
		UPDATE workflow_steps SET decided_by = $4, decided_at = $5, resource_version = resource_version + 1, updated_at = $5
		WHERE id = $1 AND resource_version = $2 AND tenant_id = $3 RETURNING `+workflowStepColumns,
		current.ID, in.ExpectedVersion, in.TenantID, decision, s.now()))
	if err != nil {
		return kernelstore.WorkflowStep{}, classify(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return kernelstore.WorkflowStep{}, classify(err)
	}
	return updated, nil
}

// RequestWorkflowCancellation records the durable cancel intent.
func (s *Store) RequestWorkflowCancellation(ctx context.Context, tenantID string, workflowID uuid.UUID, expectedVersion int64) (kernelstore.Workflow, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return kernelstore.Workflow{}, err
	}
	defer rollback(ctx, tx)
	current, err := scanWorkflow(tx.QueryRow(ctx,
		`SELECT `+workflowColumns+` FROM workflows WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, workflowID))
	if err != nil {
		return kernelstore.Workflow{}, err
	}
	if current.ResourceVersion != expectedVersion {
		return kernelstore.Workflow{}, versionConflict("workflow", workflowID, expectedVersion, current.ResourceVersion)
	}
	if current.Status.Terminal() {
		return kernelstore.Workflow{}, fmt.Errorf("%w: workflow already terminal", kernelstore.ErrInvalidTransition)
	}
	if current.CancelRequestedAt != nil {
		rollback(ctx, tx)
		return current, nil
	}
	updated, err := scanWorkflow(tx.QueryRow(ctx, `
		UPDATE workflows SET cancel_requested_at = $3, resource_version = resource_version + 1, updated_at = $3
		WHERE id = $1 AND resource_version = $2 RETURNING `+workflowColumns,
		workflowID, expectedVersion, s.now()))
	if err != nil {
		return kernelstore.Workflow{}, classify(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return kernelstore.Workflow{}, classify(err)
	}
	return updated, nil
}

// ArtifactMetadata reads the digest, size and media type of one artifact.
func (s *Store) ArtifactMetadata(ctx context.Context, tenantID, uri string) ([]byte, int64, string, error) {
	var digest []byte
	var size int64
	var mediaType string
	err := s.pool.QueryRow(ctx,
		`SELECT sha256, size_bytes, media_type FROM artifacts WHERE tenant_id = $1 AND uri = $2`,
		tenantID, uri).Scan(&digest, &size, &mediaType)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, "", kernelstore.ErrNotFound
		}
		return nil, 0, "", classify(err)
	}
	return digest, size, mediaType, nil
}

// canonicalJSON normalizes a JSON document for replay comparison.
func canonicalJSON(raw []byte) string {
	var document any
	if json.Unmarshal(raw, &document) != nil {
		return string(raw)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return string(raw)
	}
	return string(encoded)
}
