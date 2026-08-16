-- Tenant aggregate consumption quotas (v0.6): windowed per-tenant limits on
-- aggregate token/cost/tool-call/wall-time consumption, enforced at task
-- admission and settled from the same settlement stream as per-task budgets.
--
-- A quota is a set of window limits; a limit of 0 means that dimension is
-- unlimited (matching task budget semantics). Consumption is exact: every
-- task budget settlement appends to the tenant's current window in the same
-- transaction that records the settlement, so replays never double-count.

CREATE TABLE tenant_quotas (
    tenant_id        text PRIMARY KEY CHECK (tenant_id <> ''),
    -- Window length in seconds; windows are fixed (aligned to the epoch),
    -- so the current window for any instant is deterministic.
    window_seconds   bigint NOT NULL DEFAULT 86400 CHECK (window_seconds >= 60),
    tokens           bigint NOT NULL DEFAULT 0 CHECK (tokens >= 0),
    cost_usd         double precision NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    tool_calls       bigint NOT NULL DEFAULT 0 CHECK (tool_calls >= 0),
    wall_seconds     bigint NOT NULL DEFAULT 0 CHECK (wall_seconds >= 0),
    resource_version bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL
);

-- One row per tenant per fixed window. Consumed counters are bumped by the
-- transaction that appends a settlement, under the window row lock, so the
-- aggregate is exact and idempotent. Old windows age out naturally; the row
-- set is bounded by (tenants x windows since the quota was configured).
CREATE TABLE tenant_consumption_windows (
    tenant_id              text NOT NULL CHECK (tenant_id <> ''),
    window_start           timestamptz NOT NULL,
    consumed_tokens        bigint NOT NULL DEFAULT 0 CHECK (consumed_tokens >= 0),
    consumed_cost_usd      double precision NOT NULL DEFAULT 0 CHECK (consumed_cost_usd >= 0),
    consumed_tool_calls    bigint NOT NULL DEFAULT 0 CHECK (consumed_tool_calls >= 0),
    consumed_wall_seconds  bigint NOT NULL DEFAULT 0 CHECK (consumed_wall_seconds >= 0),
    resource_version       bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    created_at             timestamptz NOT NULL,
    updated_at             timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, window_start)
);

CREATE INDEX tenant_consumption_windows_window_idx
    ON tenant_consumption_windows (window_start);
