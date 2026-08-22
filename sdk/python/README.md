# Python SDK (planned)

Status: **planned** — part of the v0.9 Platform slice, not yet implemented.

Purpose: Agent development, Evals, data processing and test tooling for the
Agent OS control plane.

Boundary rules (per the tech baseline):

- Python never enters the Kernel trusted computing base.
- Python Agents must run inside a `RuntimeClass`; they are never loaded as
  trusted control-plane plugins.
- Generated OpenAPI/Protobuf types form the low layer; hand-written stable
  high-level APIs sit on top.

This directory is reserved; do not add ad-hoc scripts here until the SDK
contract is defined.
