-- Tool Gateway slice: versioned tool descriptors, the tool-call decision
-- ledger, and approval requests bound to canonical call summaries (invariant
-- 9: an approval binds tool name, canonical args hash, target resource and
-- expiry, and cannot be reused by another invocation).
--
-- Side-effect receipts for tool calls reuse runtime_operation_receipts
-- (000003) with operation 'TOOL:<name>@<version>'.

CREATE TABLE tool_descriptors (
    id                uuid PRIMARY KEY,
    tenant_id         text NOT NULL CHECK (tenant_id <> ''),
    name              text NOT NULL CHECK (name <> ''),
    version           text NOT NULL CHECK (version <> ''),
    side_effect_risk  text NOT NULL CHECK (side_effect_risk IN ('none', 'low', 'high')),
    actions           text[] NOT NULL DEFAULT '{}',
    resource_patterns text[] NOT NULL DEFAULT '{}',
    params_schema     jsonb NOT NULL,
    spec_hash         bytea NOT NULL CHECK (octet_length(spec_hash) = 32),
    created_at        timestamptz NOT NULL,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, name, version)
);

CREATE TABLE tool_calls (
    id               uuid PRIMARY KEY,
    tenant_id        text NOT NULL CHECK (tenant_id <> ''),
    task_id          uuid NOT NULL,
    run_id           uuid NOT NULL,
    attempt_id       uuid NOT NULL,
    tool_name        text NOT NULL CHECK (tool_name <> ''),
    tool_version     text NOT NULL CHECK (tool_version <> ''),
    action           text NOT NULL CHECK (action <> ''),
    resource         text NOT NULL CHECK (resource <> ''),
    args_hash        bytea NOT NULL CHECK (octet_length(args_hash) = 32),
    status           text NOT NULL DEFAULT 'PENDING' CHECK (
        status IN ('PENDING', 'REQUIRES_APPROVAL', 'APPROVED', 'EXECUTED', 'DENIED', 'FAILED')
    ),
    decision_reasons text[] NOT NULL DEFAULT '{}',
    policy_revision  text NOT NULL DEFAULT '',
    approval_id      uuid,
    idempotency_key  text NOT NULL CHECK (idempotency_key <> ''),
    request_hash     bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    resource_version bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, attempt_id, tool_name, idempotency_key),
    FOREIGN KEY (tenant_id, task_id) REFERENCES tasks (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, run_id) REFERENCES runs (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempts (tenant_id, id) ON DELETE RESTRICT
);

CREATE TABLE tool_approvals (
    id                uuid PRIMARY KEY,
    tenant_id         text NOT NULL CHECK (tenant_id <> ''),
    call_id           uuid NOT NULL,
    task_id           uuid NOT NULL,
    run_id            uuid NOT NULL,
    attempt_id        uuid NOT NULL,
    tool_name         text NOT NULL CHECK (tool_name <> ''),
    tool_version      text NOT NULL CHECK (tool_version <> ''),
    action            text NOT NULL CHECK (action <> ''),
    resource          text NOT NULL CHECK (resource <> ''),
    args_hash         bytea NOT NULL CHECK (octet_length(args_hash) = 32),
    status            text NOT NULL DEFAULT 'PENDING' CHECK (
        status IN ('PENDING', 'APPROVED', 'REJECTED', 'EXPIRED')
    ),
    requested_at      timestamptz NOT NULL,
    expires_at        timestamptz NOT NULL,
    decided_at        timestamptz,
    decided_by        text,
    resource_version  bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, attempt_id, tool_name, tool_version, action, args_hash, resource),
    FOREIGN KEY (tenant_id, call_id) REFERENCES tool_calls (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, task_id) REFERENCES tasks (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, run_id) REFERENCES runs (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, attempt_id) REFERENCES attempts (tenant_id, id) ON DELETE RESTRICT
);
