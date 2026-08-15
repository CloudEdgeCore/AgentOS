package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	kernelstore "github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var _ kernelstore.ControlStore = (*Store)(nil)

func (s *Store) GetTask(ctx context.Context, tenantID string, id uuid.UUID) (kernelstore.Task, error) {
	if strings.TrimSpace(tenantID) == "" || id == uuid.Nil {
		return kernelstore.Task{}, kernelstore.ErrNotFound
	}
	task, err := scanTask(s.pool.QueryRow(ctx, `SELECT `+taskColumns+`
		FROM tasks WHERE tenant_id = $1 AND id = $2`, tenantID, id.String()))
	return task, classify(err)
}

func (s *Store) ClaimTasks(ctx context.Context, in kernelstore.ClaimTasksInput) ([]kernelstore.TaskClaim, error) {
	if err := validateClaimInput(in); err != nil {
		return nil, err
	}
	now := s.now()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, classify(err)
	}
	defer rollback(ctx, tx)

	rows, err := tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks t
		WHERE t.phase = $1
		  AND NOT EXISTS (
			SELECT 1 FROM task_controller_claims c
			WHERE c.tenant_id = t.tenant_id AND c.task_id = t.id
			  AND c.controller_kind = $2 AND c.expires_at > $3
		  )
		ORDER BY t.created_at, t.id
		FOR UPDATE OF t SKIP LOCKED
		LIMIT $4`, in.Phase, in.Kind, now, in.Limit)
	if err != nil {
		return nil, classify(err)
	}
	var tasks []kernelstore.Task
	for rows.Next() {
		task, scanErr := scanTask(rows)
		if scanErr != nil {
			rows.Close()
			return nil, classify(scanErr)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, classify(err)
	}
	rows.Close()

	claims := make([]kernelstore.TaskClaim, 0, len(tasks))
	for _, task := range tasks {
		var token int64
		var expires time.Time
		err := tx.QueryRow(ctx, `INSERT INTO task_controller_claims (
			tenant_id, task_id, controller_kind, owner_id, fencing_token,
			resource_version, acquired_at, expires_at
		) VALUES ($1, $2, $3, $4, 1, 1, $5, $6)
		ON CONFLICT (tenant_id, task_id, controller_kind) DO UPDATE SET
			owner_id = EXCLUDED.owner_id,
			fencing_token = task_controller_claims.fencing_token + 1,
			resource_version = task_controller_claims.resource_version + 1,
			acquired_at = EXCLUDED.acquired_at,
			expires_at = EXCLUDED.expires_at
		WHERE task_controller_claims.expires_at <= $5
		RETURNING fencing_token, expires_at`, task.TenantID, task.ID.String(), in.Kind,
			in.OwnerID, now, now.Add(in.TTL)).Scan(&token, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, classify(err)
		}
		claims = append(claims, kernelstore.TaskClaim{
			Task: task, Kind: in.Kind, OwnerID: in.OwnerID, FencingToken: token, ExpiresAt: expires,
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, classify(err)
	}
	return claims, nil
}

func (s *Store) DecideAdmission(ctx context.Context, in kernelstore.DecideAdmissionInput) (kernelstore.Task, error) {
	if strings.TrimSpace(in.TenantID) == "" || strings.TrimSpace(in.OwnerID) == "" ||
		strings.TrimSpace(in.ReasonCode) == "" || strings.TrimSpace(in.EvaluatorVersion) == "" {
		return kernelstore.Task{}, fmt.Errorf("admission tenant, owner, reason code, and evaluator version are required")
	}
	if in.Budget != nil && !in.Budget.Valid() {
		return kernelstore.Task{}, fmt.Errorf("budget values must not be negative")
	}
	reasons, err := json.Marshal(in.Reasons)
	if err != nil {
		return kernelstore.Task{}, fmt.Errorf("encode admission reasons: %w", err)
	}
	now := s.now()
	tx, err := s.begin(ctx)
	if err != nil {
		return kernelstore.Task{}, err
	}
	defer rollback(ctx, tx)

	task, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, in.TenantID, in.TaskID.String()))
	if err != nil {
		return kernelstore.Task{}, classify(err)
	}
	if task.ResourceVersion != in.ExpectedTaskVersion {
		return kernelstore.Task{}, versionConflict("task", task.ID, in.ExpectedTaskVersion, task.ResourceVersion)
	}
	if err := requireTaskClaim(ctx, tx, in.TenantID, in.TaskID, kernelstore.ControllerAdmission, in.OwnerID, in.ClaimFencingToken, now); err != nil {
		return kernelstore.Task{}, err
	}
	to := domain.TaskRejected
	decision := "REJECT"
	if in.Admit {
		to = domain.TaskAdmitted
		decision = "ADMIT"
	}
	if err := domain.ValidateTaskTransition(task.Phase, to); err != nil {
		return kernelstore.Task{}, fmt.Errorf("%w: %v", kernelstore.ErrInvalidTransition, err)
	}
	agentVersionID := sql.NullString{}
	if in.AgentVersionID != nil {
		agentVersionID = sql.NullString{String: in.AgentVersionID.String(), Valid: true}
	}
	updated, err := scanTask(tx.QueryRow(ctx, `UPDATE tasks SET phase = $1,
		admission_reason_code = $2,
		agent_version_id = $7,
		admitted_at = CASE WHEN $1 = 'ADMITTED' THEN $3 ELSE admitted_at END,
		resource_version = resource_version + 1, updated_at = $3
		WHERE tenant_id = $4 AND id = $5 AND resource_version = $6
		RETURNING `+taskColumns, to, in.ReasonCode, now, in.TenantID, in.TaskID.String(), in.ExpectedTaskVersion, agentVersionID))
	if err != nil {
		return kernelstore.Task{}, classifyCAS(err, "task", task.ID, in.ExpectedTaskVersion)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO admission_decisions (
		id, tenant_id, task_id, decision, reason_code, reasons, evaluator_version, decided_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, s.newID().String(), in.TenantID,
		in.TaskID.String(), decision, in.ReasonCode, reasons, in.EvaluatorVersion, now); err != nil {
		return kernelstore.Task{}, classify(err)
	}
	if err := deleteTaskClaim(ctx, tx, in.TenantID, in.TaskID, kernelstore.ControllerAdmission, in.OwnerID, in.ClaimFencingToken); err != nil {
		return kernelstore.Task{}, err
	}
	// The budget reservation commits atomically with the admission decision:
	// only admitted tasks hold a ledger, and rejected tasks never reserve.
	if to == domain.TaskAdmitted && in.Budget != nil && !in.Budget.Zero() {
		if _, err := tx.Exec(ctx, `INSERT INTO task_budget_ledgers (
			tenant_id, task_id, reserved_tokens, reserved_cost_usd,
			reserved_tool_calls, reserved_wall_seconds, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		ON CONFLICT (tenant_id, task_id) DO NOTHING`,
			in.TenantID, in.TaskID.String(), in.Budget.Tokens, in.Budget.CostUSD,
			in.Budget.ToolCalls, in.Budget.WallSeconds, now); err != nil {
			return kernelstore.Task{}, classify(err)
		}
	}
	if err := insertEvent(ctx, tx, in.TenantID, "Task", task.ID, updated.ResourceVersion, "Task"+title(string(to)), map[string]any{
		"taskId": task.ID, "phase": to, "reasonCode": in.ReasonCode, "reasons": in.Reasons,
	}, now, s.newID()); err != nil {
		return kernelstore.Task{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return kernelstore.Task{}, classify(err)
	}
	return updated, nil
}

func (s *Store) ReleaseTaskClaim(ctx context.Context, claim kernelstore.TaskClaim) error {
	command, err := s.pool.Exec(ctx, `DELETE FROM task_controller_claims
		WHERE tenant_id = $1 AND task_id = $2 AND controller_kind = $3
		  AND owner_id = $4 AND fencing_token = $5`, claim.Task.TenantID, claim.Task.ID.String(),
		claim.Kind, claim.OwnerID, claim.FencingToken)
	if err != nil {
		return classify(err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("%w: task claim changed before release", kernelstore.ErrFenced)
	}
	return nil
}

func (s *Store) ScheduleTask(ctx context.Context, in kernelstore.ScheduleTaskInput) (kernelstore.AttemptLease, error) {
	if err := validateScheduleInput(in); err != nil {
		return kernelstore.AttemptLease{}, err
	}
	if in.RunID == uuid.Nil {
		in.RunID = s.newID()
	}
	if in.AttemptID == uuid.Nil {
		in.AttemptID = s.newID()
	}
	if in.LeaseID == uuid.Nil {
		in.LeaseID = s.newID()
	}
	now := s.now()
	tx, err := s.begin(ctx)
	if err != nil {
		return kernelstore.AttemptLease{}, err
	}
	defer rollback(ctx, tx)

	task, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, in.TenantID, in.TaskID.String()))
	if err != nil {
		return kernelstore.AttemptLease{}, classify(err)
	}
	if task.ResourceVersion != in.ExpectedTaskVersion {
		return kernelstore.AttemptLease{}, versionConflict("task", task.ID, in.ExpectedTaskVersion, task.ResourceVersion)
	}
	if err := requireTaskClaim(ctx, tx, in.TenantID, in.TaskID, kernelstore.ControllerScheduling, in.OwnerID, in.ClaimFencingToken, now); err != nil {
		return kernelstore.AttemptLease{}, err
	}
	if err := domain.ValidateTaskTransition(task.Phase, domain.TaskRunning); err != nil {
		return kernelstore.AttemptLease{}, fmt.Errorf("%w: %v", kernelstore.ErrInvalidTransition, err)
	}
	var runOrdinal int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(ordinal), 0) + 1 FROM runs WHERE task_id = $1`, task.ID.String()).Scan(&runOrdinal); err != nil {
		return kernelstore.AttemptLease{}, classify(err)
	}
	run, err := scanRun(tx.QueryRow(ctx, `INSERT INTO runs (
		id, tenant_id, task_id, ordinal, phase, active_attempt_id, current_fencing_token,
		resource_version, created_at, updated_at
	) VALUES ($1, $2, $3, $4, 'RUNNING', $5, 1, 1, $6, $6) RETURNING `+runColumns,
		in.RunID.String(), in.TenantID, task.ID.String(), runOrdinal, in.AttemptID.String(), now))
	if err != nil {
		return kernelstore.AttemptLease{}, classify(err)
	}
	attempt, err := scanAttempt(tx.QueryRow(ctx, `INSERT INTO attempts (
		id, tenant_id, run_id, ordinal, phase, runtime_class, runtime_pool_id,
		runtime_instance_id, fencing_token, resource_version, created_at, updated_at
	) VALUES ($1, $2, $3, 1, 'PLACED', $4, $5, $6, 1, 1, $7, $7)
	RETURNING `+attemptColumns, in.AttemptID.String(), in.TenantID, run.ID.String(),
		in.RuntimeClass, in.RuntimePoolID, in.RuntimeInstanceID, now))
	if err != nil {
		return kernelstore.AttemptLease{}, classify(err)
	}
	lease, err := scanLease(tx.QueryRow(ctx, `INSERT INTO runtime_leases (
		id, tenant_id, run_id, attempt_id, fencing_token, resource_version,
		acquired_at, heartbeat_at, expires_at
	) VALUES ($1, $2, $3, $4, 1, 1, $5, $5, $6) RETURNING `+leaseColumns,
		in.LeaseID.String(), in.TenantID, run.ID.String(), attempt.ID.String(), now, now.Add(in.LeaseTTL)))
	if err != nil {
		return kernelstore.AttemptLease{}, classify(err)
	}
	updatedTask, err := scanTask(tx.QueryRow(ctx, `UPDATE tasks SET phase = 'RUNNING', active_run_id = $1,
		resource_version = resource_version + 1, updated_at = $2
		WHERE tenant_id = $3 AND id = $4 AND resource_version = $5 RETURNING `+taskColumns,
		run.ID.String(), now, in.TenantID, task.ID.String(), in.ExpectedTaskVersion))
	if err != nil {
		return kernelstore.AttemptLease{}, classifyCAS(err, "task", task.ID, in.ExpectedTaskVersion)
	}
	if err := deleteTaskClaim(ctx, tx, in.TenantID, task.ID, kernelstore.ControllerScheduling, in.OwnerID, in.ClaimFencingToken); err != nil {
		return kernelstore.AttemptLease{}, err
	}
	events := []struct {
		aggregateType string
		aggregateID   uuid.UUID
		version       int64
		eventType     string
		payload       map[string]any
	}{
		{"Run", run.ID, run.ResourceVersion, "RunCreated", map[string]any{"runId": run.ID, "taskId": task.ID, "ordinal": run.Ordinal}},
		{"Attempt", attempt.ID, attempt.ResourceVersion, "AttemptPlaced", map[string]any{"attemptId": attempt.ID, "runId": run.ID, "runtimePoolId": in.RuntimePoolID, "fencingToken": 1}},
		{"Task", task.ID, updatedTask.ResourceVersion, "TaskRunning", map[string]any{"taskId": task.ID, "runId": run.ID}},
	}
	for _, event := range events {
		if err := insertEvent(ctx, tx, in.TenantID, event.aggregateType, event.aggregateID, event.version, event.eventType, event.payload, now, s.newID()); err != nil {
			return kernelstore.AttemptLease{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return kernelstore.AttemptLease{}, classify(err)
	}
	return kernelstore.AttemptLease{Attempt: attempt, Lease: lease, Run: run}, nil
}

func (s *Store) ClaimOutbox(ctx context.Context, in kernelstore.ClaimOutboxInput) ([]kernelstore.OutboxEvent, error) {
	if strings.TrimSpace(in.DispatcherID) == "" || in.Limit < 1 || in.Limit > 500 || in.LockTTL <= 0 {
		return nil, fmt.Errorf("dispatcher id, limit 1..500, and positive lock TTL are required")
	}
	now := s.now()
	rows, err := s.pool.Query(ctx, `WITH candidates AS (
		SELECT id FROM outbox_events
		WHERE published_at IS NULL AND next_attempt_at <= $1
		  AND (locked_until IS NULL OR locked_until <= $1)
		  AND NOT EXISTS (
			SELECT 1 FROM outbox_events earlier
			WHERE earlier.tenant_id = outbox_events.tenant_id
			  AND earlier.aggregate_type = outbox_events.aggregate_type
			  AND earlier.aggregate_id = outbox_events.aggregate_id
			  AND earlier.aggregate_version < outbox_events.aggregate_version
			  AND earlier.published_at IS NULL
		  )
		ORDER BY next_attempt_at, occurred_at, id
		FOR UPDATE SKIP LOCKED LIMIT $2
	)
	UPDATE outbox_events e SET locked_by = $3, locked_until = $4,
		lock_fencing_token = lock_fencing_token + 1,
		publish_attempts = publish_attempts + 1
	FROM candidates c WHERE e.id = c.id
	RETURNING e.id::text, e.tenant_id, e.aggregate_type, e.aggregate_id::text,
		e.aggregate_version, e.event_type, e.payload, e.occurred_at,
		e.publish_attempts, e.locked_by, e.locked_until, e.lock_fencing_token`, now, in.Limit, in.DispatcherID, now.Add(in.LockTTL))
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	var events []kernelstore.OutboxEvent
	for rows.Next() {
		var event kernelstore.OutboxEvent
		var id, aggregateID string
		if err := rows.Scan(&id, &event.TenantID, &event.AggregateType, &aggregateID,
			&event.AggregateVersion, &event.EventType, &event.Payload, &event.OccurredAt,
			&event.PublishAttempts, &event.LockedBy, &event.LockedUntil, &event.LockFencingToken); err != nil {
			return nil, classify(err)
		}
		var parseErr error
		if event.ID, parseErr = uuid.Parse(id); parseErr != nil {
			return nil, fmt.Errorf("parse outbox id: %w", parseErr)
		}
		if event.AggregateID, parseErr = uuid.Parse(aggregateID); parseErr != nil {
			return nil, fmt.Errorf("parse outbox aggregate id: %w", parseErr)
		}
		events = append(events, event)
	}
	return events, classify(rows.Err())
}

func (s *Store) MarkOutboxPublished(ctx context.Context, eventID uuid.UUID, dispatcherID string, lockToken int64, publishedAt time.Time) error {
	now := s.now()
	command, err := s.pool.Exec(ctx, `UPDATE outbox_events SET published_at = $1,
		locked_by = NULL, locked_until = NULL, last_error = NULL
		WHERE id = $2 AND locked_by = $3 AND lock_fencing_token = $4
		  AND locked_until > $5 AND published_at IS NULL`, publishedAt.UTC(), eventID.String(), dispatcherID, lockToken, now)
	if err != nil {
		return classify(err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("%w: outbox event %s is not owned by dispatcher %s", kernelstore.ErrFenced, eventID, dispatcherID)
	}
	return nil
}

func (s *Store) MarkOutboxFailed(ctx context.Context, eventID uuid.UUID, dispatcherID string, lockToken int64, message string, retryAt time.Time) error {
	if len(message) > 2048 {
		message = message[:2048]
	}
	command, err := s.pool.Exec(ctx, `UPDATE outbox_events SET locked_by = NULL,
		locked_until = NULL, last_error = $1, next_attempt_at = $2
		WHERE id = $3 AND locked_by = $4 AND lock_fencing_token = $5
		  AND locked_until > $6 AND published_at IS NULL`, message, retryAt.UTC(), eventID.String(), dispatcherID, lockToken, s.now())
	if err != nil {
		return classify(err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("%w: outbox event %s is not owned by dispatcher %s", kernelstore.ErrFenced, eventID, dispatcherID)
	}
	return nil
}

func validateClaimInput(in kernelstore.ClaimTasksInput) error {
	if strings.TrimSpace(in.OwnerID) == "" || in.Limit < 1 || in.Limit > 100 || in.TTL <= 0 {
		return fmt.Errorf("claim owner, limit 1..100, and positive TTL are required")
	}
	valid := (in.Kind == kernelstore.ControllerAdmission && in.Phase == domain.TaskQueued) ||
		(in.Kind == kernelstore.ControllerScheduling && in.Phase == domain.TaskAdmitted)
	if !valid {
		return fmt.Errorf("controller kind %s cannot claim phase %s", in.Kind, in.Phase)
	}
	return nil
}

func validateScheduleInput(in kernelstore.ScheduleTaskInput) error {
	if strings.TrimSpace(in.TenantID) == "" || strings.TrimSpace(in.OwnerID) == "" ||
		strings.TrimSpace(in.RuntimePoolID) == "" || strings.TrimSpace(in.RuntimeClass) == "" ||
		strings.TrimSpace(in.RuntimeInstanceID) == "" || in.LeaseTTL <= 0 {
		return fmt.Errorf("schedule tenant, owner, runtime placement, and positive lease TTL are required")
	}
	return nil
}

func requireTaskClaim(ctx context.Context, tx pgx.Tx, tenantID string, taskID uuid.UUID, kind kernelstore.ControllerKind, ownerID string, token int64, now time.Time) error {
	var expires time.Time
	err := tx.QueryRow(ctx, `SELECT expires_at FROM task_controller_claims
		WHERE tenant_id = $1 AND task_id = $2 AND controller_kind = $3
		  AND owner_id = $4 AND fencing_token = $5 FOR UPDATE`, tenantID, taskID.String(), kind, ownerID, token).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: task claim owner or fencing token is stale", kernelstore.ErrFenced)
	}
	if err != nil {
		return classify(err)
	}
	if !expires.After(now) {
		return fmt.Errorf("%w: task claim expired at %s", kernelstore.ErrFenced, expires.Format(time.RFC3339Nano))
	}
	return nil
}

func deleteTaskClaim(ctx context.Context, tx pgx.Tx, tenantID string, taskID uuid.UUID, kind kernelstore.ControllerKind, ownerID string, token int64) error {
	command, err := tx.Exec(ctx, `DELETE FROM task_controller_claims
		WHERE tenant_id = $1 AND task_id = $2 AND controller_kind = $3
		  AND owner_id = $4 AND fencing_token = $5`, tenantID, taskID.String(), kind, ownerID, token)
	if err != nil {
		return classify(err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("%w: task claim changed before release", kernelstore.ErrFenced)
	}
	return nil
}
