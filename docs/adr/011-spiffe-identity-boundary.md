# ADR-011: SPIFFE X.509-SVID Identity Boundary

- Status: Accepted
- Date: 2026-08-16

## Context

Every fenced boundary in Agent OS trusts the transport for peer identity.
A compromised or misconfigured runtime worker could present any tenant's
identity, impersonate another worker instance, or replay another deployment's
credentials. The kernel-to-runtime Runtime Protocol, in particular, carries
fenced attempt control: a peer that can claim `tenant-a` and `worker-1` can
drive that worker's attempts. Identity must be machine-verifiable and bound to
the request claims, not a shared secret or a plaintext label.

## Decision

The v0.3 Secure Runtime establishes a SPIFFE-style X.509-SVID identity
boundary on the Runtime Protocol:

- Every principal holds a certificate whose SPIFFE ID names its exact
  position in the trust domain:
  - workers: `spiffe://<trust-domain>/ns/<tenant>/worker/<runtime-instance>`
  - control plane: `spiffe://<trust-domain>/ns/system/control`
- Workers connect over mutual TLS (TLS 1.3 minimum) presenting their SVID;
  the control plane requires and verifies the client chain against the trust
  bundle and enforces the SPIFFE ID pattern
  (`spiffe://<trust-domain>/ns/*/worker/*`) on every call.
- The service additionally binds request claims to the SVID: the tenant a
  request claims must equal the SVID's tenant, and poll requests must claim
  the exact runtime instance the SVID names. Any mismatch is
  `PermissionDenied` before store access; a missing client certificate never
  completes the TLS handshake.
- `cmd/agentos-svid` generates the dev material: a self-signed CA (the trust
  bundle), a control plane SVID (with loopback IP SAN) and one SVID per
  worker. Production rotates the CA through a real CA process; private keys
  live in the secret manager; the trust bundle is distributed to every
  boundary.
- The reference and OCI/gVisor runtime commands accept `-tls-cert`,
  `-tls-key` and `-trust-bundle`; the runtime-control command additionally
  accepts `-spiffe-pattern`. Without these flags the loopback dev transport
  stays plaintext, explicitly acknowledged with `-dev-mode`; deployments that
  configure mTLS are enforced fail-closed (a peer without a verified SVID is
  rejected before any store access).

## Consequences

- A worker cannot impersonate another tenant or instance: the SVID binds both
  the transport (TLS client chain + SPIFFE ID pattern) and the request claims
  (service-level binding), so cross-tenant and cross-instance calls are
  rejected with machine-readable status codes.
- SVIDs are short-lived by default (90 days dev); rotation is a re-issue plus
  trust-bundle update, and previously issued SVIDs stop verifying at expiry.
- The Runtime Protocol is the v0.3 identity boundary; the Tool and Model
  gateways remain loopback dev endpoints in this release and are documented
  as such (see the v0.3 boundary documentation).
- Certificate lifecycle, revocation and workload attestation (K8s SPIFFE CSI,
  workload API) are projected; the current CA-per-deployment model is the
  explicit boundary of v0.3.
