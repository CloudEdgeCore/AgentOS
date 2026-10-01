# Web / Dashboard

Status: **reserved** — the full platform dashboard (TypeScript + React) is
still planned; no code exists in this directory yet.

A lightweight developer workbench exists in
[`cmd/agent/ui.go`](../cmd/agent/ui.go), but it is intentionally gated: this
release is CLI-only, `agent ui` refuses to start for external access, and only
the loopback-bound internal preview (`agent ui --preview`) launches the
single-process console (configuration hub, model connectivity test, MCP tool
probing, audit receipts). The supported interface is the CLI
(`agent config wizard`, `agent config set`, `agent test-llm`).

The planned dashboard covers Task/Run/Attempt views, Trace Viewer,
Policy debugger, approval simulator and Checkpoint inspector; it consumes
only the public `/v1` REST/SSE contract — never internal gRPC.
