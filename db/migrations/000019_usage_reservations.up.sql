-- 000019: pre-consumption reservations prevent concurrent model/tool calls
-- from each independently observing the same budget headroom.
CREATE TABLE task_usage_reservations (
    id uuid PRIMARY KEY,
    tenant_id text NOT NULL,
    task_id uuid NOT NULL,
    reservation_key text NOT NULL,
    tokens bigint NOT NULL DEFAULT 0 CHECK (tokens >= 0),
    cost_usd double precision NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    tool_calls bigint NOT NULL DEFAULT 0 CHECK (tool_calls >= 0),
    wall_seconds bigint NOT NULL DEFAULT 0 CHECK (wall_seconds >= 0),
    status text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','RELEASED','EXPIRED')),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    released_at timestamptz,
    UNIQUE (tenant_id, task_id, reservation_key),
    FOREIGN KEY (tenant_id, task_id) REFERENCES tasks (tenant_id, id)
);

CREATE INDEX task_usage_reservations_active_idx
    ON task_usage_reservations (tenant_id, task_id, expires_at)
    WHERE status = 'ACTIVE';
