# ADR-007: Logical Checkpoint and Recovery Envelope

- Status: Accepted
- Date: 2026-08-14

## Context

Runtime process or node failure must not lose a Task or permit the old process
to publish a late result. Provider-native process and VM snapshots have narrow
version compatibility and cannot be the Agent OS portable recovery ABI.

## Decision

Agent OS v0.1 uses logical Checkpoints. The immutable envelope records the
AgentVersion reference, RuntimeClass, Provider, Runtime ABI, Checkpoint schema,
content-addressed state Artifact, confirmed side-effect receipt IDs, fencing
token and a canonical SHA-256 request/envelope digest.

Artifact bytes are written before their immutable reference is committed.
PostgreSQL owns Artifact metadata, Checkpoint ordering, compatibility metadata
and idempotency. Orphaned content is safe to garbage-collect; a database row may
never point at content that was not durably written. The local filesystem
adapter is development-only and uses the same `artifact://tenant/sha256/digest`
identity that a production S3-compatible adapter must preserve.

On lease expiry, one serializable transaction releases the old lease, marks the
old Attempt failed, increments the Run fencing token and either creates a new
placed Attempt or terminates the Run after `retryPolicy.maxAttempts`. The next
Runtime receives the latest Checkpoint and must reject incompatible Provider,
ABI, AgentVersion, schema, size or digest. A pending cancellation converges to
cancelled instead of being retried.

Final result registration, Attempt completion, Run completion, Task success,
lease release, outbox events and the completion idempotency receipt share one
transaction. The same atomic rule applies to Runtime cancellation
acknowledgement.

## Consequences

- Recovery is portable at the logical state boundary without promising
  transparent arbitrary-process snapshots.
- Old Runtime writes fail after a higher fencing token is issued.
- Confirmed external side effects can be carried across retries without
  claiming external exactly-once delivery.
- Provider-private snapshots may be added later but cannot replace or weaken
  the Kernel envelope checks.
