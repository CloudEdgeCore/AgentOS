#!/usr/bin/env bash
# v0.7: install and start a pinned containerd + runsc on a Linux host for the
# OCI/gVisor real-machine leg. Must run as root (GitHub Actions: sudo).
#
# Pinned sources (verified 2026-08-16, see deploy/ci/runtime-matrix.md):
#   - containerd release tarball from GitHub Releases;
#   - gVisor runsc + containerd-shim-runsc-v1 from the GCS release bucket
#     (releases/release/<tag>/x86_64/), verified against the published
#     .sha512. The shim binary must sit on PATH with its exact name so
#     containerd's task-v2 discovery resolves io.containerd.runsc.v1.
set -euo pipefail

# Every network or daemon-facing command below is bounded: a wedged registry
# connection or a stuck sandbox must fail loudly (with the timeout's stderr in
# the step log) instead of hanging until the CI job timeout kills the step.
command -v timeout >/dev/null 2>&1 || { echo "coreutils timeout is required" >&2; exit 1; }

CONTAINERD_VERSION="${CONTAINERD_VERSION:?set a pinned containerd release (e.g. 2.0.2)}"
RUNSC_TAG="${RUNSC_TAG:?set a pinned gVisor release tag (e.g. 20260810.0)}"
RUNSC_BASE="https://storage.googleapis.com/gvisor/releases/release/${RUNSC_TAG}/x86_64"

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  aarch64) ARCH="arm64" ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

# --- containerd ------------------------------------------------------------
# Runner images may ship containerd (e.g. 2.3.3 on ubuntu-24.04). The pinned
# version must win, so install when missing or when the installed binary is
# not the pinned one. `containerd --version` prints
# "containerd github.com/containerd/containerd/v2 v2.0.2 ..." — the third
# field is the version.
INSTALLED_CONTAINERD="$(containerd --version 2>/dev/null | awk '{print $3}' || true)"
if [ "$INSTALLED_CONTAINERD" != "v${CONTAINERD_VERSION}" ]; then
  echo ">> installing containerd ${CONTAINERD_VERSION} (${ARCH}); installed: ${INSTALLED_CONTAINERD:-none}"
  timeout 300 curl -fsSL -o "$TMPDIR/containerd.tar.gz" \
    "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${ARCH}.tar.gz"
  sha256sum "$TMPDIR/containerd.tar.gz"
  tar -C /usr/local -xzf "$TMPDIR/containerd.tar.gz"
else
  echo ">> containerd ${CONTAINERD_VERSION} already installed"
fi
containerd --version

# --- runsc + containerd shim -------------------------------------------------
if ! command -v runsc >/dev/null 2>&1 || ! command -v containerd-shim-runsc-v1 >/dev/null 2>&1; then
  echo ">> installing gVisor ${RUNSC_TAG} (runsc + containerd-shim-runsc-v1)"
  timeout 300 curl -fsSL -o "$TMPDIR/runsc" "${RUNSC_BASE}/runsc"
  timeout 300 curl -fsSL -o "$TMPDIR/containerd-shim-runsc-v1" "${RUNSC_BASE}/containerd-shim-runsc-v1"
  # Verify against the published sha512 (the .sha512 files carry the bare
  # filename; verify from the same directory).
  (cd "$TMPDIR" \
    && timeout 120 curl -fsSL -o runsc.sha512 "${RUNSC_BASE}/runsc.sha512" \
    && timeout 120 curl -fsSL -o containerd-shim-runsc-v1.sha512 "${RUNSC_BASE}/containerd-shim-runsc-v1.sha512" \
    && sha512sum -c runsc.sha512 containerd-shim-runsc-v1.sha512)
  install -m 0755 "$TMPDIR/runsc" /usr/local/bin/runsc.real
  install -m 0755 "$TMPDIR/containerd-shim-runsc-v1" /usr/local/bin/containerd-shim-runsc-v1
fi
if [ -n "${RUNSC_EXTRA_FLAGS:-}" ]; then
  # Environments where runsc cannot manage cgroups (e.g. containerd nested in
  # a container) need flags like --ignore-cgroups. runsc only honors such
  # flags from its own command line — annotation overrides are gated behind
  # --allow-flag-override (chicken-and-egg) — so install a thin wrapper that
  # prepends them. Bring-up finding 2026-08-16, see runtime-matrix.md.
  echo ">> installing runsc wrapper with extra flags: ${RUNSC_EXTRA_FLAGS}"
  cat > /usr/local/bin/runsc <<EOF
