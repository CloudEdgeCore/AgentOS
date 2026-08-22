-- Effective task execution deadlines make wall-time budgets enforceable by
-- the recovery controller even while a worker continues heartbeating.
ALTER TABLE tasks
    ADD COLUMN execution_deadline_at timestamptz;

CREATE INDEX tasks_active_execution_deadline_idx
    ON tasks (execution_deadline_at, tenant_id, id)
    WHERE execution_deadline_at IS NOT NULL
      AND phase = 'RUNNING';
