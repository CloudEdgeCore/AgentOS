# ADR-008: Embedded Rego Policy Engine

- Status: Accepted
- Date: 2026-08-15

## Context

Permission and admission decisions must be enforced outside the LLM and
outside per-call application code, with default-deny semantics, machine-readable
outcomes and a recorded policy revision. Inline Go `if` chains scale poorly
once policy becomes attribute- or tenant-driven, and they cannot be updated
without redeploying the kernel binary.

## Decision

Admission and future Gateway decisions evaluate Rego v1 policies with the OPA
Go library embedded in the kernel process. The decision input is a versioned,
typed document (`principal`, `action`, `resource`, `context`) constructed by
trusted kernel code — never raw request JSON. Policies are compiled into the
binary for v0.1 as an embedded module set with a pinned revision; signed
external bundles become the delivery mechanism in a later slice, at which
point revision, freshness and fail-closed loading follow the existing bundle
contract.

The engine queries `data.agentos.policy.allow` and
`data.agentos.policy.deny_reasons` together. Default deny applies when no rule
matches, when evaluation errors, or when the tenant has no policy data entry.
Every decision is recorded in the append-only admission decision ledger
together with the policy revision and the hit rule names, so outcomes stay
explainable without model inference.

Admission chains the Go limits engine and the Rego engine: both must allow.
Each engine owns disjoint checks — the Rego engine owns tenant-attribute rules
(such as a per-tenant maximum priority), while the Go engine keeps the bounded
workload-spec checks — so no check has two sources of truth.

## Consequences

- Default-deny is structural: a missing or broken policy rejects the task.
- Policy changes are data-driven and testable with `opa test`-style fixtures.
- The OPA module adds a significant dependency tree to the kernel binary;
  evaluation stays in-process so decisions do not depend on a remote hop.
- WASM-compiled policy evaluation and signed bundle delivery are deferred
  until the Gateway slice needs multi-service policy distribution.
