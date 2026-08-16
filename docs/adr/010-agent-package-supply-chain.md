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
