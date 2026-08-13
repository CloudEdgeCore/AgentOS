# Agent OS

Agent OS is a system software layer for securely publishing, scheduling, executing, recovering, governing, and auditing heterogeneous AI agents.

The project is in its architecture and kernel-foundation phase. The first implementation slice establishes the invariants that every later API, scheduler, runtime provider, and gateway must preserve:

- persistent `Task`, `Run`, and `Attempt` lifecycles;
- optimistic concurrency through `resource_version`;
- single-active-attempt execution through lease and fencing tokens;
- idempotent creation and durable inbox/outbox messaging;
- tenant-scoped database relationships;
- result-before-success transaction ordering.

The complete architecture and technology baseline live in [`docs/`](./docs/).

## Development

Requirements:

- Go 1.26.x;
- PostgreSQL 18 for integration tests;
- Docker only when running the local PostgreSQL container.

Run unit tests:

```shell
go test ./...
```

Run PostgreSQL integration tests:

```powershell
docker compose -f deploy/dev/compose.yaml up -d --wait
$env:AGENTOS_TEST_DATABASE_URL = "postgres://agentos:agentos-dev-only@127.0.0.1:55432/agentos?sslmode=disable"
go test -tags=integration -count=1 ./...
```

Apply migrations:

```powershell
go run ./cmd/agentos-migrate -database-url $env:DATABASE_URL
```

The integration suite applies checksum-protected migrations and clears only
kernel test tables before each test. Never point `AGENTOS_TEST_DATABASE_URL`
at a database containing durable data. Linux CI additionally runs all unit and
integration tests with the Go race detector.

## Status

No compatibility guarantees apply before the first `v1alpha1` API release. Correctness and security invariants are expected to remain stable even while schemas evolve.
