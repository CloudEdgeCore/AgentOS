-- v1.3 Dynamic & Distributed Agent Orchestration: dynamic step spawning
-- with recursion guards and workflow budgets, plus orchestrator work-claim
-- leases for distributed sharding and per-tenant fairness.
--
-- Dynamic steps persist their full definition here (declared steps keep
-- theirs in the workflow document); spawn_key makes the brokered spawn tool
-- idempotent per (workflow, attempt, arguments). The orchestrator claim
-- columns are internal (they never participate in the resource_version CAS
-- clients observe) and expire so a dead instance's workflows are taken over
-- by its peers.
ALTER TABLE workflows ADD COLUMN budget_max_tasks bigint CHECK (budget_max_tasks IS NULL OR budget_max_tasks > 0),
    ADD COLUMN budget_max_tokens bigint CHECK (budget_max_tokens IS NULL OR budget_max_tokens > 0),
	ADD COLUMN budget_max_cost_usd numeric(14, 6) CHECK (budget_max_cost_usd IS NULL OR budget_max_cost_usd > 0),
	ADD COLUMN budget_exhausted_at timestamptz,
	ADD COLUMN deadline_at timestamptz,
	ADD COLUMN deadline_exceeded_at timestamptz,
	ADD COLUMN orchestrator_claim text,
	ADD COLUMN orchestrator_claim_until timestamptz,
	ADD COLUMN step_count bigint NOT NULL DEFAULT 0 CHECK (step_count >= 0),
	ADD COLUMN dynamic_step_count bigint NOT NULL DEFAULT 0 CHECK (dynamic_step_count >= 0 AND dynamic_step_count <= step_count);

ALTER TABLE workflow_steps ADD COLUMN parent_step_name text,
    ADD COLUMN spawn_depth integer NOT NULL DEFAULT 0 CHECK (spawn_depth >= 0),
    ADD COLUMN is_dynamic boolean NOT NULL DEFAULT false,
    ADD COLUMN spawn_key text,
    ADD COLUMN goal text,
    ADD COLUMN agent_version_ref text,
    ADD COLUMN spec jsonb,
    ADD COLUMN max_attempts integer,
    ADD COLUMN child_count bigint NOT NULL DEFAULT 0 CHECK (child_count >= 0);

-- Backfill aggregate counters for workflows created before this migration.
UPDATE workflows w SET
    step_count = counts.total,
    dynamic_step_count = counts.dynamic
FROM (
    SELECT workflow_id, COUNT(*) AS total, COUNT(*) FILTER (WHERE is_dynamic) AS dynamic
    FROM workflow_steps GROUP BY workflow_id
) counts
WHERE w.id = counts.workflow_id;

UPDATE workflow_steps parent SET child_count = counts.children
FROM (
    SELECT workflow_id, parent_step_name, COUNT(*) AS children
    FROM workflow_steps WHERE parent_step_name IS NOT NULL
    GROUP BY workflow_id, parent_step_name
) counts
WHERE parent.workflow_id = counts.workflow_id AND parent.name = counts.parent_step_name;

-- One spawn tool call creates at most one step, whatever the retries.
CREATE UNIQUE INDEX workflow_steps_spawn_key_idx ON workflow_steps (workflow_id, spawn_key)
    WHERE spawn_key IS NOT NULL;

-- Claim lease lookups for the distributed reconcile loop.
CREATE INDEX workflows_claim_idx ON workflows (orchestrator_claim_until)
    WHERE status IN ('PENDING', 'RUNNING');

-- Dynamic children of one step (group joins, child caps).
CREATE INDEX workflow_steps_parent_idx ON workflow_steps (workflow_id, parent_step_name)
    WHERE parent_step_name IS NOT NULL;
