DROP INDEX IF EXISTS idx_agent_service_instances_task;
ALTER TABLE agent_service_instances
    DROP CONSTRAINT IF EXISTS fk_service_instance_task,
    DROP COLUMN IF EXISTS drain_deadline,
    DROP COLUMN IF EXISTS draining_at,
    DROP COLUMN IF EXISTS launch_spec,
    DROP COLUMN IF EXISTS task_id,
    DROP COLUMN IF EXISTS fencing_token,
    DROP COLUMN IF EXISTS runtime_class,
    DROP COLUMN IF EXISTS agent_version;
