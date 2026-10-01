# Web / Dashboard

Status: **reserved** — the full platform dashboard (TypeScript + React) is
still planned; no code exists in this directory yet.

A lightweight developer workbench ships with v1.3: `agent ui` launches an
embedded, single-process console (configuration hub, model connectivity and
streaming test, MCP tool probing, agent/tool management, audit receipts)
implemented in [`cmd/agent/ui.go`](../cmd/agent/ui.go). It is a local
developer tool, not the multi-tenant platform dashboard.

The planned dashboard covers Task/Run/Attempt views, Trace Viewer,
Policy debugger, approval simulator and Checkpoint inspector; it consumes
only the public `/v1` REST/SSE contract — never internal gRPC.
