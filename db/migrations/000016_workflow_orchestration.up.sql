-- v1.2 Agent Orchestration: durable WorkflowRun and Step state. The
-- orchestrator decides who executes when and creates ordinary Tasks; the
-- scheduler decides where a Task runs. Every transition is CAS-guarded so
-- concurrent orchestrator instances converge without double dispatch.
CREATE TABLE workflows (
    id                   uuid PRIMARY KEY,
    tenant_id            text NOT NULL CHECK (tenant_id <> ''),
    namespace            text NOT NULL CHECK (namespace <> ''),
    idempotency_key      text NOT NULL CHECK (idempotency_key <> ''),
    goal                 text NOT NULL CHECK (goal <> ''),
    spec                 jsonb NOT NULL,
    request_hash         bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    status               text NOT NULL DEFAULT 'PENDING' CHECK (
        status IN ('PENDING', 'RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED')
    ),
    failure_code         text,
    cancel_requested_at  timestamptz,
    resource_version     bigint NOT NULL CHECK (resource_version > 0),
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    UNIQUE (tenant_id, idempotency_key)
);

CREATE INDEX workflows_active_idx ON workflows (created_at)
    WHERE status IN ('PENDING', 'RUNNING');

CREATE TABLE workflow_steps (
    id                uuid PRIMARY KEY,
    tenant_id         text NOT NULL CHECK (tenant_id <> ''),
    workflow_id       uuid NOT NULL REFERENCES workflows (id),
    name              text NOT NULL CHECK (name <> ''),
    ordinal           integer NOT NULL CHECK (ordinal >= 0),
    status            text NOT NULL DEFAULT 'PENDING' CHECK (
        status IN ('PENDING', 'WAITING_APPROVAL', 'RUNNING', 'SUCCEEDED', 'FAILED', 'SKIPPED', 'CANCELLED')
    ),
    attempt_count     integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    task_id           uuid,
    result_summary    jsonb,
    failure_code      text,
    decided_by        text,
    decided_at        timestamptz,
    resource_version  bigint NOT NULL CHECK (resource_version > 0),
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    UNIQUE (workflow_id, name),
    UNIQUE (workflow_id, ordinal)
);

CREATE INDEX workflow_steps_dispatch_idx ON workflow_steps (workflow_id, status);
