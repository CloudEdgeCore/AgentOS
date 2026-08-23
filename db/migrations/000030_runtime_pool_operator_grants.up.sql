-- Runtime pool authority is split from usage authority. Tenant grants
-- (runtime_pool_tenant_grants) decide which tenants a pool is visible and
-- schedulable for; operator grants decide who may cordon, drain, or
-- reactivate the pool. A tenant usage grant never implies operator
-- authority, so one tenant of a shared pool cannot take the pool away from
-- its peers.
CREATE TABLE runtime_pool_operator_grants (
    pool_id text NOT NULL REFERENCES runtime_pools(id) ON DELETE CASCADE,
    subject text NOT NULL CHECK (subject <> ''),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (subject, pool_id)
);

CREATE INDEX runtime_pool_operator_grants_pool_idx
    ON runtime_pool_operator_grants(pool_id, subject);
