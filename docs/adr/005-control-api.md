# ADR-005: Versioned Public Control API

- Status: Accepted
- Date: 2026-08-13

## Context

External clients need a stable task contract without gaining access to internal
runtime RPCs or kernel-owned lifecycle fields. Tenant identity must not be
accepted from untrusted request data.

## Decision

The public contract is REST described by OpenAPI 3.1 under `/v1`. Task creation
requires `Idempotency-Key`, uses strict JSON and is limited to 1 MiB. The server
derives tenant identity from authenticated context, generates UUIDv7 task IDs,
and rejects unknown fields including status. Responses include
`resourceVersion`, `traceId`, ETag and structured machine-readable reason codes.

Tenant-scoped lookup returns `404` for both absent and cross-tenant objects to
avoid an existence oracle. Internal Runtime APIs remain separate future
Protobuf/gRPC contracts. The local executable supports only a fixed development
identity on a loopback listener; it is not a production authentication mode.

## Consequences

- Public compatibility can evolve independently of internal execution RPCs.
- Generated clients can rely on a semantically validated OpenAPI document.
- Production deployment remains blocked on verified OIDC/workload identity
  middleware rather than silently trusting tenant headers.
- SSE task-event observation remains a later additive endpoint.
