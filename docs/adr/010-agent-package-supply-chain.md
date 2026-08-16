# ADR-010: OCI Agent Package, Signing, SBOM and Trust Root

- Status: Accepted
- Date: 2026-08-16

## Context

An immutable `AgentVersion` is only as trustworthy as the artifact it deploys.
Admission must reject anything whose digest, signer, provenance or dependency
content is unknown. Tag-based references, unsigned manifests and unverifiable
builds turn a supply-chain compromise into an arbitrary-code execution on
every runtime pool.

## Decision

Agent Packages are OCI artifacts with a custom media type, pushed and pulled
with ORAS against an OCI Distribution registry. The package manifest pins, by
digest: the AgentVersion spec, runtime lock, tool lock, permission policy,
memory schema and migrations, SBOM and provenance. CI generates an SPDX or
CycloneDX SBOM and in-toto/SLSA provenance for every package, and signs the
package and its container images with Cosign.

Production admission verifies, in order and fail-closed: image/package digest
pinning, Cosign signature validity, signer identity against the OIDC issuer,
provenance authenticity, and membership in the allow-listed build workflows.
Production references are digest-only; mutable tags are never deployed.
Dependency scanning, secret scanning, license policy and base-image EOL checks
are required CI gates.

The trust root for v0.1 is the repository's own CI/CD identity chain
(build workflow OIDC → Cosign key → package manifest); federated multi-org
trust and a public trust policy are deferred to the v0.3 Secure Runtime slice.

## Consequences

- Admission has a mechanical, auditable gate before any AgentVersion is
  scheduled; a broken signature, missing provenance or unknown dependency
  fails the task with a machine-readable reason code.
- Rollback is expressible: any previously signed digest remains deployable
  while its verification inputs remain valid.
- Package provenance and SBOMs become queryable for compliance and incident
  response.
- CI/CD and key management become part of the security boundary; key custody
  and rotation policy must be explicit before production exposure.

## v0.3 implementation note

The v0.3 Secure Runtime slice implements the kernel-level trust anchor of this
ADR: the canonical `agentos.agentpkg/v1` manifest is signed with ed25519 and
verified before any publication is admitted.

- `internal/kernel/agentpkg` defines the manifest, signature envelope,
  digest pinning and the fail-closed `Registry` of trusted signing keys.
- Publish admission (`internal/control/api`, `POST /v1/agent-versions`)
  verifies, in order: signature validity against the trusted registry,
  canonical manifest digest, and binding of the signed manifest to the exact
  `name@version` reference and spec bytes of the publication. With a trust
  registry configured (`-package-trust-key` or `AGENTOS_PACKAGE_TRUST_KEYS`)
  unsigned publications are rejected (`PACKAGE_REQUIRED`); without one
  (dev mode) unsigned publications are allowed but a presented package is
  still verified fail-closed.
- The verified signature envelope (key ID, signature, manifest digest) is
  persisted with the immutable `agent_versions` row (migration 000011) and
  returned by the control API, making every publication provable in audits.
- `cmd/agentos-pkg` is the CI-facing tool: `genkey` creates signing keys,
  `sign` produces the canonical signed package document (compact JSON so
  manifest spec bytes round-trip), `verify` checks a package against a trust
  registry.
- Federated multi-org trust, OCI image-level Cosign signatures and SBOM
  ingestion remain projected; the signed kernel manifest is the v0.3 trust
  root, and production admission fails closed on it.

