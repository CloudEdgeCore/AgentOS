-- Migration 000034: Kernel-level Agent IPC Messages durable mailbox table.
CREATE TABLE IF NOT EXISTS agent_ipc_messages (
    id UUID PRIMARY KEY,
    tenant_id TEXT NOT NULL CHECK (tenant_id <> ''),
    namespace TEXT NOT NULL CHECK (namespace <> ''),
    sender TEXT NOT NULL CHECK (sender <> ''),
    receiver TEXT NOT NULL CHECK (receiver <> ''),
    message_type TEXT NOT NULL CHECK (message_type <> ''),
    correlation_id TEXT,
    payload BYTEA,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'DELIVERED', 'ACKED', 'EXPIRED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deadline TIMESTAMPTZ NOT NULL,
    acked_at TIMESTAMPTZ,
    trace_id TEXT
);

CREATE INDEX IF NOT EXISTS idx_agent_ipc_messages_receiver_status
    ON agent_ipc_messages (tenant_id, receiver, status);

CREATE INDEX IF NOT EXISTS idx_agent_ipc_messages_receiver_created
    ON agent_ipc_messages (tenant_id, receiver, created_at);

CREATE INDEX IF NOT EXISTS idx_agent_ipc_messages_deadline
    ON agent_ipc_messages (deadline)
    WHERE status = 'PENDING';

CREATE INDEX IF NOT EXISTS idx_agent_ipc_messages_correlation
    ON agent_ipc_messages (tenant_id, correlation_id)
    WHERE correlation_id IS NOT NULL;
