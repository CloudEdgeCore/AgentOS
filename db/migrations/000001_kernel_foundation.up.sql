CREATE TABLE tasks (
    id                  uuid PRIMARY KEY,
    tenant_id           text NOT NULL CHECK (tenant_id <> ''),
    namespace           text NOT NULL CHECK (namespace <> ''),
    agent_version_ref   text NOT NULL CHECK (agent_version_ref <> ''),
    goal                text NOT NULL CHECK (goal <> ''),
    spec                jsonb NOT NULL,
    request_hash        bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    idempotency_key     text NOT NULL CHECK (idempotency_key <> ''),
    phase               text NOT NULL DEFAULT 'QUEUED' CHECK (
        phase IN ('QUEUED', 'ADMITTED', 'RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED', 'TIMED_OUT', 'REJECTED')
    ),
    cancel_requested_at timestamptz,
    active_run_id       uuid,
    result_ref          text,
    resource_version    bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, namespace, idempotency_key)
);

CREATE TABLE runs (
    id                    uuid PRIMARY KEY,
    tenant_id             text NOT NULL CHECK (tenant_id <> ''),
    task_id               uuid NOT NULL,
    ordinal               integer NOT NULL CHECK (ordinal > 0),
    phase                 text NOT NULL DEFAULT 'PENDING' CHECK (
        phase IN ('PENDING', 'RUNNING', 'COMPLETED', 'FAILED', 'CANCELLED', 'TIMED_OUT')
    ),
    active_attempt_id     uuid,
    current_fencing_token bigint NOT NULL DEFAULT 0 CHECK (current_fencing_token >= 0),
    result_ref            text,
    resource_version      bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    created_at            timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL,
    completed_at          timestamptz,
    UNIQUE (tenant_id, id),
    UNIQUE (task_id, ordinal),
    FOREIGN KEY (tenant_id, task_id) REFERENCES tasks (tenant_id, id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX runs_one_active_per_task
    ON runs (task_id)
    WHERE phase IN ('PENDING', 'RUNNING');

CREATE TABLE attempts (
    id                  uuid PRIMARY KEY,
    tenant_id           text NOT NULL CHECK (tenant_id <> ''),
    run_id              uuid NOT NULL,
    ordinal             integer NOT NULL CHECK (ordinal > 0),
    phase               text NOT NULL DEFAULT 'PENDING' CHECK (
        phase IN (
            'PENDING', 'PLACED', 'STARTING', 'RUNNING', 'WAITING_TOOL', 'WAITING_AGENT',
            'WAITING_APPROVAL', 'CHECKPOINTING', 'COMPLETED', 'ATTEMPT_FAILED',
            'CANCEL_REQUESTED', 'CANCELLED'
        )
    ),
    runtime_class       text NOT NULL CHECK (runtime_class <> ''),
    runtime_instance_id text NOT NULL CHECK (runtime_instance_id <> ''),
    fencing_token       bigint NOT NULL CHECK (fencing_token > 0),
    failure_code        text,
    failure_message     text,
    resource_version    bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    started_at          timestamptz,
    finished_at         timestamptz,
    UNIQUE (tenant_id, id),
    UNIQUE (run_id, ordinal),
    FOREIGN KEY (tenant_id, run_id) REFERENCES runs (tenant_id, id) ON DELETE RESTRICT
);

ALTER TABLE tasks
    ADD CONSTRAINT tasks_active_run_fk
    FOREIGN KEY (tenant_id, active_run_id) REFERENCES runs (tenant_id, id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE runs
    ADD CONSTRAINT runs_active_attempt_fk
    FOREIGN KEY (tenant_id, active_attempt_id) REFERENCES attempts (tenant_id, id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE runtime_leases (
    id                  uuid PRIMARY KEY,
    tenant_id           text NOT NULL CHECK (tenant_id <> ''),
    run_id              uuid NOT NULL,
    attempt_id          uuid NOT NULL,
    fencing_token       bigint NOT NULL CHECK (fencing_token > 0),
    resource_version    bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    acquired_at         timestamptz NOT NULL,
    heartbeat_at        timestamptz NOT NULL,
    expires_at          timestamptz NOT NULL,
    released_at         timestamptz,
    release_reason      text,
    CHECK (expires_at > acquired_at),
    CHECK ((released_at IS NULL) = (release_reason IS NULL)),
    UNIQUE (tenant_id, id),
    UNIQUE (attempt_id),
    FOREIGN KEY (tenant_id, run_id) REFERENCES runs (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempts (tenant_id, id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX runtime_leases_one_active_per_run
    ON runtime_leases (run_id)
    WHERE released_at IS NULL;

CREATE INDEX runtime_leases_expiry_idx
    ON runtime_leases (expires_at)
    WHERE released_at IS NULL;

CREATE TABLE outbox_events (
    id                uuid PRIMARY KEY,
    tenant_id         text NOT NULL CHECK (tenant_id <> ''),
    aggregate_type    text NOT NULL CHECK (aggregate_type <> ''),
    aggregate_id      uuid NOT NULL,
    aggregate_version bigint NOT NULL CHECK (aggregate_version > 0),
    event_type        text NOT NULL CHECK (event_type <> ''),
    payload           jsonb NOT NULL,
    occurred_at       timestamptz NOT NULL,
    published_at      timestamptz,
    publish_attempts  integer NOT NULL DEFAULT 0 CHECK (publish_attempts >= 0),
    last_error        text
);

CREATE INDEX outbox_events_unpublished_idx
    ON outbox_events (occurred_at, id)
    WHERE published_at IS NULL;

CREATE TABLE inbox_receipts (
    tenant_id      text NOT NULL CHECK (tenant_id <> ''),
    consumer_name  text NOT NULL CHECK (consumer_name <> ''),
    event_id       uuid NOT NULL,
    processed_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, consumer_name, event_id)
);
