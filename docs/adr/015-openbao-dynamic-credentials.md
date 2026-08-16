# ADR-015: OpenBao Dynamic Database Credentials

- Status: Accepted
- Date: 2026-08-16

## Context

ADR-012 delivered KV v2 reads as the v0.3 Secret Broker boundary and
explicitly projected dynamic secrets (leases, databases, PKI). Static
credentials in KV are long-lived: they cannot be rotated per invocation,
they survive the gateway process, and a leak is a standing credential.
The tool decision chain issues a scoped handle per invocation; the broker
should issue a credential that is born, renewed and revoked around that
invocation — nothing more.

## Decision

The v0.4 dynamic Secret Broker issues credentials from the OpenBao database
secrets engine:

- `internal/gateway/bao.DynamicBroker` implements `tool.SecretBroker` against
  `database/creds/<role>`: each fresh issuance returns a username/password
  pair as the scoped handle (JSON), registers the lease, and caches the
  handle per invocation scope for the bounded TTL.
- Lease lifecycle is explicit:
  - a janitor (`Janitor`) renews leases approaching expiry while the gateway
    runs (the lease is extended to 70% of the renewed TTL) and revokes
    leases that can no longer be renewed;
  - `Close` revokes every outstanding lease — dynamic credentials never
    outlive the gateway process;
  - issuing after close fails closed.
- The gateway command takes `-bao-dynamic-role`; when set, the dynamic
  broker replaces the KV broker and runs its janitor on a 30s loop, with
  revocation on shutdown.
- Dev provisioning: `deploy/dev/openbao-init.ps1` configures the database
  engine against the dev PostgreSQL (role `dev-db`, 5m default TTL, SELECT
  on public tables only) — idempotent and repeatable.

## Consequences

- Credentials are short-lived and revocable: the E2E smoke proves the full
  lifecycle against real OpenBao + PostgreSQL — issue, connect and query
  with the credential, revoke on close, and PostgreSQL rejecting the revoked
  credential (password authentication failure).
- The lease janitor is best-effort; if the gateway crashes, OpenBao's own
  TTL still expires the credential — no permanent lease can leak.
- The dynamic role's privileges are the security boundary: the dev role is
  SELECT-only; production roles must be least-privilege per tenant and tool.
- Root-credential rotation (templated connection URLs), PKI/database engine
  families beyond PostgreSQL, and per-scope roles remain projections.
