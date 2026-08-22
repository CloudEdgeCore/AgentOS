package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const workflowColumns = `id, tenant_id, namespace, idempotency_key, goal, spec, status,
	COALESCE(failure_code, ''), cancel_requested_at,
	COALESCE(budget_max_tasks, 0), COALESCE(budget_max_tokens, 0), COALESCE(budget_max_cost_usd, 0),
	budget_exhausted_at, deadline_at, deadline_exceeded_at, resource_version, created_at, updated_at`

const workflowStepColumns = `id, tenant_id, workflow_id, name, ordinal, status, attempt_count, task_id,
	result_summary, COALESCE(failure_code, ''), COALESCE(decided_by, ''), decided_at,
	COALESCE(parent_step_name, ''), spawn_depth, is_dynamic, COALESCE(spawn_key, ''),
	COALESCE(goal, ''), COALESCE(agent_version_ref, ''), spec, COALESCE(max_attempts, 0),
	resource_version, created_at, updated_at`

func scanWorkflow(row pgx.Row) (kernelstore.Workflow, error) {
	var workflow kernelstore.Workflow
	err := row.Scan(&workflow.ID, &workflow.TenantID, &workflow.Namespace, &workflow.IdempotencyKey,
		&workflow.Goal, &workflow.Spec, &workflow.Status, &workflow.FailureCode, &workflow.CancelRequestedAt,
		&workflow.BudgetMaxTasks, &workflow.BudgetMaxTokens, &workflow.BudgetMaxCostUSD,
		&workflow.BudgetExhaustedAt, &workflow.DeadlineAt, &workflow.DeadlineExceededAt,
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
		&step.ParentStepName, &step.SpawnDepth, &step.IsDynamic, &step.SpawnKey,
		&step.Goal, &step.AgentVersionRef, &step.Spec, &step.MaxAttempts,
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
			status, budget_max_tasks, budget_max_tokens, budget_max_cost_usd,
			deadline_at, step_count, resource_version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING', $8, $9, $10, $11, $12, 1, $13, $13)
		ON CONFLICT (tenant_id, idempotency_key) DO UPDATE SET updated_at = workflows.updated_at
		RETURNING `+workflowColumns,
		in.ID, in.TenantID, in.Namespace, in.IdempotencyKey, in.Goal, in.Spec, hash[:],
		nullableInt64(in.BudgetMaxTasks), nullableInt64(in.BudgetMaxTokens), nullableFloat(in.BudgetMaxCostUSD),
		in.DeadlineAt, len(in.Steps), now))
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
		&workflow.BudgetMaxTasks, &workflow.BudgetMaxTokens, &workflow.BudgetMaxCostUSD,
		&workflow.BudgetExhaustedAt, &workflow.DeadlineAt, &workflow.DeadlineExceededAt,
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
		var spec []byte
		if err := rows.Scan(&step.ID, &step.TenantID, &step.WorkflowID, &step.Name, &step.Ordinal,
			&step.Status, &step.AttemptCount, &step.TaskID, &summary, &step.FailureCode, &step.DecidedBy,
			&step.DecidedAt, &step.ParentStepName, &step.SpawnDepth, &step.IsDynamic, &step.SpawnKey,
			&step.Goal, &step.AgentVersionRef, &spec, &step.MaxAttempts,
			&step.ResourceVersion, &step.CreatedAt, &step.UpdatedAt); err != nil {
			return nil, classify(err)
		}
		step.ResultSummary = summary
		step.Spec = spec
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

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullableFloat(value float64) any {
	if value == 0 {
		return nil
	}
	return value
}

// ClaimWorkflows leases active workflows to one orchestrator instance
// (v1.3): unclaimed or expired-claim workflows are taken round-robin across
// tenants so one tenant's backlog cannot crowd out the others, and a dead
// instance's claims expire for its peers to take over. The claim columns are
// internal and never touch the client-observed resource_version.
func (s *Store) ClaimWorkflows(ctx context.Context, in kernelstore.ClaimWorkflowsInput) ([]kernelstore.Workflow, error) {
	if in.Owner == "" || in.Batch <= 0 || in.Lease <= 0 {
		return nil, fmt.Errorf("owner, positive batch and lease are required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	// Rank eligible work within each tenant, then take rank 1 from every
	// tenant before rank 2, and so on. This fills the batch under a small
	// tenant count without allowing one tenant's backlog to crowd out peers.
	// Budgeted workflows stay eligible: the controller must observe and
	// durably drain an exhausted run.
	now := s.now()
	rows, err := tx.Query(ctx, `
		SELECT `+workflowColumns+` FROM (
			SELECT workflows.*,
				ROW_NUMBER() OVER (PARTITION BY tenant_id ORDER BY
					CASE WHEN orchestrator_claim IS NULL OR orchestrator_claim_until <= $2 THEN 0 ELSE 1 END,
					orchestrator_claim_until ASC NULLS FIRST, created_at ASC, id ASC
				) AS tenant_rank
			FROM workflows
			WHERE status IN ('PENDING','RUNNING')
				AND (orchestrator_claim IS NULL OR orchestrator_claim = $1 OR orchestrator_claim_until <= $2)
		) fair
		ORDER BY tenant_rank ASC, orchestrator_claim_until ASC NULLS FIRST, created_at ASC, tenant_id ASC
		LIMIT $3`, in.Owner, now, in.Batch)
	if err != nil {
		return nil, classify(err)
	}
	candidates := []kernelstore.Workflow{}
	for rows.Next() {
		workflow, err := scanWorkflowRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, workflow)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	claimed := make([]kernelstore.Workflow, 0, len(candidates))
	for _, workflow := range candidates {
		updated, err := scanWorkflow(tx.QueryRow(ctx, `
			UPDATE workflows SET orchestrator_claim = $3, orchestrator_claim_until = $4
			WHERE id = $1 AND resource_version = $2
				AND (orchestrator_claim IS NULL OR orchestrator_claim = $3 OR orchestrator_claim_until <= $5)
			RETURNING `+workflowColumns,
			workflow.ID, workflow.ResourceVersion, in.Owner, now.Add(in.Lease), now))
		if err != nil {
			if errors.Is(err, kernelstore.ErrNotFound) {
				continue // lost the claim race; a peer took it
			}
			return nil, classify(err)
		}
		claimed = append(claimed, updated)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, classify(err)
	}
	return claimed, nil
}

// workflowTaskUsage aggregates the tasks carrying one workflow's idempotency
// prefix: their count, settled usage, and any unsettled overage.
func (s *Store) workflowTaskUsage(ctx context.Context, tx pgx.Tx, workflowID uuid.UUID) (kernelstore.WorkflowUsage, error) {
	var usage kernelstore.WorkflowUsage
	err := tx.QueryRow(ctx, `
		SELECT COUNT(DISTINCT t.id),
			COALESCE(SUM(s.tokens), 0),
			COALESCE(SUM(s.cost_usd), 0),
			COALESCE(BOOL_OR(s.id IS NULL), false)
		FROM tasks t
		LEFT JOIN task_budget_settlements s ON s.tenant_id = t.tenant_id AND s.task_id = t.id
		WHERE t.idempotency_key LIKE 'workflow/' || $1::uuid || '/%'
			AND t.tenant_id IN (SELECT tenant_id FROM workflows WHERE id = $1)`,
		workflowID).Scan(
		&usage.Tasks, &usage.Tokens, &usage.CostUSD, &usage.PendingOverage)
	if err != nil {
		return usage, classify(err)
	}
	return usage, nil
}

// WorkflowUsageSnapshot aggregates a workflow's task count and settled
// usage. Callers without a transaction use this entry point.
func (s *Store) WorkflowUsageSnapshot(ctx context.Context, tenantID string, workflowID uuid.UUID) (kernelstore.WorkflowUsage, error) {
	var usage kernelstore.WorkflowUsage
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT t.id),
			COALESCE(SUM(s.tokens), 0),
			COALESCE(SUM(s.cost_usd), 0),
			COALESCE(BOOL_OR(s.id IS NULL), false)
		FROM tasks t
		LEFT JOIN task_budget_settlements s ON s.tenant_id = t.tenant_id AND s.task_id = t.id
		WHERE t.tenant_id = $1 AND t.idempotency_key LIKE 'workflow/' || $2::uuid || '/%'`,
		tenantID, workflowID).Scan(
		&usage.Tasks, &usage.Tokens, &usage.CostUSD, &usage.PendingOverage)
	if err != nil {
		return usage, classify(err)
	}
	var budgets struct {
		Tasks  *int64
		Tokens *int64
		Cost   *float64
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT budget_max_tasks, budget_max_tokens, budget_max_cost_usd FROM workflows WHERE id = $1`,
		workflowID).Scan(&budgets.Tasks, &budgets.Tokens, &budgets.Cost); err != nil {
		return usage, classify(err)
	}
	if budgets.Tasks != nil {
		usage.BudgetMaxTasks = *budgets.Tasks
	}
	if budgets.Tokens != nil {
		usage.BudgetMaxTokens = *budgets.Tokens
	}
	if budgets.Cost != nil {
		usage.BudgetMaxCostUSD = *budgets.Cost
	}
	return usage, nil
}

// SpawnWorkflowStep creates one dynamic step with every recursion, fan-out
// and budget guard evaluated inside the same transaction as the insert
// (v1.3), so two racing spawn calls can never both slip past a cap. A
// spawn_key replay returns the existing step unchanged.
func (s *Store) SpawnWorkflowStep(ctx context.Context, in kernelstore.SpawnWorkflowStepInput) (kernelstore.SpawnWorkflowStepResult, error) {
	result := kernelstore.SpawnWorkflowStepResult{}
	if in.WorkflowID == uuid.Nil || strings.TrimSpace(in.TenantID) == "" || in.WorkflowVersion <= 0 ||
		strings.TrimSpace(in.ParentStepName) == "" || strings.TrimSpace(in.Goal) == "" ||
		strings.TrimSpace(in.AgentVersionRef) == "" || strings.TrimSpace(in.IdempotencyKey) == "" {
		return result, fmt.Errorf("workflow, tenant, version, parent, goal, agent version and idempotency key are required")
	}
	if err := kernelstore.ValidateStepName(in.Name); err != nil {
		return result, err
	}
	if len(in.Goal) > 8192 || len(in.IdempotencyKey) > 512 || in.MaxAttempts < 0 || in.MaxAttempts > 10 {
		return result, fmt.Errorf("spawn input exceeds its bound")
	}
	var specObject map[string]json.RawMessage
	if len(in.Spec) == 0 || json.Unmarshal(in.Spec, &specObject) != nil || specObject == nil {
		return result, fmt.Errorf("dynamic step spec must be a JSON object")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return result, err
	}
	defer rollback(ctx, tx)

	workflow, err := scanWorkflow(tx.QueryRow(ctx,
		`SELECT `+workflowColumns+` FROM workflows WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		in.TenantID, in.WorkflowID))
	if err != nil {
		if errors.Is(err, kernelstore.ErrNotFound) {
			return result, kernelstore.ErrWorkflowNotFound
		}
		return result, err
	}
	result.Workflow = workflow
	if !in.Guards.Enabled {
		return result, kernelstore.SpawnDenial{Code: "SPAWN_DISABLED", Message: "dynamic spawning is not enabled for this workflow"}
	}
	if workflow.ResourceVersion != in.WorkflowVersion {
		return result, versionConflict("workflow", in.WorkflowID, in.WorkflowVersion, workflow.ResourceVersion)
	}
	if workflow.Status.Terminal() {
		return result, fmt.Errorf("%w: workflow is terminal", kernelstore.ErrInvalidTransition)
	}
	if workflow.CancelRequestedAt != nil {
		return result, kernelstore.SpawnDenial{Code: "SPAWN_CANCELLED", Message: "workflow cancellation is in progress"}
	}
	if workflow.BudgetExhaustedAt != nil {
		result.WorkflowExhausted = true
		return result, kernelstore.SpawnDenial{Code: "SPAWN_BUDGET_EXHAUSTED", Message: "workflow budget is exhausted"}
	}
	if workflow.DeadlineExceededAt != nil || (workflow.DeadlineAt != nil && !s.now().Before(*workflow.DeadlineAt)) {
		return result, kernelstore.SpawnDenial{Code: "SPAWN_DEADLINE_EXCEEDED", Message: "workflow deadline has been exceeded"}
	}

	spawnKey := in.IdempotencyKey + "-" + kernelstore.SpawnKeyHash(in.Arguments)
	// Replay: the unique partial index makes a racing duplicate impossible.
	if existing, err := scanWorkflowStep(tx.QueryRow(ctx,
		`SELECT `+workflowStepColumns+` FROM workflow_steps WHERE workflow_id = $1 AND spawn_key = $2`,
		in.WorkflowID, spawnKey)); err == nil {
		result.Step = existing
		result.Usage = usageWithBudgets(workflow)
		return result, tx.Commit(ctx)
	} else if !errors.Is(err, kernelstore.ErrNotFound) {
		return result, err
	}
	if _, err := scanWorkflowStep(tx.QueryRow(ctx,
		`SELECT `+workflowStepColumns+` FROM workflow_steps WHERE workflow_id = $1 AND name = $2`,
		in.WorkflowID, in.Name)); err == nil {
		return result, kernelstore.SpawnDenial{Code: "SPAWN_NAME_CONFLICT", Message: "workflow step name already exists"}
	} else if !errors.Is(err, kernelstore.ErrNotFound) {
		return result, err
	}

	// Recursion guard: the spawning parent's depth bounds its children.
	var parent kernelstore.WorkflowStep
	var children int64
	if in.ParentStepName != "" {
		parent, err = scanWorkflowStep(tx.QueryRow(ctx,
			`SELECT `+workflowStepColumns+` FROM workflow_steps WHERE workflow_id = $1 AND name = $2 FOR UPDATE`,
			in.WorkflowID, in.ParentStepName))
		if err != nil {
			if errors.Is(err, kernelstore.ErrNotFound) {
				return result, kernelstore.ErrStepNotFound
			}
			return result, err
		}
		if err := tx.QueryRow(ctx,
			`SELECT child_count FROM workflow_steps WHERE workflow_id = $1 AND name = $2`,
			in.WorkflowID, in.ParentStepName).Scan(&children); err != nil {
			return result, classify(err)
		}
	}
	depth := parent.SpawnDepth + 1
	if in.Guards.MaxSpawnDepth > 0 && depth > in.Guards.MaxSpawnDepth {
		return result, kernelstore.SpawnDenial{Code: "SPAWN_DEPTH_EXCEEDED",
			Message: fmt.Sprintf("spawn depth %d exceeds maxSpawnDepth %d", depth, in.Guards.MaxSpawnDepth)}
	}

	// Workflow-level guards.
	var stepCount, dynamicCount int64
	if err := tx.QueryRow(ctx,
		`SELECT step_count, dynamic_step_count FROM workflows WHERE id = $1`,
		in.WorkflowID).Scan(&stepCount, &dynamicCount); err != nil {
		return result, classify(err)
	}
	if in.Guards.MaxWorkflowSteps > 0 && stepCount >= in.Guards.MaxWorkflowSteps {
		return result, kernelstore.SpawnDenial{Code: "SPAWN_TOTAL_STEPS_EXCEEDED",
			Message: fmt.Sprintf("workflow already has %d steps (maxWorkflowSteps %d)", stepCount, in.Guards.MaxWorkflowSteps)}
	}
	if in.Guards.MaxDynamicSteps > 0 && dynamicCount >= in.Guards.MaxDynamicSteps {
		return result, kernelstore.SpawnDenial{Code: "SPAWN_DYNAMIC_LIMIT_EXCEEDED",
			Message: fmt.Sprintf("workflow already has %d dynamic steps (maxDynamicSteps %d)", dynamicCount, in.Guards.MaxDynamicSteps)}
	}

	// Budget guards: tasks/tokens/cost ceilings across the whole run.
	usage, err := s.workflowTaskUsage(ctx, tx, in.WorkflowID)
	if err != nil {
		return result, err
	}
	usage.BudgetMaxTasks, usage.BudgetMaxTokens, usage.BudgetMaxCostUSD =
		workflow.BudgetMaxTasks, workflow.BudgetMaxTokens, workflow.BudgetMaxCostUSD
	result.Usage = usage
	if workflow.BudgetMaxTasks > 0 && usage.Tasks >= workflow.BudgetMaxTasks {
		result.WorkflowExhausted = true
		return result, kernelstore.SpawnDenial{Code: "SPAWN_BUDGET_EXHAUSTED",
			Message: fmt.Sprintf("workflow budget exhausted: %d/%d tasks", usage.Tasks, workflow.BudgetMaxTasks)}
	}
	if workflow.BudgetMaxTokens > 0 && usage.Tokens >= workflow.BudgetMaxTokens {
		result.WorkflowExhausted = true
		return result, kernelstore.SpawnDenial{Code: "SPAWN_BUDGET_EXHAUSTED",
			Message: fmt.Sprintf("workflow budget exhausted: %d/%d tokens", usage.Tokens, workflow.BudgetMaxTokens)}
	}
	if workflow.BudgetMaxCostUSD > 0 && usage.CostUSD >= workflow.BudgetMaxCostUSD {
		result.WorkflowExhausted = true
		return result, kernelstore.SpawnDenial{Code: "SPAWN_BUDGET_EXHAUSTED",
			Message: fmt.Sprintf("workflow budget exhausted: $%.6f/$%.6f", usage.CostUSD, workflow.BudgetMaxCostUSD)}
	}
	if in.Guards.MaxSpawnTasks > 0 && usage.Tasks >= in.Guards.MaxSpawnTasks {
		return result, kernelstore.SpawnDenial{Code: "SPAWN_TASK_LIMIT_EXCEEDED",
			Message: fmt.Sprintf("spawned task limit reached: %d/%d", usage.Tasks, in.Guards.MaxSpawnTasks)}
	}
	if in.Guards.MaxSpawnTokens > 0 && usage.Tokens >= in.Guards.MaxSpawnTokens {
		return result, kernelstore.SpawnDenial{Code: "SPAWN_TOKEN_LIMIT_EXCEEDED",
			Message: fmt.Sprintf("spawned token limit reached: %d/%d", usage.Tokens, in.Guards.MaxSpawnTokens)}
	}
	if in.Guards.MaxSpawnCostUSD > 0 && usage.CostUSD >= in.Guards.MaxSpawnCostUSD {
		return result, kernelstore.SpawnDenial{Code: "SPAWN_COST_LIMIT_EXCEEDED",
			Message: fmt.Sprintf("spawned cost limit reached: $%.6f/$%.6f", usage.CostUSD, in.Guards.MaxSpawnCostUSD)}
	}

	// Per-parent fan-out cap.
	if in.ParentStepName != "" {
		if in.Guards.MaxChildrenPerStep > 0 && children >= in.Guards.MaxChildrenPerStep {
			return result, kernelstore.SpawnDenial{Code: "SPAWN_FANOUT_EXCEEDED",
				Message: fmt.Sprintf("parent %q already spawned %d children (maxChildrenPerStep %d)",
					in.ParentStepName, children, in.Guards.MaxChildrenPerStep)}
		}
	}

	ordinal := int(stepCount)
	created, err := scanWorkflowStep(tx.QueryRow(ctx, `
		INSERT INTO workflow_steps (id, tenant_id, workflow_id, name, ordinal, status, attempt_count,
			parent_step_name, spawn_depth, is_dynamic, spawn_key, goal, agent_version_ref, spec, max_attempts,
			resource_version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'PENDING', 0, $6, $7, true, $8, $9, $10, $11, $12, 1, $13, $13)
		RETURNING `+workflowStepColumns,
		uuid.New(), in.TenantID, in.WorkflowID, in.Name, ordinal,
		nullableString(in.ParentStepName), depth, spawnKey, in.Goal, in.AgentVersionRef, in.Spec,
		nullableInt(int(in.MaxAttempts)), s.now()))
	if err != nil {
		return result, classify(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workflows SET step_count = step_count + 1, dynamic_step_count = dynamic_step_count + 1
		WHERE id = $1`, in.WorkflowID); err != nil {
		return result, classify(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workflow_steps SET child_count = child_count + 1
		WHERE workflow_id = $1 AND name = $2`, in.WorkflowID, in.ParentStepName); err != nil {
		return result, classify(err)
	}
	result.Step = created
	result.Created = true
	if err := insertEvent(ctx, tx, in.TenantID, "WorkflowStep", created.ID, 1, "WorkflowStepSpawned", map[string]any{
		"workflowId": in.WorkflowID, "step": created.Name, "parentStep": in.ParentStepName,
		"spawnDepth": depth, "spawnKey": spawnKey,
	}, s.now(), s.newID()); err != nil {
		return result, err
	}
	if err := tx.Commit(ctx); err != nil {
		return result, classify(err)
	}
	return result, nil
}

