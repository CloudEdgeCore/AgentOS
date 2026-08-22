-- 000018: atomically reserve runtime capacity and establish explicit
-- workflow lineage on tasks. Both invariants are tenant-scoped and no longer
-- depend on stale scheduler snapshots or idempotency-key parsing.

CREATE TABLE runtime_pool_capacities (
    pool_id              text PRIMARY KEY CHECK (pool_id <> ''),
    total_cpu_millis     bigint NOT NULL CHECK (total_cpu_millis >= 0),
    total_memory_mib     bigint NOT NULL CHECK (total_memory_mib >= 0),
    total_llm_slots      integer NOT NULL CHECK (total_llm_slots >= 0),
    reserved_cpu_millis  bigint NOT NULL DEFAULT 0 CHECK (reserved_cpu_millis >= 0),
    reserved_memory_mib  bigint NOT NULL DEFAULT 0 CHECK (reserved_memory_mib >= 0),
    reserved_llm_slots   integer NOT NULL DEFAULT 0 CHECK (reserved_llm_slots >= 0),
    resource_version     bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    updated_at           timestamptz NOT NULL,
    CHECK (reserved_cpu_millis <= total_cpu_millis),
    CHECK (reserved_memory_mib <= total_memory_mib),
    CHECK (reserved_llm_slots <= total_llm_slots)
);

CREATE TABLE runtime_capacity_reservations (
    tenant_id           text NOT NULL CHECK (tenant_id <> ''),
    task_id             uuid NOT NULL,
    pool_id             text NOT NULL REFERENCES runtime_pool_capacities(pool_id) ON DELETE RESTRICT,
    cpu_millis          bigint NOT NULL CHECK (cpu_millis >= 0),
    memory_mib          bigint NOT NULL CHECK (memory_mib >= 0),
    llm_slots           integer NOT NULL CHECK (llm_slots >= 0),
    owner_fencing_token bigint NOT NULL CHECK (owner_fencing_token > 0),
    status              text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'RELEASED')),
    created_at          timestamptz NOT NULL,
    released_at         timestamptz,
    PRIMARY KEY (tenant_id, task_id),
    FOREIGN KEY (tenant_id, task_id) REFERENCES tasks(tenant_id, id) ON DELETE RESTRICT,
    CHECK ((status = 'ACTIVE' AND released_at IS NULL) OR (status = 'RELEASED' AND released_at IS NOT NULL))
);

CREATE INDEX runtime_capacity_reservations_active_idx
    ON runtime_capacity_reservations(pool_id, task_id) WHERE status = 'ACTIVE';

ALTER TABLE tasks
    ADD COLUMN workflow_id uuid,
    ADD COLUMN workflow_step_id uuid,
    ADD COLUMN workflow_step_name text,
    ADD COLUMN workflow_attempt integer,
    ADD COLUMN parent_task_id uuid;

ALTER TABLE workflows ADD CONSTRAINT workflows_tenant_id_unique UNIQUE (tenant_id, id);
ALTER TABLE workflow_steps ADD CONSTRAINT workflow_steps_tenant_id_unique UNIQUE (tenant_id, id);
ALTER TABLE tasks
    ADD CONSTRAINT tasks_workflow_fk FOREIGN KEY (tenant_id, workflow_id)
        REFERENCES workflows(tenant_id, id) ON DELETE RESTRICT,
    ADD CONSTRAINT tasks_workflow_step_fk FOREIGN KEY (tenant_id, workflow_step_id)
        REFERENCES workflow_steps(tenant_id, id) ON DELETE RESTRICT,
    ADD CONSTRAINT tasks_parent_task_fk FOREIGN KEY (tenant_id, parent_task_id)
        REFERENCES tasks(tenant_id, id) ON DELETE RESTRICT;

CREATE INDEX tasks_workflow_lineage_idx
    ON tasks(tenant_id, workflow_id, workflow_step_name, workflow_attempt)
    WHERE workflow_id IS NOT NULL;

CREATE INDEX tasks_tenant_id_idx ON tasks(tenant_id, id);
CREATE INDEX runs_tenant_id_idx ON runs(tenant_id, id);
CREATE INDEX attempts_tenant_id_idx ON attempts(tenant_id, id);
