CREATE TABLE runtime_pools (
    id text PRIMARY KEY CHECK (id <> ''),
    runtime_class text NOT NULL CHECK (runtime_class <> ''),
    runtime_instance_id text NOT NULL CHECK (runtime_instance_id <> ''),
    region text NOT NULL CHECK (region <> ''),
    data_residency text NOT NULL DEFAULT '',
    ready boolean NOT NULL DEFAULT false,
    status text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','CORDONED','DRAINING')),
    failure_domain text NOT NULL DEFAULT '',
    available_cpu_millis bigint NOT NULL CHECK (available_cpu_millis >= 0),
    available_memory_mib bigint NOT NULL CHECK (available_memory_mib >= 0),
    available_llm_slots integer NOT NULL CHECK (available_llm_slots >= 0),
    artifact_regions jsonb NOT NULL DEFAULT '[]'::jsonb,
    cost_weight double precision NOT NULL DEFAULT 0 CHECK (cost_weight >= 0 AND cost_weight < 'Infinity'::double precision),
    resource_version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE runtime_pool_tenant_grants (
    pool_id text NOT NULL REFERENCES runtime_pools(id) ON DELETE CASCADE,
    tenant_id text NOT NULL CHECK (tenant_id <> ''),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, pool_id)
);

CREATE INDEX runtime_pool_tenant_grants_pool_idx ON runtime_pool_tenant_grants(pool_id, tenant_id);
