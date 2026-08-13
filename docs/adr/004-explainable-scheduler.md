# ADR-004: Fenced Two-Stage Scheduling

- Status: Accepted
- Date: 2026-08-13

## Context

Agent placement depends on AI and business resources that Kubernetes does not
model: token/cost limits, LLM concurrency, data residency, runtime isolation and
Artifact locality. Kernel controllers must also recover safely after crashes and
must not allow an expired controller to commit a decision.

## Decision

Admission and placement are separate reconciliation stages:

1. Admission validates the bounded workload specification and records an
   append-only decision with stable reason codes.
2. Scheduling hard-filters runtime pools for readiness, runtime class, region,
   residency and capacity, then applies deterministic explainable scores for
   preferred class, locality, headroom and cost.

Controllers claim tasks using PostgreSQL leases with controller kind, owner,
expiry and monotonically increasing fencing token. A stale controller cannot
commit. Scheduling commits Task `RUNNING`, Run, Attempt, RuntimeLease and outbox
events in one serializable transaction. Equal scores are resolved by stable pool
ID ordering. When no pool fits, the short claim is allowed to expire to avoid a
hot retry loop.

Kubernetes remains responsible for Pod/Node placement. Agent OS selects a
runtime pool and instance; it does not store core Task/Run/Attempt state as CRDs.

## Consequences

- Every rejection and placement can be explained without model inference.
- Controller restarts and split ownership are safe under the database contract.
- Static development pools can later be replaced by a live capacity registry
  without changing the scoring or persistence interfaces.
- Quota reservation, fairness and live capacity accounting remain required
  extensions before production scheduling.
