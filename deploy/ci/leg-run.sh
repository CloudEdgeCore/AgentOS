#!/usr/bin/env bash
# v0.7: run the OCI/gVisor real-machine leg from a clean Linux host (root).
# Used by the runtime-linux-leg CI job's local equivalent and by self-hosted
# KVM runners. Assumes a checkout at the current directory, Go on PATH, and
# AGENTOS_TEST_DATABASE_URL pointing at a reachable PostgreSQL.
set -euo pipefail

: "${AGENTOS_TEST_DATABASE_URL:?AGENTOS_TEST_DATABASE_URL is required}"
: "${AGENTOS_OCI_CONTAINERD_NAMESPACE:=agentos-ci}"
# Pinned runtime toolchain (deploy/ci/runtime-matrix.md); CI overrides these.
: "${CONTAINERD_VERSION:=2.0.2}"
: "${RUNSC_TAG:=20260810.0}"
export CONTAINERD_VERSION RUNSC_TAG
# Overlayfs on production hosts; nested containers (Docker Desktop/WSL2)
# cannot mount overlay-on-overlay and must use the native snapshotter.
: "${SNAPSHOTTER:=overlayfs}"
export SNAPSHOTTER

# Install and start the pinned containerd + runsc when the host does not
# already have a ready daemon (first bring-up path).
if ! ctr version >/dev/null 2>&1; then
  bash deploy/ci/setup-containerd-runsc.sh
fi

# Environment fingerprint: every runtime result must be reproducible against
# the same fingerprint.
bash deploy/ci/env-fingerprint.sh | tee /tmp/runtime-fingerprint.txt

# Build the provider and put it on PATH (the conformance worker is spawned
# by the test through exec.LookPath).
go build -o /usr/local/bin/agentos-runtime-oci ./cmd/agentos-runtime-oci

# The conformance worker runs with --skip-image-pull, so the workload image
# must already live in the namespace. Default to alpine:latest for the first
# bring-up; CI pins the digest after resolving it (see ci.yml).
if [ -z "${AGENTOS_OCI_IMAGE:-}" ]; then
  ctr -n "$AGENTOS_OCI_CONTAINERD_NAMESPACE" images pull \
    --snapshotter "$SNAPSHOTTER" docker.io/library/alpine:latest
  export AGENTOS_OCI_IMAGE="docker.io/library/alpine:latest"
fi
export AGENTOS_OCI_SNAPSHOTTER="$SNAPSHOTTER"

# Run the OCI/gVisor conformance leg on the real containerd + runsc host.
go test -tags=integration -count=1 \
  -run 'TestSameAgentVersionRunsOnBothProviders/oci' \
  ./internal/runtime/control/...
