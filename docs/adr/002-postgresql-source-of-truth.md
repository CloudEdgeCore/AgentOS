# ADR-002: PostgreSQL as Strong-Consistency Source of Truth

- Status: Accepted
- Date: 2026-08-13

## Context

Task, Run and Attempt state, runtime leases and fencing tokens, budget
reservations, approvals, outbox events and inbox deduplication must be updated
atomically across objects. No in-memory cache, message broker or document
store can express the cross-row invariants (one active Attempt per Run, CAS on
`resource_version`, result-before-success, idempotent settlements) that the
kernel depends on. The schema must also enforce tenant boundaries structurally.

## Decision

PostgreSQL is the strong-consistency source of truth for kernel state, and the
schema evolves through checksum-protected SQL migrations
(`db/migrations/000001`–`000006` as of this decision). Data access uses `pgx`
and generated `sqlc` queries; no heavy ORM that hides SQL or lock semantics is
allowed on core state paths.

Transaction rules:

- Default `READ COMMITTED`; state machines are expressed with
  `resource_version` compare-and-swap, targeted row locks (`FOR UPDATE`) and
  unique constraints. `SERIALIZABLE` is not used on the v0.1 hot paths:
  load testing measured ~94% conflict under 16-way concurrent completions
  (PostgreSQL SSI page-level predicates abort disjoint-row writers), which
  makes the retry budget impractical. Every invariant that ADR drafts
  assigned to SERIALIZABLE is instead enforced by row locks and constraints:
  budget settlement serializes on the ledger row lock, one-active-attempt
  placement on the partial unique index, completion finalization and
  recovery on the fenced owner row locks plus CAS.
- `beginSerializable` remains available for future cross-row invariants that
  row locks cannot express; any adoption must first measure the SSI conflict
  rate under the target concurrency and calibrate retry budgets.
- Work claiming uses `FOR UPDATE SKIP LOCKED` with an expiring owner lock and
  a monotonically increasing fencing token, never a bare "first writer wins"
  poll.
- Status transitions are centralized in single repository commands; API
  handlers only record intent (for example `cancel_requested_at`).

Tenancy and schema rules:

- Every tenant-scoped table has a `NOT NULL tenant_id`; unique keys include or
  validate the tenant boundary (for example `UNIQUE (tenant_id, id)`).
- Row-Level Security is defense-in-depth only; application-level tenant
  conditions remain mandatory, and `BYPASSRLS` roles are never used for
  business traffic.
- Migrations are expand → migrate/backfill → contract; no one-shot destructive
  changes. Migration checksums are enforced before application.
- Distributed SQL (YugabyteDB, CockroachDB) is not adopted at v0.1; it is
  re-evaluated only with real cross-region write, single-primary throughput or
  RTO/RPO evidence, and never on a "PostgreSQL wire compatible" assumption.

## Consequences

- Kernel invariants are machine-checked by the database in addition to tests.
- One database transaction can atomically cover state + outbox + receipt +
  audit rows, which is the foundation of ADR-003 and ADR-007.
- PostgreSQL becomes a scaling and operations focus: short transactions,
  partitioning, read-only projections and capacity testing precede any
  distributed-SQL discussion.
- Teams must write disciplined SQL; the ORM shortcut is closed by policy.
