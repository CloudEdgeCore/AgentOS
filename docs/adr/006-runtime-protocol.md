# ADR-006: Fenced Runtime Protocol

- Status: Accepted
- Date: 2026-08-14

## Context

Scheduling currently ends after PostgreSQL commits an `AttemptPlaced` event and
an execution lease. Runtime workers need a versioned control boundary without
direct database access, and stale workers must be unable to mutate a Run after
lease loss or recovery.

## Decision

Kernel-to-Runtime control uses Protobuf and gRPC under
`agentos.runtime.v1alpha1`. Buf linting and generated Go bindings are checked
into the monorepo. Public REST schemas are not reused as internal RPC schemas.

A Runtime polls assignments for its configured instance, then reconciles each
assignment against PostgreSQL. Every mutation carries tenant, Attempt UUID,
fencing token and expected resource version. Checkpoint, completion and
cancellation operations additionally store durable idempotency records in the
same PostgreSQL transaction. Start transitions and heartbeats are effect-
idempotent through lifecycle validation and CAS; a caller that loses a response
must read the current assignment before retrying with its new version.

RPC messages are capped at 1 MiB. Results and Checkpoint state are immutable,
content-addressed Artifact references rather than embedded payloads. The
deterministic Go Provider is a conformance/fault-injection tool only. The Rust
Wasmtime Provider is a separate process, uses no Go/Rust FFI, forbids unsafe
Rust, and exposes no WASI capabilities unless a future RuntimeClass explicitly
adds them.

Development transport is plaintext only on an explicit loopback-only server
with a fixed tenant. It is not a production identity mode. Production exposure
remains blocked on SPIFFE mTLS and workload attestation.

## Consequences

- Runtime implementations can evolve independently of the public Control API.
- PostgreSQL resync makes assignment discovery recoverable even when an event
  notification is delayed or lost.
- Fencing and CAS remain authoritative; gRPC connectivity never grants
  ownership by itself.
- Protocol breaking checks become a required CI gate before v1alpha1 evolves.