#!/bin/sh
exec /usr/local/bin/runsc.real ${RUNSC_EXTRA_FLAGS} "\$@"
EOF
  chmod 0755 /usr/local/bin/runsc
else
  # No wrapper: the shim resolves runsc by name on PATH.
  ln -sf /usr/local/bin/runsc.real /usr/local/bin/runsc
fi
# head -n 1 can close the pipe while runsc is still writing (SIGPIPE), so the
# version print must never fail the script.
runsc --version | head -n 1 || true

# --- containerd config with the runsc runtime ------------------------------
# ctr resolves io.containerd.runsc.v1 through containerd's task-v2 shim
# discovery (containerd-shim-runsc-v1 on PATH); the CRI entry keeps
# containerd's CRI surface consistent for future kubernetes-style
# consumption. The exact config surface is itself part of what v0.7
# validates on the first runner bring-up.
#
# SNAPSHOTTER defaults to overlayfs (production hosts). Nested environments
# (containerd inside a container, e.g. Docker Desktop/WSL2) cannot mount
# overlay-on-overlay ("mount source overlay ... invalid argument"), so they
# set SNAPSHOTTER=native, which containerd 2.x clients pick up as the
# server-side default snapshotter.
mkdir -p /etc/containerd
cat > /etc/containerd/config.toml <<EOF
version = 2
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
[plugins."io.containerd.snapshotter.v1"]
  default_snapshotter = "${SNAPSHOTTER:-overlayfs}"
EOF

# --- start containerd and wait for readiness -------------------------------
# Always run our own daemon: a pre-existing daemon on the runner may serve an
# unpinned version, so stop it first (systemd service, then pkill twice for a
# slow shutdown race), then start the pinned binary with our config.
if systemctl stop containerd >/dev/null 2>&1; then
  echo ">> stopped containerd systemd service"
fi
if pgrep -x containerd >/dev/null 2>&1; then
  echo ">> stopping pre-existing containerd daemon"
  pkill -x containerd || true
  sleep 1
  pkill -x containerd || true
fi
echo ">> starting containerd (pinned ${CONTAINERD_VERSION})"
nohup containerd >/tmp/agentos-containerd.log 2>&1 &
for attempt in $(seq 1 30); do
  if ctr version >/dev/null 2>&1; then
    break
  fi
  sleep 1
  if [ "$attempt" -eq 30 ]; then
    echo "containerd did not become ready; log:" >&2
    tail -n 50 /tmp/agentos-containerd.log >&2 || true
    exit 1
  fi
done
echo ">> containerd ready"
# The socket must serve the pinned version: a resurrected service daemon
# would silently run the leg against an unpinned containerd. Capture the
# output first — grep -q on a live pipe closes it while ctr is still
# writing (SIGPIPE + pipefail would misreport a version mismatch).
VERSION_OUTPUT="$(ctr version 2>&1 || true)"
if ! echo "$VERSION_OUTPUT" | grep -q "v${CONTAINERD_VERSION}"; then
  echo "containerd on the socket is not the pinned ${CONTAINERD_VERSION}; full version output:" >&2
  echo "$VERSION_OUTPUT" >&2
  exit 1
fi
echo ">> containerd on socket is pinned ${CONTAINERD_VERSION}"

# --- verify the runsc runtime end to end -------------------------------------
SNAPSHOTTER="${SNAPSHOTTER:-overlayfs}"
echo ">> pulling busybox (bounded 300s)"
timeout 300 ctr -n "${AGENTOS_OCI_CONTAINERD_NAMESPACE:-agentos-ci}" images pull \
  --snapshotter "$SNAPSHOTTER" docker.io/library/busybox:latest >/dev/null
echo ">> running runsc probe (bounded 300s)"
if timeout 300 ctr -n "${AGENTOS_OCI_CONTAINERD_NAMESPACE:-agentos-ci}" run \
  --rm --runtime io.containerd.runsc.v1 --snapshotter "$SNAPSHOTTER" \
  docker.io/library/busybox:latest agentos-runtime-probe /bin/true; then
  echo ">> runsc runtime probe: PASS (snapshotter=${SNAPSHOTTER})"
else
  echo ">> runsc runtime probe: FAIL (the pinned matrix must be re-validated)" >&2
  exit 1
fi
