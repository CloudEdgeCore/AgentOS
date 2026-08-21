CREATE TABLE artifacts (
    id          uuid PRIMARY KEY,
    tenant_id   text NOT NULL CHECK (tenant_id <> ''),
    uri         text NOT NULL CHECK (uri <> ''),
    sha256      bytea NOT NULL CHECK (octet_length(sha256) = 32),
    size_bytes  bigint NOT NULL CHECK (size_bytes >= 0),
    media_type  text NOT NULL CHECK (media_type <> ''),
    created_at  timestamptz NOT NULL,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, sha256)
);

CREATE TABLE checkpoints (
    id                    uuid PRIMARY KEY,
    tenant_id             text NOT NULL CHECK (tenant_id <> ''),
    run_id                uuid NOT NULL,
    attempt_id            uuid NOT NULL,
    ordinal               integer NOT NULL CHECK (ordinal > 0),
    fencing_token         bigint NOT NULL CHECK (fencing_token > 0),
    agent_version_ref     text NOT NULL CHECK (agent_version_ref <> ''),
    runtime_class         text NOT NULL CHECK (runtime_class <> ''),
    provider              text NOT NULL CHECK (provider <> ''),
    runtime_abi           text NOT NULL CHECK (runtime_abi <> ''),
    schema_version        text NOT NULL CHECK (schema_version <> ''),
    state_artifact_id     uuid NOT NULL,
    confirmed_receipt_ids text[] NOT NULL DEFAULT '{}',
    envelope_sha256       bytea NOT NULL CHECK (octet_length(envelope_sha256) = 32),
    idempotency_key       text NOT NULL CHECK (idempotency_key <> ''),
    request_hash          bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    created_at            timestamptz NOT NULL,
    UNIQUE (tenant_id, id),
    UNIQUE (run_id, ordinal),
    UNIQUE (attempt_id, idempotency_key),
    FOREIGN KEY (tenant_id, run_id) REFERENCES runs (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempts (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, state_artifact_id) REFERENCES artifacts (tenant_id, id) ON DELETE RESTRICT
);

CREATE INDEX checkpoints_latest_run_idx
    ON checkpoints (tenant_id, run_id, ordinal DESC);

CREATE TABLE runtime_operation_receipts (
    tenant_id       text NOT NULL CHECK (tenant_id <> ''),
    attempt_id      uuid NOT NULL,
    operation       text NOT NULL CHECK (operation <> ''),
    idempotency_key text NOT NULL CHECK (idempotency_key <> ''),
    request_hash    bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    response         jsonb NOT NULL,
    processed_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, attempt_id, operation, idempotency_key),
    FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempts (tenant_id, id) ON DELETE RESTRICT
);
