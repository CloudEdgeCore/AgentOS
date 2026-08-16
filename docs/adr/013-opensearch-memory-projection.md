# ADR-013: OpenSearch Memory Projection with Tombstone Deletion Propagation

- Status: Accepted
- Date: 2026-08-16

## Context

The canonical Memory store (ADR-009) lives in PostgreSQL with FTS, pg_trgm
and pgvector. It is the source of truth for retrieval in the control plane,
but long-tail, cross-record full-text search and analytics are better served
by a dedicated search engine. The projection must never drift from the
canonical store: every durable write must flow into the index, and every
deletion must propagate out of it — a deleted memory that still surfaces in
search is a data-retention violation.

## Decision

The v0.3 Secure Runtime adds an OpenSearch search projection fed by the
existing outbox pipeline:

- The canonical store emits projection events in the same transaction as the
  write: `MemoryUpserted` on every durable put (new record or corrected
  version; idempotent replays emit nothing) and `MemoryTombstoned` on every
  CAS tombstone. Events carry identity and resource version, never content.
- `cmd/agentos-outbox` dispatches the events to JetStream
  (`agentos.events.memory.>`), exactly as for the control-plane events.
- `cmd/agentos-projector` consumes them with a durable, explicit-ack
  JetStream consumer and applies them to the OpenSearch index
  (`agentos-memory`, one index, documents scoped `tenantId/memoryId`):
  - `MemoryUpserted` fetches the canonical record and indexes it; a stale
    event (projected version already at or above the event version) is
    skipped, a missing record is dropped, and a record whose tombstone won is
    deleted instead of resurrected.
  - `MemoryTombstoned` deletes the projected document — tombstone deletion
    propagation. Deleting a missing document is idempotent.
- Application failures are Nak'd with backoff and redelivered; malformed
  envelopes are acked and dropped. The projector is exactly-once-apply by
  construction (version idempotency), so replays are safe.
- The index carries text and identity fields; embeddings stay in the
  canonical pgvector store (vector search remains PostgreSQL's job in v0.3).
- The dev compose file runs OpenSearch under the `search` profile
  (`docker compose --profile search up -d`, `http://127.0.0.1:39200`,
  security disabled for dev).

## Consequences

- The search projection is eventual but convergent: any replay of the event
  log reproduces the index state, and tombstones are never resurrected.
- Deletion is durable at the canonical store and propagates asynchronously;
  a short window exists between tombstone and index deletion, documented as
  the projection lag (seconds under normal operation).
- OpenSearch is an external dependency of the projector only; the control
  plane remains fully functional without it.
- Additional projections (tool calls, approvals, audit) can reuse the same
  consumer pattern on their own filters; memory is the v0.3 reference
  projection.
