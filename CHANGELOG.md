# Changelog

All notable changes use [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
categories. AgentOS product version `1.0.0.0` maps to SemVer tag `v1.0.0`;
public protocol versions evolve independently.

## Unreleased

### Added

- Tokenizer plugin registry: deployments can register exact per-provider tokenizers (`tokens.Register`) under provider-config names; built-in `heuristic`/`conservative` names are reserved and duplicates are rejected. The conservative-reservation-envelope + exact-provider-settlement product semantics stay unchanged.
- Public error-code dispositions: every stable error code in the canonical registry now carries exactly one public class (`retryable`, `terminal`, `user-action-required`, `operator-action-required`) so callers can pick a retry policy without parsing messages.
- Workflow budget reservations close the spawn-time commitment loop: declaring or dynamically spawning a step reserves its future Task's token/cost ceiling and task slot on the workflow usage ledger in the same transaction, transfers to the task's own budget ledger at admission, and is released on skip, cancellation, rejection, or terminal tasks without a ledger. Concurrent spawns can no longer collectively promise past a workflow budget, retries re-reserve under the same guard, and the ledger reconciles against per-step reservations after crashes.
- Runtime pool operator grants: cordon/drain/activate now requires a separate `runtime_pool_operator_grants` row for the requesting subject; tenant usage grants no longer imply operator authority on shared pools, and status changes record the deciding operator subject in the audit chain.
- Runtime bindings decouple deployment endpoints from immutable AgentVersions: `agentos init` writes an environment-independent `agentos-binding://<name>/remote` entrypoint by default, and `agentos-runtime-adapter -runtime-bindings` maps version refs (or `name@*` wildcards) to concrete Runtime Interface endpoints. Unresolved logical entrypoints fail closed.
- Runtime Interface streaming extension: `GET /executions/{id}/events/stream` serves one long-lived SSE connection that pushes events after the `after` cursor and terminates with a result frame. The Go and Python hosts implement it, the Go client consumes it with automatic fallback to v1 polling for runtimes without the route, and the adapter worker uses streaming-first observation with mid-flight fallback.
- Tokenizer-aware token estimation: provider configs accept a `tokenizer` field (`heuristic` by default, `conservative` for uncharacterized tokenizers). The estimator is script-aware (dense CJK/kana/hangul text no longer under-reserves against bytes/4), never estimates below the legacy floor, and provider-reported usage remains the authoritative settlement.
- A durable runtime-pool registry with tenant grants.
- Workflow CLI operations and one-command Runtime Interface conformance.
- A typed TypeScript Control API and Runtime Interface SDK.
- Python RuntimeHost watchdogs, bounded concurrency and payloads, unit tests, and PEP 561 metadata.
- Scheduled scale, fuzz, live-model, real-KVM, and 24-hour soak evidence workflows.
- Multi-language release SBOMs and SLSA build provenance attestations.
- Typed workflow output contracts with JSON Schema validation and RFC 6901 conditions.
- Logical-model routing independent of provider endpoint and wire-model selection.

### Changed

- Model tool declarations now cross the wire in the OpenAI-compatible function-tool shape (`{"type":"function","function":{…}}`); strict providers (vLLM/Qwen/DeepSeek/GLM/OpenAI) no longer risk rejecting or ignoring flat tool objects. The internal `ToolDefinition` stays flat.
- Nightly live-model acceptance runs `TestRealModelToolCalling` in addition to the v1.1 real-model suite, so the nightly log proves the model→tool→model closed loop.
- MCP tool idempotency keys bind the resolved tool version: the same arguments against two versions of one tool never share replay semantics.
- Tool discovery shows exactly one entry per tool name — the latest granted version, which is what a bare-name invocation resolves to — while explicit `name@version` pins still reach older granted versions.
- Runtime Interface HTTP client caching includes the binding's TLS material fingerprint: bindings sharing an endpoint and SNI but carrying different certificates resolve to distinct clients.
- Scheduling uses effective capacity: pool listing subtracts the durable active reservation ledger from declared totals, placement hard filters and headroom scores use the result, and pools without a registered capacity ledger fail placement closed.
- Capacity races fall back: the scheduler walks the full ranked candidate list in one reconcile pass and defers with per-candidate diagnostics only after every candidate lost the transactional reservation.
- `ScheduleTask` never writes pool total capacity; the runtime pool registry is the single authoritative writer (with a shrink-below-active-reservation guard), and placement on an unregistered capacity ledger fails closed.
- Workflow idempotency scope now includes the namespace (`tenant_id + namespace + idempotency_key`), matching the task scope; the same key under different namespaces creates independent workflows.
- MCP execution identity is mandatory in production and idempotency hashes use full SHA-256 values.
- The runtime adapter's sandbox MCP listener is loopback-only in every mode, including fully configured production mTLS.
- Model token and cost guards account for streaming tool calls, ambiguous outcomes, and outstanding reservations.
- Failed model calls record known-zero, known, or unknown usage instead of treating uncertainty as free usage.
- Provider, tool, Runtime Adapter, and artifact boundaries share credential redaction rules.
- Runtime Adapter polling is adaptive and immutable AgentVersion decoding is forward-compatible.
- Workflow approval decisions preserve both the deciding principal and the decision.
- The Python RuntimeHost releases a stuck execution's ledger capacity after the termination grace and documents its protocol-host (non-isolation) boundary.

### Security

- Remote production runtime bindings must present a mutual-TLS client certificate identity; loopback endpoints and deployments that explicitly acknowledge the development policy are exempt. Server verification alone no longer satisfies the production policy for remote Runtime Interface endpoints.
- Tenant-owned mutations and lineage foreign keys are tenant-scoped.
- System tool namespaces are reserved, capability wildcard behavior is unified, and memory sensitivity is authorization-enforced.
- Remote Runtime Adapter control traffic requires SPIFFE/mTLS outside explicit loopback development mode.

### Fixed

- Real model→tool→model closed loop: the runtime adapter's gRPC model broker dropped the agent-resolved tool definitions from `InvokeRequest.tools`, so providers never saw the tool surface and answered from parametric memory; and assistant turns serialized tool calls as flat `{id,name,arguments}` instead of the OpenAI-compatible `{"id","type":"function","function":{…}}` envelope, so strict providers rejected the follow-up turn carrying tool results. Both surfaces now convert at the wire boundary only; regression tests pin the mapping. Verified end-to-end against a live vLLM endpoint (`TestRealModelToolCalling`: model calls → webhook tool execution → grounded answer with exact usage settlement).

## 1.0.0 - 2026-08-21

### Added

- GA AgentOS control plane, task kernel, scheduling, recovery, runtime providers, governed gateways, stable v1 contracts, audit ledger, and signed release pipeline.