// MarkWorkflowBudgetExhausted records the durable budget-stop intent; the
// orchestrator observes it like a cancellation and drains the workflow.
func (s *Store) MarkWorkflowBudgetExhausted(ctx context.Context, tenantID string, workflowID uuid.UUID, expectedVersion int64) (kernelstore.Workflow, error) {
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
	if current.BudgetExhaustedAt != nil {
		rollback(ctx, tx)
		return current, nil
	}
	updated, err := scanWorkflow(tx.QueryRow(ctx, `
		UPDATE workflows SET budget_exhausted_at = $3, resource_version = resource_version + 1, updated_at = $3
		WHERE id = $1 AND resource_version = $2 RETURNING `+workflowColumns,
		workflowID, expectedVersion, s.now()))
	if err != nil {
		return kernelstore.Workflow{}, classify(err)
	}
	if err := insertEvent(ctx, tx, tenantID, "Workflow", workflowID, updated.ResourceVersion, "WorkflowBudgetExhausted", map[string]any{
		"workflowId": workflowID,
	}, s.now(), s.newID()); err != nil {
		return kernelstore.Workflow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return kernelstore.Workflow{}, classify(err)
	}
	return updated, nil
}

// MarkWorkflowDeadlineExceeded records the durable deadline-stop intent; the
// orchestrator then cancels active work and fails the workflow deterministically.
func (s *Store) MarkWorkflowDeadlineExceeded(ctx context.Context, tenantID string, workflowID uuid.UUID, expectedVersion int64) (kernelstore.Workflow, error) {
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
	if current.DeadlineExceededAt != nil {
		rollback(ctx, tx)
		return current, nil
	}
	updated, err := scanWorkflow(tx.QueryRow(ctx, `
		UPDATE workflows SET deadline_exceeded_at = $3, resource_version = resource_version + 1, updated_at = $3
		WHERE id = $1 AND resource_version = $2 RETURNING `+workflowColumns,
		workflowID, expectedVersion, s.now()))
	if err != nil {
		return kernelstore.Workflow{}, classify(err)
	}
	if err := insertEvent(ctx, tx, tenantID, "Workflow", workflowID, updated.ResourceVersion, "WorkflowDeadlineExceeded", map[string]any{
		"workflowId": workflowID, "deadline": current.DeadlineAt,
	}, s.now(), s.newID()); err != nil {
		return kernelstore.Workflow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return kernelstore.Workflow{}, classify(err)
	}
	return updated, nil
}

func nullableInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func usageWithBudgets(workflow kernelstore.Workflow) kernelstore.WorkflowUsage {
	return kernelstore.WorkflowUsage{
		BudgetMaxTasks: workflow.BudgetMaxTasks, BudgetMaxTokens: workflow.BudgetMaxTokens,
		BudgetMaxCostUSD: workflow.BudgetMaxCostUSD,
	}
}

// WorkflowLineage resolves the workflow origin of one task from its
// idempotency key ("workflow/<wf>/<step>/<attempt>", v1.3). Standalone tasks
// report ok=false.
func (s *Store) WorkflowLineage(ctx context.Context, tenantID, taskKey string) (uuid.UUID, string, int64, bool, error) {
	rest, ok := strings.CutPrefix(taskKey, "workflow/")
	if !ok {
		return uuid.Nil, "", 0, false, nil
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return uuid.Nil, "", 0, false, nil
	}
	workflowID, err := uuid.Parse(parts[0])
	if err != nil {
		return uuid.Nil, "", 0, false, nil
	}
	var version int64
	err = s.pool.QueryRow(ctx,
		`SELECT resource_version FROM workflows WHERE tenant_id = $1 AND id = $2`,
		tenantID, workflowID).Scan(&version)
	if err != nil {
		if errors.Is(err, kernelstore.ErrNotFound) {
			return uuid.Nil, "", 0, false, nil
		}
		return uuid.Nil, "", 0, false, classify(err)
	}
	return workflowID, parts[1], version, true, nil
}
