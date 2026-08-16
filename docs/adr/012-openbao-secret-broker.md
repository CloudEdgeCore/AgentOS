# ADR-012: OpenBao Secret Broker

- Status: Accepted
- Date: 2026-08-16

## Context

Tool invocations need scoped credentials, but the Agent must never hold them:
the gateway's `SecretBroker.Issue(scope)` contract returns an opaque
`SecretHandle` that is injected into the executing adapter and redacted from
results (architecture §12.4). The v0.1 development broker returned a fixed
handle, which exercises the sanitizer but issues no real secret. The Secure
Runtime needs a reference broker that actually resolves scope to secret
material, fail-closed, without embedding credentials in the control plane or
the runtime.

## Decision

The reference Secret Broker is backed by OpenBao (the open-source Vault
fork), KV v2:

- Secrets live under
  `<mount>/agentos/<tenant>/<tool>/<resource>` — the resource is URL-escaped
  so tool resources containing slashes (`fs:/tmp`) stay one path segment.
- `internal/gateway/bao` implements `tool.SecretBroker`: `Issue` reads the
  scope's secret with the broker token, renders the material (the `value`
  string key, or the compact JSON of the data object), and returns it as the
  handle. The handle is cached per scope for a bounded TTL (default 30s) so
  repeated tool calls do not hammer the broker.
- Fail-closed: a missing secret (`404`), a denied read (`403`), an unreachable
  broker, or an empty secret fails the invocation; the tool call is recorded
  `SECRET_BROKER_FAILED` and nothing runs without a scoped credential.
- The Tool Gateway command takes `-bao-addr`, `-bao-token`, `-bao-mount`,
  `-bao-namespace` and `-bao-cache-ttl`; without `-bao-addr` it falls back to
  the dev stub with a warning.
- The dev compose file runs OpenBao in dev mode
  (`docker compose --profile secrets up -d`, token `bao-dev-only`,
  `127.0.0.1:58200`) and secrets are seeded with the `bao` CLI, e.g.
  `bao kv put secret/agentos/tenant-a/echo.dev/fs-tmp value=dev-secret-1`.

## Consequences

- Scoped secret issuance is real and auditable (issuance is logged per scope,
  never the material), and the gateway's existing redaction path is exercised
  with real secret-shaped values.
- The broker token is a static root token in dev; production wires an
  OpenBao token with least-privilege policies per mount and rotates it
  through the secret manager. Dynamic secrets (leases, databases, PKI) are a
  projection: KV v2 reads are the v0.3 reference boundary.
- OpenBao is an external dependency of the gateway only when configured; the
  rest of the platform is unchanged.
