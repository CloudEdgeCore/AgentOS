ALTER TABLE tasks
    ADD COLUMN admission_reason_code text,
    ADD COLUMN admitted_at timestamptz;

ALTER TABLE attempts
    ADD COLUMN runtime_pool_id text;

CREATE TABLE task_controller_claims (
    tenant_id       text NOT NULL CHECK (tenant_id <> ''),
    task_id         uuid NOT NULL,
    controller_kind text NOT NULL CHECK (controller_kind IN ('ADMISSION', 'SCHEDULING')),
    owner_id        text NOT NULL CHECK (owner_id <> ''),
    fencing_token   bigint NOT NULL CHECK (fencing_token > 0),
    resource_version bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    acquired_at     timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL CHECK (expires_at > acquired_at),
    PRIMARY KEY (tenant_id, task_id, controller_kind),
    FOREIGN KEY (tenant_id, task_id) REFERENCES tasks (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX task_controller_claims_expiry_idx
    ON task_controller_claims (controller_kind, expires_at);

CREATE TABLE admission_decisions (
    id               uuid PRIMARY KEY,
    tenant_id        text NOT NULL CHECK (tenant_id <> ''),
    task_id          uuid NOT NULL,
    decision         text NOT NULL CHECK (decision IN ('ADMIT', 'REJECT')),
    reason_code      text NOT NULL CHECK (reason_code <> ''),
    reasons          jsonb NOT NULL,
    evaluator_version text NOT NULL CHECK (evaluator_version <> ''),
    decided_at       timestamptz NOT NULL,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, task_id) REFERENCES tasks (tenant_id, id) ON DELETE RESTRICT
);

CREATE INDEX admission_decisions_task_idx
    ON admission_decisions (tenant_id, task_id, decided_at DESC);

ALTER TABLE outbox_events
    ADD COLUMN locked_by text,
    ADD COLUMN locked_until timestamptz,
    ADD COLUMN lock_fencing_token bigint NOT NULL DEFAULT 0 CHECK (lock_fencing_token >= 0),
    ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT '-infinity'::timestamptz,
    ADD CONSTRAINT outbox_lock_pair_check CHECK ((locked_by IS NULL) = (locked_until IS NULL));

ALTER TABLE outbox_events
    ADD CONSTRAINT outbox_aggregate_version_unique
    UNIQUE (tenant_id, aggregate_type, aggregate_id, aggregate_version);

DROP INDEX outbox_events_unpublished_idx;

CREATE INDEX outbox_events_dispatch_idx
    ON outbox_events (next_attempt_at, occurred_at, id)
    WHERE published_at IS NULL;
