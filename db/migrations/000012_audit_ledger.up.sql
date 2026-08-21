-- ADR-014: transactional append-only audit ledger. Every kernel decision
-- appends one audit event in the same transaction as the state change. The
-- per-tenant hash chain (prev_hash → chain_hash) makes tampering detectable,
-- and the control plane exports signed archives for compliance. Rows are
-- append-only at the database level: updates and deletes are rejected.
CREATE TABLE audit_events (
    id            uuid PRIMARY KEY,
    tenant_id     text NOT NULL CHECK (tenant_id <> ''),
    seq           bigint NOT NULL CHECK (seq > 0),
    prev_hash     bytea NOT NULL CHECK (octet_length(prev_hash) = 32),
    chain_hash    bytea NOT NULL CHECK (octet_length(chain_hash) = 32),
    event_type    text NOT NULL CHECK (event_type <> ''),
    resource_type text NOT NULL CHECK (resource_type <> ''),
    resource_id   uuid NOT NULL,
    actor         text NOT NULL CHECK (actor <> ''),
    details       jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at   timestamptz NOT NULL,
    UNIQUE (tenant_id, seq)
);

CREATE INDEX audit_events_tenant_seq_idx
    ON audit_events (tenant_id, seq);

CREATE FUNCTION audit_events_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'audit_events are append-only';
END;
$$;

CREATE TRIGGER audit_events_append_only
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_reject_mutation();
