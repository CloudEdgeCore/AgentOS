-- 000010: memory canonical store (ADR-009).
--
-- Memory is governed like data, not like a cache: provenance, tenant,
-- sensitivity, version, retention and tombstone survive in PostgreSQL, and
-- the v0.1 retrieval index (FTS + pg_trgm + pgvector) shares the same
-- transaction system. Derived indexes can always be rebuilt from this table.

-- The extensions are installed into pg_catalog so their types and operator
-- classes (vector, gin_trgm_ops) resolve under every search_path: the
-- conformance and load suites run their migrations in isolated schemas.
CREATE EXTENSION IF NOT EXISTS vector SCHEMA pg_catalog;
CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA pg_catalog;

CREATE TABLE memory_records (
    id                 uuid PRIMARY KEY,
    tenant_id          text NOT NULL CHECK (tenant_id <> ''),
    namespace          text NOT NULL CHECK (namespace <> ''),
    key                text NOT NULL CHECK (key <> ''),
    content_type       text NOT NULL CHECK (content_type <> ''),
    content            text NOT NULL CHECK (length(content) <= 262144),
    -- v0.1 default embedding dimension; the provider is recorded so a real
    -- embedding service can be introduced without mixing vector spaces.
    embedding          vector(384) NOT NULL,
    embedding_provider text NOT NULL CHECK (embedding_provider <> ''),
    sensitivity        text NOT NULL DEFAULT 'internal'
        CHECK (sensitivity IN ('internal', 'confidential', 'restricted')),
    -- Provenance: which task/run/attempt produced this memory.
    source_task_id     uuid,
    source_run_id      uuid,
    source_attempt_id  uuid,
    provenance         jsonb NOT NULL DEFAULT '{}',
    retention_until    timestamptz,
    tombstone_at       timestamptz,
    superseded_by      uuid,
    resource_version   bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    UNIQUE (tenant_id, namespace, key),
    FOREIGN KEY (tenant_id, source_task_id) REFERENCES tasks (tenant_id, id) ON DELETE SET NULL,
    FOREIGN KEY (tenant_id, source_run_id) REFERENCES runs (tenant_id, id) ON DELETE SET NULL,
    FOREIGN KEY (tenant_id, source_attempt_id) REFERENCES attempts (tenant_id, id) ON DELETE SET NULL
);

-- Full-text search over the 'simple' config (Latin tokenization). Chinese and
-- multilingual keyword quality is carried by the trigram index below, which
-- also serves substring/ILIKE matching; both must be evaluated per language
-- before relying on FTS ranking alone (tech baseline §13.2).
ALTER TABLE memory_records
    ADD COLUMN search tsvector GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED;

CREATE INDEX memory_records_search_idx ON memory_records USING GIN (search);
CREATE INDEX memory_records_trgm_idx ON memory_records USING GIN (content gin_trgm_ops);
CREATE INDEX memory_records_tenant_idx ON memory_records (tenant_id, namespace, updated_at DESC);

-- Exact vector search is the v0.1 default (ADR-009); an HNSW/ivfflat index is
-- only added after measured recall/latency requirements justify it.
