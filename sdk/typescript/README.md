# TypeScript SDK (planned)

Status: **planned** — part of the v0.9 Platform slice, not yet implemented.

Purpose: Web/Node clients of the Agent OS control plane (`/v1` REST + SSE task
events), generated from the OpenAPI document in `api/openapi`.

Boundary rules (per the tech baseline):

- TypeScript never enters the Kernel trusted computing base.
- Node Agents must run inside a `RuntimeClass`, never as trusted
  control-plane plugins.

This directory is reserved; package layout is defined when the v0.9 SDK work
starts.
