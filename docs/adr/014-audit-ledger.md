# ADR-014: Transactional Audit Ledger with Hash Chain and Signed Export

- Status: Accepted
- Date: 2026-08-16

## Context

The engineering baseline (§23) flags "Audit 被普通日志替代" as a top risk and
mandates its mitigation: transactional append, hash chain, signed WORM
archive, and independent access policy. Ordinary logs are mutable, lossy and
not tenant-scoped; compliance and incident response need a ledger that proves
which kernel decisions happened, in what order, and that nobody edited the
record afterwards — including the operator.

## Decision

The v0.4 audit ledger is a PostgreSQL table appended inside the same
transaction as every kernel decision (ADR-014):

- `audit_events`: per-tenant monotonically increasing `seq`, the previous
  event's `chain_hash` as `prev_hash`, the event's own canonical `chain_hash`
  (SHA-256 over a length-prefixed encoding of id, seq, prev_hash, event
  type, resource type/id, actor, canonicalized details, and UTC occurrence),
  plus the decision details as jsonb.
- Appends are serialized per tenant with a transactional advisory lock, so
  concurrent decisions produce dense, race-free sequences from the first
  event; the first event's `prev_hash` is a fixed genesis constant, making
  head truncation detectable.
- The ledger is append-only at the database level: an `BEFORE UPDATE OR
  DELETE` trigger rejects any mutation.
- Every append also emits an `AuditRecorded` outbox event (aggregate
  `Audit`, version = seq) so the search projection consumes the same
  pipeline as memory (ADR-013).
- Hooks cover the kernel decision surface: task queued/transitioned/
  admitted/rejected/scheduled/cancelled, agent version published, memory
  upserted/tombstoned, approval requested/decided/expired, tool call
  decisions, attempt acquired/transitioned, checkpoint committed, attempt
  completed/recovered, cancellation acknowledged.
- Control API (tenant-scoped): `GET /v1/audit` (cursor-paged), `GET
  /v1/audit/verify` (full chain walk: link + hash integrity, first broken
  seq), `GET /v1/audit/export` (full ledger; signed with the control plane's
  ed25519 key over the canonical payload when `-audit-signing-key` is
  configured — the dev mode without a key exports unsigned with a warning).

## Consequences

- Tampering is detectable at the exact broken link: changing an event's
  content breaks its own chain hash, and deleting an event breaks the next
  link; `verify` reports the first broken seq.
- The ledger is durable and transactional by construction: a decision that
  committed has its audit record committed with it, and a rolled-back
  decision leaves none.
- Verification is O(n) per tenant; the API paginates reads and the verify
  walk is explicit, so cost is opt-in.
- The audit key is the compliance trust anchor: production must keep it in a
  secret manager and export signed archives to WORM storage; the key itself
  is never stored in the ledger. Key rotation and cross-tenant export
  aggregation are projections.
