CREATE TABLE IF NOT EXISTS namespaces (
    tenant_id TEXT NOT NULL,
    name TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    phase TEXT NOT NULL DEFAULT 'Active',
    labels JSONB NOT NULL DEFAULT '{}'::jsonb,
    quota JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS idx_namespaces_tenant ON namespaces(tenant_id);

CREATE TABLE IF NOT EXISTS namespace_usage (
    tenant_id TEXT NOT NULL,
    namespace TEXT NOT NULL,
    active_services INT NOT NULL DEFAULT 0,
    active_instances INT NOT NULL DEFAULT 0,
    active_tasks INT NOT NULL DEFAULT 0,
    mailbox_messages INT NOT NULL DEFAULT 0,
    consumed_tokens BIGINT NOT NULL DEFAULT 0,
    consumed_cost_micro_usd BIGINT NOT NULL DEFAULT 0,
    consumed_tool_calls BIGINT NOT NULL DEFAULT 0,
    consumed_wall_seconds BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, namespace)
);
