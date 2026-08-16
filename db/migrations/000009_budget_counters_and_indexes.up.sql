-- 000009: rolling budget consumption counters + hot-path indexes.
--
-- Cumulative consumption used to be re-derived by SUMming the entire
-- append-only settlement ledger on every budget read and settlement
-- (O(n) per operation, O(n^2) over a task's lifetime). The ledger row now
-- carries rolling consumed counters updated in the same transaction that
-- appends a settlement, so reads are O(1) and stays exact under the
-- existing row lock (ADR-002).

ALTER TABLE task_budget_ledgers
    ADD COLUMN consumed_tokens bigint NOT NULL DEFAULT 0 CHECK (consumed_tokens >= 0),
    ADD COLUMN consumed_cost_usd double precision NOT NULL DEFAULT 0 CHECK (consumed_cost_usd >= 0),
    ADD COLUMN consumed_tool_calls bigint NOT NULL DEFAULT 0 CHECK (consumed_tool_calls >= 0),
    ADD COLUMN consumed_wall_seconds bigint NOT NULL DEFAULT 0 CHECK (consumed_wall_seconds >= 0);

-- Backfill from the append-only ledger so existing tasks stay exact.
UPDATE task_budget_ledgers l
SET consumed_tokens = s.tokens,
    consumed_cost_usd = s.cost_usd,
    consumed_tool_calls = s.tool_calls,
    consumed_wall_seconds = s.wall_seconds
FROM (
    SELECT tenant_id, task_id,
           COALESCE(SUM(tokens), 0) AS tokens,
           COALESCE(SUM(cost_usd), 0) AS cost_usd,
           COALESCE(SUM(tool_calls), 0) AS tool_calls,
           COALESCE(SUM(wall_seconds), 0) AS wall_seconds
    FROM task_budget_settlements
    GROUP BY tenant_id, task_id
) s
WHERE l.tenant_id = s.tenant_id AND l.task_id = s.task_id;

-- Controller claim poll: WHERE phase = ... ORDER BY created_at, id.
CREATE INDEX tasks_phase_idx ON tasks (phase, created_at, id);

-- Worker assignment poll: WHERE runtime_instance_id = ... AND phase IN (...).
CREATE INDEX attempts_runtime_poll_idx ON attempts (tenant_id, runtime_instance_id, phase);

-- Model descriptor resolution orders by registration recency.
CREATE INDEX model_descriptors_latest_idx
    ON model_descriptors (tenant_id, provider, model_name, created_at DESC);

-- Outbox claim's predecessor check filters unpublished events per aggregate.
CREATE INDEX outbox_events_claim_idx
    ON outbox_events (tenant_id, aggregate_type, aggregate_id, aggregate_version)
    WHERE published_at IS NULL;
