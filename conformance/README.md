# Conformance Test Kit (planned)

Status: **planned** — the v0.1 dual-provider conformance suite currently lives
in `internal/runtime/control/conformance_integration_test.go`; the formal kit
is part of the v0.9 Platform slice.

Purpose: the executable compatibility contract of Agent OS. The kit runs the
same published `AgentVersion` across providers (Go reference, Wasmtime,
future OCI/gVisor) and verifies protocol, schema, state-machine and
compatibility behavior. Any P0 middleware replacement must pass the same suite
before it is accepted (per the [tech baseline](docs/Agent_OS_技术选型与工程基线.md) §18).

Planned contents:

- Provider conformance runner and compatibility matrix.
- Checkpoint compatibility (accept/reject) fixtures.
- Protocol breaking-change checks tied to Buf.
- Fault-injection scenarios: duplicate messages, late writes, restart,
  partition.

This directory is reserved; existing conformance tests keep their current home
until the kit is formalized.
