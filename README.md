# Agent OS

Agent OS is a system software layer for securely publishing, scheduling, executing, recovering, governing, and auditing heterogeneous AI agents.

The project is in its v0.1 kernel-slice phase. The current implementation establishes the invariants that every later runtime provider and gateway must preserve:

- persistent `Task`, `Run`, and `Attempt` lifecycles;
- optimistic concurrency through `resource_version`;
- single-active-attempt execution through lease and fencing tokens;
- idempotent creation and durable inbox/outbox messaging;
- tenant-scoped database relationships;
- result-before-success transaction ordering;
- a versioned REST task submission/read contract;
- fenced Admission and Scheduler reconciliation;
- explainable runtime-pool placement;
- PostgreSQL outbox delivery to NATS JetStream;
- a fenced Protobuf/gRPC Runtime Protocol with bounded messages;
- content-addressed Artifact metadata and logical Checkpoint envelopes;
- lease-expiry recovery with higher fencing tokens and bounded retries;
- durable cancellation from REST through Runtime acknowledgement;
- deterministic Go and capability-free Rust/Wasmtime Runtime Providers.

The complete architecture and technology baseline live in [`docs/`](./docs/).

## Development

Requirements:

- Go 1.26.x;
- Rust 1.97.1 for the Wasmtime Provider;
- PostgreSQL 18 for integration tests;
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
go run ./cmd/agentos-controller -database-url $databaseUrl -controller-id dev-controller -runtime-pools deploy/dev/runtime-pools.json -dev-mode
go run ./cmd/agentos-outbox -database-url $databaseUrl -nats-url nats://127.0.0.1:54222 -dispatcher-id dev-outbox
go run ./cmd/agentos-runtime-control -database-url $databaseUrl -listen 127.0.0.1:9090 -dev-tenant dev -dev-mode
go run ./cmd/agentos-runtime-reference -control-address 127.0.0.1:9090 -tenant dev -runtime-instance-id dev-worker-1 -artifact-root tmp/artifacts -dev-mode
```

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

The development API intentionally accepts only a fixed tenant identity and
refuses non-loopback listeners. Verified OIDC/workload identity middleware is
required before a non-development deployment.

The integration suite applies checksum-protected migrations and clears only
kernel test tables before each test. Never point `AGENTOS_TEST_DATABASE_URL`
at a database containing durable data. Linux CI additionally runs all unit and
integration tests with the Go race detector.

## Status

No compatibility guarantees apply before the first `v1alpha1` API release. Correctness and security invariants are expected to remain stable even while schemas evolve.
