-- Service instances retain their execution generation across controller restarts.
ALTER TABLE agent_service_instances
    ADD COLUMN agent_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN runtime_class TEXT NOT NULL DEFAULT '',
    ADD COLUMN fencing_token BIGINT NOT NULL DEFAULT 0 CHECK (fencing_token >= 0),
    ADD COLUMN task_id UUID,
    ADD COLUMN launch_spec JSONB,
    ADD COLUMN draining_at TIMESTAMPTZ,
    ADD COLUMN drain_deadline TIMESTAMPTZ,
    ADD CONSTRAINT fk_service_instance_task FOREIGN KEY (tenant_id, task_id) REFERENCES tasks (tenant_id, id)
        DEFERRABLE INITIALLY DEFERRED;

CREATE INDEX idx_agent_service_instances_task ON agent_service_instances (tenant_id, task_id)
    WHERE task_id IS NOT NULL;

-- The earlier supervisor recorded readiness without launching a runtime task.
-- Its timestamps must not be carried forward as evidence of live execution.
UPDATE agent_service_instances
SET phase = 'Starting', last_heartbeat = '0001-01-01 00:00:00+00',
    next_restart_at = NULL, consecutive_failures = 0, updated_at = NOW()
WHERE phase IN ('Created', 'Starting', 'Running', 'Degraded', 'Recovering');

-- Legacy drain state had no persisted deadline and no underlying process.
UPDATE agent_service_instances
SET phase = 'Stopped', terminated_at = NOW(), next_restart_at = NULL,
    exit_code = 0, exit_reason = 'drained', updated_at = NOW()
WHERE phase IN ('Draining', 'Stopping');

UPDATE agent_services
SET status = status || jsonb_build_object(
        'phase', CASE WHEN COALESCE((spec->>'replicas')::INT, 0) > 0 THEN 'Degraded' ELSE 'Suspended' END,
        'readyReplicas', 0, 'availableReplicas', 0, 'updatedReplicas', 0,
        'lastTransitionAt', NOW(),
        'message', 'Runtime readiness is pending; configure a published agentVersionRef and workloadSpec for legacy services'),
    updated_at = NOW()
WHERE status->>'phase' <> 'Terminated';
