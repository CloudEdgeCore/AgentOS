# ADR-001: Go/Rust Language Boundary and FFI Prohibition

- Status: Accepted
- Date: 2026-08-13

## Context

Agent OS spans a control plane (API, controllers, admission, scheduling, budget,
gateways, CLI, conformance kit) and a high-risk runtime boundary (Wasmtime
embedding, future microVM hosts, filesystem proxies). A single language either
slows control-plane delivery or enlarges the memory-unsafe trusted computing
base. Process-internal Go/Rust FFI would additionally couple two runtimes in
one process: panics and undefined behavior cross the boundary, ownership and
allocation models clash, and the resulting ABI cannot be versioned with the
Runtime Protocol.

## Decision

Go is the language of the control plane and of every component where ecosystem,
concurrency and delivery velocity dominate. Rust is used only at boundaries
where the safety or performance benefit justifies its cost: the Wasmtime
Provider, and any future microVM host, filesystem proxy or binary-protocol
module with precise memory and lifecycle requirements. Rust never replaces the
control plane.

The boundary between Go and Rust is the fenced Protobuf/gRPC Runtime Protocol,
never an in-process FFI:

- No cgo, no Go/Rust shared library, no `unsafe` interop shims between the
  kernel and any Rust component.
- Rust crates in this repository default to `#![forbid(unsafe_code)]`; any
  crate that must use `unsafe` is isolated, individually reviewed and fuzzed,
  and declares its reason.
- `rust-toolchain.toml` pins the stable toolchain and defines the MSRV;
  `Cargo.lock` is committed.
- Each provider process runs independently and is deployed/upgraded
  independently; a Rust provider crash cannot take down the kernel.
- Python and TypeScript never enter the kernel trusted computing base; they
  produce SDKs and Agent workloads that run inside a RuntimeClass.

## Consequences

- Kernel and Runtime evolve independently behind the gRPC contract; a Rust
  provider can be replaced without recompiling Go code.
- No single process inherits the failure modes of both runtimes.
- The protocol boundary adds serialization and deployment overhead that is
  acceptable for control-volume traffic and is bounded by the 1 MiB message
  cap and artifact-reference rule of ADR-006.
- Hiring and review must cover two language ecosystems; the Rust surface is
  deliberately kept small and owned explicitly.
- A future need for in-process embedding must be proven by measurements and
  reopen this ADR before any FFI is introduced.
