# ADR-003: PostgreSQL Outbox and NATS JetStream

- Status: Accepted
- Date: 2026-08-13

## Context

Agent OS must atomically persist domain state and the fact that an event exists.
No distributed transaction is available between PostgreSQL and a message broker,
and runtime/controller recovery must not depend on an in-memory queue.

## Decision

PostgreSQL is the event source of truth. Every domain mutation inserts an outbox
row in the same transaction. Dispatchers claim rows with `FOR UPDATE SKIP LOCKED`,
an expiring owner lock and a monotonically increasing lock fencing token, publish
synchronously to JetStream, then acknowledge with the same owner and token. A
publish failure clears the claim and records bounded exponential backoff.
JetStream receives the outbox event UUID as `Nats-Msg-Id`.

Delivery is at-least-once. A process failure after the JetStream acknowledgement
and before the PostgreSQL acknowledgement can produce a duplicate. Consumers
must therefore use inbox deduplication and aggregate-version checks. JetStream
deduplication reduces duplicates but is not the correctness mechanism.

Subjects contain only controlled aggregate and event type tokens. Tenant IDs
remain in the versioned envelope and are never interpolated into subjects.
Payloads larger than 1 MiB must use an Artifact reference.

Only the earliest unpublished version of an aggregate is claimable. This
preserves per-aggregate ordering while allowing unrelated aggregates to publish
in parallel; no global ordering is promised.

## Consequences

- PostgreSQL recovery alone can reconstruct all unpublished work.
- Multiple dispatchers can operate concurrently without publishing the same
  currently claimed row.
- Duplicate and late messages remain part of the consumer contract.
- Broker retention and projection loss do not change canonical task state.
