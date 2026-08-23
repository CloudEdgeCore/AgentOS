-- Workflow idempotency scope now includes the namespace, matching the
-- task idempotency scope (tenant_id, namespace, idempotency_key). The same
-- key under different namespaces of one tenant is a different workflow; a
-- replay within the same scope keeps returning the original definition.
-- No data change is required: the previous (tenant_id, idempotency_key)
-- uniqueness is strictly narrower, so every existing row set remains valid.
ALTER TABLE workflows DROP CONSTRAINT workflows_tenant_id_idempotency_key_key;
ALTER TABLE workflows ADD CONSTRAINT workflows_tenant_namespace_idempotency_unique
    UNIQUE (tenant_id, namespace, idempotency_key);
