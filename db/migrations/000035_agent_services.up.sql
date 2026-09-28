CREATE TABLE IF NOT EXISTS agent_services (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    namespace TEXT NOT NULL,
    name TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    spec JSONB NOT NULL,
    status JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_agent_service_tenant_ns_name UNIQUE (tenant_id, namespace, name)
);

CREATE INDEX IF NOT EXISTS idx_agent_services_tenant_ns ON agent_services(tenant_id, namespace);
CREATE INDEX IF NOT EXISTS idx_agent_services_agent ON agent_services(tenant_id, agent_id);

CREATE TABLE IF NOT EXISTS agent_service_instances (
    id TEXT PRIMARY KEY,
    service_id TEXT NOT NULL REFERENCES agent_services(id) ON DELETE CASCADE,
    tenant_id TEXT NOT NULL,
    namespace TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    address JSONB NOT NULL,
    phase TEXT NOT NULL,
    restart_count INT NOT NULL DEFAULT 0,
    consecutive_failures INT NOT NULL DEFAULT 0,
    next_restart_at TIMESTAMPTZ,
    last_heartbeat TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    terminated_at TIMESTAMPTZ,
    exit_code INT NOT NULL DEFAULT 0,
    exit_reason TEXT
);

CREATE INDEX IF NOT EXISTS idx_agent_service_instances_service ON agent_service_instances(service_id);
CREATE INDEX IF NOT EXISTS idx_agent_service_instances_tenant ON agent_service_instances(tenant_id);
CREATE INDEX IF NOT EXISTS idx_agent_service_instances_phase ON agent_service_instances(phase);
