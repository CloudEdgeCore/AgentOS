# Agent OS

Agent OS is a system software layer for securely publishing, scheduling, executing, recovering, governing, and auditing heterogeneous AI agents.

The project is in its v0.6 Multi-tenant Scale & Capacity Evidence phase. The
v0.1 kernel establishes the invariants that every later runtime provider and
gateway must preserve — persistent `Task`/`Run`/`Attempt` lifecycles, an
immutable `AgentVersion` registry, budget ledgers, lease/fencing tokens,
durable outbox messaging, tenant-scoped storage, result-before-success
ordering, fenced Admission and Scheduler reconciliation, a bounded
Protobuf/gRPC Runtime Protocol, content-addressed Artifacts with logical
Checkpoint envelopes, lease-expiry recovery, durable cancellation, and
deterministic Go + capability-free Rust/Wasmtime runtime providers.

The v0.3 Secure Runtime closed the identity, supply-chain, secret and
projection boundaries ([`docs/v0.3-secure-runtime.md`](docs/v0.3-secure-runtime.md)):
SPIFFE X.509-SVID mTLS, signed Agent Packages with fail-closed admission,
an OpenBao Secret Broker, an OpenSearch memory projection with tombstone
deletion propagation, a security negative test suite, and Firecracker
documented as an explicit boundary.

The v0.4 Production Hardening phase ([`docs/v0.4-production-hardening.md`](docs/v0.4-production-hardening.md))
added the operations surface: a transactional hash-chained audit ledger with
signed exports, an append-only audit projection, OpenBao dynamic database
credentials with full lease lifecycle, controller hardening (per-task error
isolation, route budgets, stream caps), and OCI image digest pinning.

The v0.5 Runtime Hardening & Scale-Readiness phase (delivery notes in
[`docs/v0.5-runtime-hardening.md`](docs/v0.5-runtime-hardening.md)) completes
the OCI/gVisor checklist code surface and the scale-readiness items:

- container-class resource limits enforced end-to-end (spec → admission →
  worker → runsc flags), zero values never admitted;
- orphan container reaping at Prepare time and bounded stdout spooling into
  the artifact store;
- cosign-style image signatures and CycloneDX SBOM generation/verification
  in the signed Agent Package pipeline;
- artifact store aggregate quotas and retention garbage collection;
- parallel Reconcile with bounded worker sets in all three controllers.

The v0.6 Multi-tenant Scale & Capacity Evidence phase (delivery notes in
[`docs/v0.6-multi-tenant-scale.md`](docs/v0.6-multi-tenant-scale.md)) adds
the multi-tenant scaling surface and its capacity evidence:

- per-tenant aggregate consumption quotas: windowed token/cost/tool-call/
  wall-time limits settled exactly from the task settlement stream, with an
  atomic admission gate (`TENANT_QUOTA_EXCEEDED`) and a `GET/PUT/DELETE
  /v1/quota` control API scoped to the principal tenant;
- runtime-aware pool health: placement derives pool readiness from lease
  heartbeat freshness, so pools whose worker stopped renewing are rejected
  (`POOL_NOT_READY`) instead of stranding attempts;
- O6 scheduling backoff: a task no pool can place releases its claim
  immediately and defers the next attempt with exponential backoff recorded
  on the task (shared across controller instances);
- dual-instance controller and outbox concurrency regression tests proving
  zero re-processing and zero dropped events under real Postgres contention;
- a repeatable 1,000-task control-plane pipeline capacity baseline
  (enqueue → admit → schedule → complete) reporting per-phase throughput and
  latency percentiles as the evidence for v0.6+ middleware decisions.

The complete architecture and technology baseline live in [`docs/`](./docs/).

## Development

Requirements:

- Go 1.26.x;
- Rust 1.97.1 for the Wasmtime Provider;
- PostgreSQL 18 with the pgvector extension for integration tests
  (the dev compose and CI use the `pgvector/pgvector:pg18` image);
- Docker for local PostgreSQL and NATS integration services.

Run unit tests:

```shell
go test ./...
```

Run PostgreSQL integration tests:

```powershell
docker compose -f deploy/dev/compose.yaml up -d --wait
$env:AGENTOS_TEST_DATABASE_URL = "postgres://agentos:agentos-dev-only@127.0.0.1:55432/agentos?sslmode=disable"
$env:AGENTOS_TEST_NATS_URL = "nats://127.0.0.1:54222"
go test -tags=integration -count=1 ./...
```

Apply migrations:

```powershell
go run ./cmd/agentos-migrate -database-url $env:DATABASE_URL
```

Run the local control-plane processes after applying migrations:

