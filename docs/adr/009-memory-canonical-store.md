# ADR-009: Memory Canonical Store, Retrieval Projections and Deletion Proof

- Status: Accepted
- Date: 2026-08-15

## Context

Memory outlives any single Run and can change future behavior, so it must be
governed like data, not like a cache: provenance, owner, tenant, sensitivity,
trust, version, retention and revocation must survive. Vector, keyword and
graph stores are excellent retrieval indexes but cannot serve as the canonical
store: they cannot express ACLs and deletion intent strongly, and their
replication and failure semantics are eventual. If a derived index is the only
copy of a correction or a tombstone, legal deletion and revocation become
impossible to prove.

## Decision

PostgreSQL is the canonical store for Memory metadata: identity, provenance,
ACL, tenant, sensitivity, trust, version history, retention policy, tombstone
and deletion-intent state. Large content (text payloads, documents, datasets)
lives in the S3-compatible object store as content-addressed Artifacts, with
metadata and content digest signed/verified separately. Retrieval indexes are
always derived and always rebuildable:

- v0.1 uses PostgreSQL FTS + pgvector so the canonical store and the first
  index share one transaction system; default exact vector search, HNSW only
  after measured recall requirements, and multilingual (including Chinese)
  keyword quality is evaluated separately.
- v0.3+ may add an OpenSearch projection for keyword + vector + hybrid
  retrieval. Projected documents carry tenant, namespace, residency,
  sensitivity, visibility token and source version; retrieval applies
  tenant/ACL/sensitivity pre-filter before scoring and the Memory Service
  performs an exact ACL recheck after retrieval.
- Tombstones, corrections and deletions are tracked projection jobs; every
  projection can be fully rebuilt from the canonical store.

The ACL check runs before retrieval, never after: no sensitive content is
retrieved and then filtered. A dedicated vector database is not adopted as a
source of truth; it may only appear as a future retrieval provider whose data
is reconstructible from the canonical store.

## Consequences

- Forget, correction, supersede, rollback and revocation are expressible and
  auditable; deletion propagation to derived indexes, caches and backups is
  trackable with completion proof.
- v0.1 keeps operational complexity low by reusing PostgreSQL instead of
  adding a second distributed system.
- Projection freshness becomes a measurable contract, and index drift is
  recoverable by rebuild rather than by manual repair.
- High-sensitivity tenants require physical index/tenant boundaries beyond
  Dashboard-level tenant filtering.
