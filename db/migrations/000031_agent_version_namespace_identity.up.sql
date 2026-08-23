-- P1-07: the AgentVersion namespace becomes part of the publication identity
-- (a k8s-style isolation boundary). Before this migration the unique key was
-- (tenant_id, name, version), so two teams sharing a tenant could not publish
-- the same name@version in different namespaces. Widen the identity to include
-- the namespace.
--
-- The swap is strictly widening: every tuple that was unique under
-- (tenant_id, name, version) remains unique under the 4-column key, so no
-- existing row can violate the new constraint. The (tenant_id, id) key that
-- tasks and checkpoints reference by foreign key is left untouched.

ALTER TABLE agent_versions
    DROP CONSTRAINT IF EXISTS agent_versions_tenant_id_name_version_key;

ALTER TABLE agent_versions
    ADD CONSTRAINT agent_versions_tenant_namespace_name_version_key
    UNIQUE (tenant_id, namespace, name, version);
