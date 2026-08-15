CREATE TABLE task_budget_ledgers (
    tenant_id             text NOT NULL CHECK (tenant_id <> ''),
    task_id               uuid NOT NULL,
    reserved_tokens       bigint NOT NULL CHECK (reserved_tokens >= 0),
    reserved_cost_usd     double precision NOT NULL CHECK (reserved_cost_usd >= 0),
    reserved_tool_calls   bigint NOT NULL CHECK (reserved_tool_calls >= 0),
    reserved_wall_seconds bigint NOT NULL CHECK (reserved_wall_seconds >= 0),
    exhausted             boolean NOT NULL DEFAULT false,
    resource_version      bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    created_at            timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, task_id),
    FOREIGN KEY (tenant_id, task_id) REFERENCES tasks (tenant_id, id) ON DELETE RESTRICT
);

-- Append-only usage settlements. The unique (task, idempotency_key) pair makes
-- usage reports exactly-once at the kernel boundary; cumulative consumption is
-- derived by summing this ledger under the reservation row lock.
CREATE TABLE task_budget_settlements (
    id              uuid PRIMARY KEY,
    tenant_id       text NOT NULL CHECK (tenant_id <> ''),
    task_id         uuid NOT NULL,
    idempotency_key text NOT NULL CHECK (idempotency_key <> ''),
    tokens          bigint NOT NULL CHECK (tokens >= 0),
    cost_usd        double precision NOT NULL CHECK (cost_usd >= 0),
    tool_calls      bigint NOT NULL CHECK (tool_calls >= 0),
    wall_seconds    bigint NOT NULL CHECK (wall_seconds >= 0),
    occurred_at     timestamptz NOT NULL,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, task_id, idempotency_key),
    FOREIGN KEY (tenant_id, task_id) REFERENCES tasks (tenant_id, id) ON DELETE RESTRICT
);

CREATE INDEX task_budget_settlements_task_idx
    ON task_budget_settlements (tenant_id, task_id, occurred_at);
