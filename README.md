# AgentOS

AgentOS is a control and runtime platform for securely publishing, scheduling, executing, recovering, governing, and auditing AI agents.

> **Current release: AgentOS 1.0.0.0 (GA)**
>
> SemVer / Git tag: [`v1.0.0`](https://github.com/CloudEdgeCore/AgentOS/releases/tag/v1.0.0)
>
> Stable contracts: Control API v1, Agent Manifest v1, Runtime / Gateway / Model Protocol v1, and Runtime Interface v1

AgentOS is not a chat UI, a visual workflow builder, or a managed SaaS product. It addresses the backend systems problems that appear when agents move into production: immutable versions, durable tasks, admission policy, hard budget limits, runtime isolation, multi-tenant identity, tool and model gateways, checkpoints, failure recovery, and auditability.

## Execution lifecycle

```mermaid
flowchart LR
    A["Agent Manifest / Signed Package"] --> B["Control API"]
    B --> C["PostgreSQL durable state"]
    C --> D["Admission / Policy / Budget"]
    D --> E["Scheduler / Recovery"]
    E --> F["Runtime Protocol v1"]
    F --> G["Wasmtime"]
    F --> H["OCI / gVisor"]
    F --> I["Reference / Adapter"]
    G --> J["Tool / Model / Memory Gateway"]
    H --> J
    I --> J
    J --> C
    C --> K["Checkpoint / Result / Audit"]
```

A typical task moves through the following lifecycle:

1. A developer submits a stable Agent Manifest and, optionally, a signed Agent Package.
2. The Control API creates an immutable `AgentVersion` and a durable `Task`.
3. Admission validates the version, capabilities, policy, tenant quota, and task budget.
4. The Scheduler selects a provider using runtime class, region, capacity, and lease health.
5. A worker receives a fenced assignment through Runtime Protocol v1.
6. The agent executes in Wasmtime, OCI/gVisor, or a co-located adapter runtime.
7. Model, tool, memory, and secret operations pass through gateways for authorization, metering, and audit.
8. Checkpoints, results, and side-effect receipts are persisted before the task reaches a terminal state.
9. If a worker disappears, a lease expires, or a process restarts, the Recovery Controller converges and reschedules the workload.

## What v1.0 provides

| Area | Implemented capability |
| --- | --- |
| Agent lifecycle | Stable manifests, immutable AgentVersions, signed packages, publishing, and version lookup |
| Task kernel | `Task → Run → Attempt` state machine, cancellation, retry, timeout, SSE event streams, and durable results |
| Scheduling and recovery | Admission, default-deny Rego policy, capacity-aware placement, leases, fencing tokens, backoff, and orphan recovery |
| Runtimes | Wasmtime/Wasm provider, OCI provider, Linux gVisor isolation, reference provider, and HTTP adapter worker |
| Agent SDKs | Go Runtime Interface SDK, Python 3.11+ Runtime Interface SDK, LangGraph adapter, and A2A adapter |
| Gateways | Tool, model, memory, and capability gateways with approval, idempotent receipts, budget settlement, and fail-closed behavior |
| Multi-tenancy | Tenant-scoped storage, OIDC principals, SPIFFE X.509-SVIDs, mTLS identity, and tenant binding |
| Secrets and supply chain | OpenBao secret broker, dynamic database credentials, package signing, OCI digest pinning, and SBOM validation |
| Data and events | PostgreSQL/pgvector, NATS JetStream, transactional outbox/inbox, and optional OpenSearch projection |
| Operations | OpenTelemetry, `/healthz`, `/readyz`, `/versionz`, hash-chained audit records, and signed audit exports |
| Quality gates | Race detector, real PostgreSQL/NATS integration tests, dual-provider conformance, and Go/Rust vulnerability audits |

## Stable contracts and compatibility

| Contract | Stable version | Source |
| --- | --- | --- |
| Control REST API | `v1` | [`api/openapi/control-v1.yaml`](api/openapi/control-v1.yaml) |
| Agent Manifest | `agentos.dev/v1` | [`internal/kernel/agentversion/manifest.go`](internal/kernel/agentversion/manifest.go) |
| Runtime Protocol | `agentos.runtime.v1` | [`proto/agentos/runtime/v1/runtime.proto`](proto/agentos/runtime/v1/runtime.proto) |
| Runtime Interface | `agentos.runtime.interface/v1` | [`api/openapi/runtime-interface-v1.yaml`](api/openapi/runtime-interface-v1.yaml) |
| Gateway Protocol | `agentos.gateway.v1` | [`proto/agentos/gateway/v1/gateway.proto`](proto/agentos/gateway/v1/gateway.proto) |
| Model Protocol | `agentos.model.v1` | [`proto/agentos/model/v1/model.proto`](proto/agentos/model/v1/model.proto) |
| SLO contract | `agentos.slo/v1` | [`api/slo/v1.json`](api/slo/v1.json) |

`v1alpha1` is the N-1 compatibility level for v1.0. Legacy manifests remain readable and can be promoted deterministically, while legacy gRPC service names remain available as wire-compatible aliases. The compatibility window will not close before **2027-02-17**. Breaking changes to stable contracts require a new version, and unknown fields continue to fail closed. The machine-readable policy is stored in [`api/compatibility/v1alpha1-to-v1.json`](api/compatibility/v1alpha1-to-v1.json).

Promote a legacy manifest to v1:

```shell
go run ./cmd/agentos migrate \
  -manifest agent.v1alpha1.json \
  -out agent.v1.json
```

## Quick start

### Requirements

- Go `1.26.x`; CI and official releases use `1.26.6`.
- Python `3.11+` when developing Python or framework adapters.
- Rust `1.97.1` when building the Wasmtime provider.
- Docker and Docker Compose for local PostgreSQL, NATS, and optional infrastructure.
- Linux, containerd, and runsc for real OCI/gVisor provider validation.

Inspect the source release identity:

```shell
go run ./cmd/agentos version -json
```

Build every Go command:

```shell
go build ./cmd/...
```

The official [`v1.0.0` release](https://github.com/CloudEdgeCore/AgentOS/releases/tag/v1.0.0) contains complete command archives for Linux AMD64/ARM64, macOS AMD64/ARM64, and Windows AMD64, plus a Python wheel.

### Create and certify a Python agent runtime

The following workflow starts a local Runtime Interface and certifies its public behavior without starting the control plane:

```shell
python -m pip install -e ./sdk/python
go run ./cmd/agentos init -dir ./tmp/hello-agent -name hello-agent -adapter python
python ./tmp/hello-agent/server.py
```

In another terminal, run:

```shell
go run ./cmd/agentos-conformance -endpoint http://127.0.0.1:8088
```

A successful run emits an `agentos.conformance/v1` JSON report with `passed` set to `true`. `agentos init` also supports `go`, `langgraph`, and `a2a` adapters.

### Start local control-plane infrastructure

```powershell
docker compose -f deploy/dev/compose.yaml up -d --wait postgres nats

$env:DATABASE_URL = "postgres://agentos:agentos-dev-only@127.0.0.1:55432/agentos?sslmode=disable"
go run ./cmd/agentos-migrate -database-url $env:DATABASE_URL
```

For local development, run each process below in its own terminal:

```powershell
# HTTP Control API
go run ./cmd/agentos-control `
  -database-url $env:DATABASE_URL `
  -dev-tenant dev

# Admission, Scheduler, and Recovery
go run ./cmd/agentos-controller `
  -database-url $env:DATABASE_URL `
  -controller-id dev-controller `
  -runtime-pools deploy/dev/runtime-pools.json `
  -tenant-policies deploy/dev/tenant-policies.json `
  -dev-mode

# Transactional outbox to NATS JetStream
go run ./cmd/agentos-outbox `
  -database-url $env:DATABASE_URL `
  -nats-url nats://127.0.0.1:54222 `
  -dispatcher-id dev-outbox

# Worker Runtime Protocol
go run ./cmd/agentos-runtime-control `
  -database-url $env:DATABASE_URL `
  -listen 127.0.0.1:9090 `
  -dev-tenant dev `
  -dev-mode

# Tool, Model, and Memory Gateway
go run ./cmd/agentos-gateway `
  -database-url $env:DATABASE_URL `
  -listen 127.0.0.1:9091 `
  -tenant-policies deploy/dev/tenant-policies.json `
  -tenant dev `
  -seed-dev-tools `
  -dev-mode

# Non-sandboxed reference provider for development and deterministic tests
go run ./cmd/agentos-runtime-reference `
  -control-address 127.0.0.1:9090 `
  -gateway-address 127.0.0.1:9091 `
  -model-gateway-address 127.0.0.1:9091 `
  -mcp-listen 127.0.0.1:9092 `
  -tenant dev `
  -runtime-instance-id dev-worker-1 `
  -artifact-root tmp/artifacts `
  -dev-mode
```

These commands use a fixed development tenant, loopback plaintext connections, and development executors. They are only safe for local development. Production mode rejects these downgraded settings.

### CLI workflows

The main CLI exposes the following stable workflows:

```text
agentos version   Print product, build, and protocol versions
agentos init      Create a Go/Python/LangGraph/A2A agent project
agentos migrate   Promote a legacy manifest to v1
agentos validate  Strictly validate an Agent Manifest
agentos package   Generate a package manifest with provenance
agentos sign      Sign a package with an Ed25519 key
agentos publish   Publish an immutable AgentVersion
agentos run       Submit a durable task
agentos logs      Stream task events over SSE
```

`publish`, `run`, and `logs` use `http://127.0.0.1:8080` by default. In production, pass the HTTPS Control API through `-endpoint` and provide a bearer token through `AGENTOS_TOKEN`.

## Production security baseline

AgentOS production mode is fail-closed. Processes refuse to start when required security configuration is missing.

- **Control API:** requires HTTPS, OIDC, a production embedding endpoint, an audit signing key, and at least one package trust key.
- **Runtime Protocol:** requires SPIFFE X.509-SVID mTLS and binds worker identity to the tenant.
- **Gateway:** requires mTLS, derives the tenant from the peer SVID, and maps immutable tool versions to HTTPS endpoints.
- **Secret Broker:** obtains controlled secrets or dynamic database credentials through OpenBao instead of exposing platform credentials to agents.
- **OCI Provider:** requires digest-pinned production images and uses containerd with gVisor/runsc on the Linux isolation path.
- **Agent Package:** validates signatures, provenance, image signatures, and CycloneDX SBOMs during publication.
- **Audit:** records security-relevant events in a transactional hash chain and supports signed export and integrity verification.

Development mode is restricted to loopback or requires explicit `-dev-mode`; never expose it to an untrusted network.

## Observability and optional services

Start the reference OpenTelemetry stack:

```powershell
docker compose -f deploy/dev/compose.yaml --profile observability up -d
$env:OTEL_EXPORTER_OTLP_ENDPOINT = "127.0.0.1:4317"
```

- Grafana: `http://127.0.0.1:3300`
- Prometheus: `http://127.0.0.1:9093`
- Tempo: `http://127.0.0.1:3320`
- Loki: `http://127.0.0.1:3310`

Optional development services:

```powershell
docker compose -f deploy/dev/compose.yaml --profile secrets up -d  # OpenBao
docker compose -f deploy/dev/compose.yaml --profile search up -d   # OpenSearch
```

Control API operational endpoints:

- `GET /healthz`: process liveness.
- `GET /readyz`: readiness of PostgreSQL and other required dependencies.
- `GET /versionz`: product identity, build commit, and every stable protocol version.

## Tests and quality gates

Formatting, static analysis, unit tests, and Go vulnerability scanning:

```shell
gofmt -l .
go vet ./...
go test -race -count=1 ./...
go tool govulncheck ./...
```

Real PostgreSQL/NATS integration tests:

```powershell
docker compose -f deploy/dev/compose.yaml up -d --wait postgres nats
$env:AGENTOS_TEST_DATABASE_URL = "postgres://agentos:agentos-dev-only@127.0.0.1:55432/agentos?sslmode=disable"
$env:AGENTOS_TEST_NATS_URL = "nats://127.0.0.1:54222"
go test -race -tags=integration -count=1 ./...
```

> The integration suite applies migrations and clears AgentOS test tables. Never point `AGENTOS_TEST_DATABASE_URL` at a database containing durable business data.

Protobuf compatibility and deterministic generation:

```shell
buf lint
buf generate
git diff --exit-code -- gen/go
```

Wasmtime provider:

```shell
cargo +1.97.1 fmt --all -- --check
cargo +1.97.1 clippy --workspace --all-targets --locked -- -D warnings
cargo +1.97.1 test --workspace --locked
```

CI runs the real Linux OCI/gVisor isolation suite in the `runtime-linux-leg` job. The pinned toolchain and acceptance mapping are defined in [`deploy/ci/runtime-matrix.md`](deploy/ci/runtime-matrix.md).

Evaluate a measured SLO sample:

```shell
go run ./cmd/agentos-slo -sample measured-slo.json
```

## Releases and supply-chain verification

The official release workflow runs the GA gates and generates:

- complete command archives for Linux AMD64/ARM64, macOS AMD64/ARM64, and Windows AMD64;
- the `agentos-runtime` Python wheel;
- a CycloneDX 1.6 SBOM;
- `checksums.txt`;
- a keyless `checksums.txt.sigstore.json` Sigstore bundle.

Download and verify all release assets:

```shell
gh release download v1.0.0 --repo CloudEdgeCore/AgentOS
sha256sum -c checksums.txt
```

The release process is defined in [`.github/workflows/release.yml`](.github/workflows/release.yml), and every Action dependency is pinned to an exact commit SHA.

## Repository layout

| Path | Contents |
| --- | --- |
| `cmd/` | CLI, control-plane, gateway, runtime provider, and operator entry points |
| `internal/kernel/` | Task/Run/Attempt, Admission, Scheduler, Policy, Budget, and Recovery |
| `internal/runtime/` | Runtime Control, Reference, Adapter, and OCI providers |
| `internal/gateway/` | Tool, Model, Memory, Capability, and Secret Broker implementations |
| `sdk/agent/` | Go Runtime Interface SDK |
| `sdk/python/` | Python Runtime Interface SDK |
| `adapters/` | LangGraph and A2A adapters |
| `api/openapi/` | Stable and compatibility REST/HTTP contracts |
| `proto/agentos/` | Runtime, Gateway, and Model Protobuf contracts |
| `db/migrations/` | PostgreSQL migrations |
| `deploy/dev/` | Local dependencies and reference observability environment |
| `deploy/ci/` | OCI/gVisor isolation and environment fingerprint tests |
| `modelcheck/tla/` | TLA+ model of the kernel state machine |

## Current boundaries

- v1.0 is an agent control and execution backend; it does not include a complete web administration console or managed cloud service.
- The reference provider is deterministic development infrastructure, not a security sandbox. Production execution should use Wasmtime or OCI/gVisor.
- Firecracker currently has a CI KVM environment probe only and is not a delivered MicroVM provider.
- The TypeScript directory is reserved for a future client SDK and is not part of the v1.0 stable SDK surface.
- Production deployment requires externally operated PostgreSQL, NATS, OIDC, SPIFFE/SPIRE, OpenBao, and real model, tool, and embedding services.

These boundaries are intentional. AgentOS v1.0 delivers a verifiable, recoverable, default-deny agent runtime kernel with stable public contracts.