```powershell
$databaseUrl = "postgres://agentos:agentos-dev-only@127.0.0.1:55432/agentos?sslmode=disable"
go run ./cmd/agentos-control -database-url $databaseUrl -dev-tenant dev
go run ./cmd/agentos-controller -database-url $databaseUrl -controller-id dev-controller -runtime-pools deploy/dev/runtime-pools.json -tenant-policies deploy/dev/tenant-policies.json -dev-mode
go run ./cmd/agentos-outbox -database-url $databaseUrl -nats-url nats://127.0.0.1:54222 -dispatcher-id dev-outbox
go run ./cmd/agentos-runtime-control -database-url $databaseUrl -listen 127.0.0.1:9090 -dev-tenant dev -dev-mode
go run ./cmd/agentos-runtime-reference -control-address 127.0.0.1:9090 -gateway-address 127.0.0.1:9091 -mcp-listen 127.0.0.1:9092 -tenant dev -runtime-instance-id dev-worker-1 -artifact-root tmp/artifacts -dev-mode
go run ./cmd/agentos-gateway -database-url $databaseUrl -tenant-policies deploy/dev/tenant-policies.json -tenant dev -seed-dev-tools -dev-mode
```

The control API authenticates in one of two modes (exactly one required):

- Development: `-dev-tenant dev` (static identity, loopback listener only).
- Verified identity: `-oidc-issuer https://idp.example -oidc-client-id agentos-control
  -oidc-tenant-claim tenant` (OIDC ID tokens; the token's `sub` becomes the
  principal and the tenant claim scopes every store query).

The Memory API (ADR-009) is served by the control API:

```powershell
# write a memory (idempotent by namespace+key; corrections bump the version)
curl -X POST http://127.0.0.1:8080/v1/memories -H "Content-Type: application/json" `
  -H "Idempotency-Key: mem-1" -d '{"namespace":"default","key":"facts","contentType":"text/plain","content":"..."}'
# hybrid search: FTS + trigram + optional embedding
curl "http://127.0.0.1:8080/v1/memories?query=keyword&namespace=default&limit=10"
# soft delete (CAS tombstone, deletion intent survives)
curl -X DELETE http://127.0.0.1:8080/v1/memories/<id> -H "If-Match: `"W/`"1`""
```

The reference observability stack (tech baseline §15) is opt-in:

```powershell
docker compose --profile observability up -d
$env:OTEL_EXPORTER_OTLP_ENDPOINT = "127.0.0.1:4317"   # traces/metrics/logs -> OTLP
# Grafana: http://127.0.0.1:3300 (Prometheus/Tempo/Loki datasources provisioned)
```

The v0.3 Secure Runtime services are opt-in as well:

```powershell
docker compose --profile secrets up -d   # OpenBao Secret Broker (ADR-012)
docker compose --profile search up -d    # OpenSearch projection target (ADR-013)
go run ./cmd/agentos-svid -out tmp/svid -tenant dev -worker dev-worker-1   # SVID material (ADR-011)
go run ./cmd/agentos-pkg genkey -id ci-builder-1                          # package signing key (ADR-010)
go run ./cmd/agentos-projector -database-url $dbUrl -nats-url nats://127.0.0.1:54222 `
  -opensearch-addr http://127.0.0.1:39200                                # memory projection
# gateway with the Secret Broker:
go run ./cmd/agentos-gateway ... -bao-addr http://127.0.0.1:58200 -bao-token bao-dev-only
# runtime-control with the identity boundary (see docs/v0.3-secure-runtime.md for full flags)
```

The reference Runtime exposes a loopback MCP endpoint (`-mcp-listen`) while an
assignment executes: a sandboxed Agent can call the tenant's tools through the
standard Model Context Protocol, with the fenced attempt identity injected by
the worker and default-deny outside execution windows.

The reference Runtime is deterministic and not a security sandbox. Build and
verify the actual Wasmtime Provider with the pinned Rust toolchain:

```shell
cargo fmt --all -- --check
cargo clippy --workspace --all-targets --locked -- -D warnings
cargo test --workspace --locked
```

The Wasmtime Provider accepts only components beneath its configured package
root, exposes no WASI imports, and enforces memory, fuel and epoch-interruption
limits. Its development gRPC transport must not be exposed outside loopback.

Run the dual-provider conformance suite (the same published AgentVersion on
the Go reference provider and the Wasmtime provider):

```powershell
cargo run --bin component-fixture -- --out tmp/conformance/agent.wasm
$env:AGENTOS_WASMTIME_RUNTIME = "target\debug\agentos-runtime-wasm.exe"
$env:AGENTOS_WASMTIME_PACKAGE_ROOT = "tmp\conformance"
go test -tags=integration -run TestSameAgentVersionRunsOnBothProviders -count=1 ./internal/runtime/control/...
```

The conformance test skips the Wasmtime leg when
`AGENTOS_WASMTIME_RUNTIME` and `AGENTOS_WASMTIME_PACKAGE_ROOT` are unset; CI
builds the provider and component in the same job and always runs both legs.

The development API intentionally accepts only a fixed tenant identity and
refuses non-loopback listeners. Verified OIDC/workload identity middleware is
required before a non-development deployment.

The integration suite applies checksum-protected migrations and clears only
kernel test tables before each test. Never point `AGENTOS_TEST_DATABASE_URL`
at a database containing durable data. Linux CI additionally runs all unit and
integration tests with the Go race detector.

## Status

No compatibility guarantees apply before the first `v1alpha1` API release. Correctness and security invariants are expected to remain stable even while schemas evolve.
