CREATE TABLE agent_versions (
    id               uuid PRIMARY KEY,
    tenant_id        text NOT NULL CHECK (tenant_id <> ''),
    namespace        text NOT NULL CHECK (namespace <> ''),
    name             text NOT NULL CHECK (name <> ''),
    version          text NOT NULL CHECK (version <> ''),
    spec             jsonb NOT NULL,
    spec_digest      bytea NOT NULL CHECK (octet_length(spec_digest) = 32),
    resource_version bigint NOT NULL DEFAULT 1 CHECK (resource_version = 1),
    created_at       timestamptz NOT NULL,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, name, version)
);

-- An AgentVersion is immutable by contract: a published spec can never be
-- modified, and an upgrade is always the publication of a new version.
CREATE FUNCTION agent_versions_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'agent_versions rows are immutable; publish a new version instead';
END;
$$;

CREATE TRIGGER agent_versions_immutable
    BEFORE UPDATE OR DELETE ON agent_versions
    FOR EACH ROW EXECUTE FUNCTION agent_versions_reject_mutation();

-- Tasks bind to the immutable version their reference resolved to. The
-- composite foreign key enforces the tenant boundary.
ALTER TABLE tasks
    ADD COLUMN agent_version_id uuid;

ALTER TABLE tasks
    ADD CONSTRAINT tasks_agent_version_fk
    FOREIGN KEY (tenant_id, agent_version_id) REFERENCES agent_versions (tenant_id, id)
    ON DELETE RESTRICT;

CREATE INDEX tasks_agent_version_idx
    ON tasks (tenant_id, agent_version_id)
    WHERE agent_version_id IS NOT NULL;

-- Checkpoints record which published version the logical state belongs to.
ALTER TABLE checkpoints
    ADD COLUMN agent_version_id uuid;

ALTER TABLE checkpoints
    ADD CONSTRAINT checkpoints_agent_version_fk
    FOREIGN KEY (tenant_id, agent_version_id) REFERENCES agent_versions (tenant_id, id)
    ON DELETE RESTRICT;

CREATE INDEX checkpoints_agent_version_idx
    ON checkpoints (tenant_id, agent_version_id)
    WHERE agent_version_id IS NOT NULL;
