-- Model Gateway slice: versioned model descriptors with price tables, and
-- the model-call ledger. Token and cost consumption is settled incrementally
-- against the task budget ledger (idempotent per sequence step); the ledger
-- itself enforces the hard stop when the reservation is exhausted.

CREATE TABLE model_descriptors (
    id                         uuid PRIMARY KEY,
    tenant_id                  text NOT NULL CHECK (tenant_id <> ''),
    provider                   text NOT NULL CHECK (provider <> ''),
    model_name                 text NOT NULL CHECK (model_name <> ''),
    supports_streaming         boolean NOT NULL DEFAULT true,
    input_price_per_million    numeric NOT NULL DEFAULT 0 CHECK (input_price_per_million >= 0),
    output_price_per_million   numeric NOT NULL DEFAULT 0 CHECK (output_price_per_million >= 0),
    price_revision             text NOT NULL DEFAULT 'v1' CHECK (price_revision <> ''),
    spec_hash                  bytea NOT NULL CHECK (octet_length(spec_hash) = 32),
    created_at                 timestamptz NOT NULL,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, provider, model_name, price_revision)
);

CREATE TABLE model_calls (
    id                  uuid PRIMARY KEY,
    tenant_id           text NOT NULL CHECK (tenant_id <> ''),
    task_id             uuid NOT NULL,
    run_id              uuid NOT NULL,
    attempt_id          uuid NOT NULL,
    model_ref           text NOT NULL CHECK (model_ref <> ''),
    status              text NOT NULL DEFAULT 'STARTED' CHECK (
        status IN ('STARTED', 'COMPLETED', 'FAILED', 'STOPPED')
    ),
    idempotency_key     text NOT NULL CHECK (idempotency_key <> ''),
    request_hash        bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    input_tokens        bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens       bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cost_usd            numeric NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    price_revision      text NOT NULL DEFAULT '' CHECK (price_revision <> ''),
    provider_request_id text,
    finish_reason       text,
    resource_version    bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, attempt_id, model_ref, idempotency_key),
    FOREIGN KEY (tenant_id, task_id) REFERENCES tasks (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, run_id) REFERENCES runs (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempts (tenant_id, id) ON DELETE RESTRICT
);
