# Changelog

All notable changes use [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
categories. AgentOS product version `1.0.0.0` maps to SemVer tag `v1.0.0`;
public protocol versions evolve independently.

## Unreleased

### Added

- Atomic multi-dimensional runtime-pool reservations and durable scheduling rejection diagnostics.
- Explicit Task-to-Workflow lineage, workflow claim renewal, reserved-usage budget guards, and complete workflow lifecycle evidence.
- Fixed-point micro-USD accounting with reconciliation and repair of derivable ledger drift.
- A durable runtime-pool registry with tenant grants.
- Workflow CLI operations and one-command Runtime Interface conformance.
- A typed TypeScript Control API and Runtime Interface SDK.
- Python RuntimeHost watchdogs, bounded concurrency and payloads, unit tests, and PEP 561 metadata.
- Scheduled scale, fuzz, live-model, real-KVM, and 24-hour soak evidence workflows.
- Multi-language release SBOMs and SLSA build provenance attestations.
- Typed workflow output contracts with JSON Schema validation and RFC 6901 conditions.
- Logical-model routing independent of provider endpoint and wire-model selection.

### Changed

- MCP execution identity is mandatory in production and idempotency hashes use full SHA-256 values.
- Model token and cost guards account for streaming tool calls, ambiguous outcomes, and outstanding reservations.
- Failed model calls record known-zero, known, or unknown usage instead of treating uncertainty as free usage.
- Provider, tool, Runtime Adapter, and artifact boundaries share credential redaction rules.
- Runtime Adapter polling is adaptive and immutable AgentVersion decoding is forward-compatible.
- Workflow approval decisions preserve both the deciding principal and the decision.

### Security

- Tenant-owned mutations and lineage foreign keys are tenant-scoped.
- System tool namespaces are reserved, capability wildcard behavior is unified, and memory sensitivity is authorization-enforced.
- Remote Runtime Adapter control traffic requires SPIFFE/mTLS outside explicit loopback development mode.

## 1.0.0 - 2026-08-21

### Added

- GA AgentOS control plane, task kernel, scheduling, recovery, runtime providers, governed gateways, stable v1 contracts, audit ledger, and signed release pipeline.
