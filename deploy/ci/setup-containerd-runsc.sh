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

# Host facts recorded for later diagnosis: runsc sandbox creation needs clone()
# with namespace flags and (for the ptrace/systrap platforms) ptrace access.
# A host that blocks either (seccomp filter, userns sysctl) will hang the
# sandbox silently — these lines document that constraint in the job log.
echo ">> host kernel: $(uname -r)"
echo ">> unprivileged_userns_clone=$(cat /proc/sys/kernel/unprivileged_userns_clone 2>/dev/null || echo n/a) max_user_namespaces=$(cat /proc/sys/user/max_user_namespaces 2>/dev/null || echo n/a)"
echo ">> seccomp=$(awk '/^Seccomp:/{print $2}' /proc/self/status 2>/dev/null || echo n/a) (0=disabled,2=filter)"
echo ">> apparmor=$(cat /sys/module/apparmor/parameters/enabled 2>/dev/null || echo n/a)"

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
# The v2 task plugin runs runsc AS the shim (runtime_path), the long-standing
# gVisor/containerd integration. The standalone containerd-shim-runsc-v1 was
# found to hang on GitHub runners after containerd connects to its TTRPC
# endpoint (CreateTask never progresses and runsc never starts); routing
# through runsc-as-shim skips that layer. The CRI entry keeps containerd's
# CRI surface consistent for future kubernetes-style consumption.
#
# SNAPSHOTTER defaults to overlayfs (production hosts). Nested environments
# (containerd inside a container, e.g. Docker Desktop/WSL2) cannot mount
# overlay-on-overlay ("mount source overlay ... invalid argument"), so they
# set SNAPSHOTTER=native, which is passed to ctr explicitly.
mkdir -p /etc/containerd
cat > /etc/containerd/config.toml <<EOF
version = 2
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
[plugins."io.containerd.runtime.v2.task".runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
  runtime_path = "/usr/local/bin/runsc"
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
timeout 300 ctr -n "${AGENTOS_OCI_CONTAINERD_NAMESPACE:-agentos-ci}" run \
  --rm --runtime io.containerd.runsc.v1 --snapshotter "$SNAPSHOTTER" \
  docker.io/library/busybox:latest agentos-runtime-probe /bin/true \
  >/tmp/probe-out.log 2>&1 &
PROBE_PID=$!
# Mid-hang diagnostics: 30s in, if the probe is still alive, capture who is
# blocked where (kernel stack, wchan, fds) so a silent hang pinpoints the
# exact layer instead of burning the full 300s bound.
(
  sleep 30
  {
    echo "== DIAG-RAN probe-alive=$(kill -0 "$PROBE_PID" 2>/dev/null && echo yes || echo no) =="
    if kill -0 "$PROBE_PID" 2>/dev/null; then
      echo "== ps (runsc/shim/ctr) =="
      ps -eo pid,ppid,stat,wchan,cmd | grep -E 'runsc|shim|ctr' | grep -v grep || true
      echo "== shim sockets: $(ls -la /run/containerd/s/ 2>/dev/null || echo none) =="
      for p in $(pgrep -f 'runsc|containerd-shim' || true); do
        echo "--- proc ${p} wchan: $(cat /proc/${p}/wchan 2>/dev/null || echo unknown)"
        echo "--- proc ${p} stack:"
        cat /proc/${p}/stack 2>/dev/null | tail -n 10 || true
        echo "--- proc ${p} fds:"
        ls -l /proc/${p}/fd 2>/dev/null | tail -n 12 || true
      done
    fi
  } > /tmp/probe-diag.log 2>&1
) &
DIAG_PID=$!
if wait "$PROBE_PID"; then
  kill "$DIAG_PID" 2>/dev/null || true
  echo ">> runsc runtime probe: PASS (snapshotter=${SNAPSHOTTER})"
else
  kill "$DIAG_PID" 2>/dev/null || true
  echo ">> runsc runtime probe: FAIL (the pinned matrix must be re-validated)" >&2
  echo ">> probe output:" >&2
  cat /tmp/probe-out.log 2>/dev/null >&2 || true
  echo ">> mid-hang diagnostics:" >&2
  cat /tmp/probe-diag.log 2>/dev/null >&2 || true
  echo ">> containerd log tail:" >&2
  tail -n 200 /tmp/agentos-containerd.log >&2 || true
  echo ">> runsc debug log tail:" >&2
  tail -n 40 /tmp/runsc-debug.log* 2>/dev/null >&2 || true
  echo ">> shim sockets: $(ls -la /run/containerd/s/ 2>/dev/null || echo none)" >&2
  echo ">> dmesg tail:" >&2
  dmesg 2>/dev/null | tail -n 30 >&2 || true
  # --- bisect: direct runsc, bypassing containerd entirely ----------------
  # If the direct sandbox also hangs, runsc/kernel is the failing layer; if it
  # passes, containerd's shim handshake is. Build a minimal OCI bundle from
  # the already-pulled busybox image (single gzip layer) and run /bin/true.
  echo ">> bisect: direct runsc probe (no containerd)" >&2
  BUNDLE="$TMPDIR/bundle"
  mkdir -p "$BUNDLE/rootfs"
  timeout 180 ctr -n "${AGENTOS_OCI_CONTAINERD_NAMESPACE:-agentos-ci}" images export \
    /tmp/busybox-export.tar docker.io/library/busybox:latest >/dev/null 2>&1 || true
  ROOTFS_LAYER="$(tar -tf /tmp/busybox-export.tar 2>/dev/null | grep -E 'blobs/.+layer\.tar$' | head -n 1 || true)"
  if [ -n "$ROOTFS_LAYER" ] && tar -xOf /tmp/busybox-export.tar "$ROOTFS_LAYER" 2>/dev/null | tar -xz -C "$BUNDLE/rootfs" 2>/dev/null; then
    cat > "$BUNDLE/config.json" <<'JSON'
{
  "ociVersion": "1.0.2",
  "process": {"terminal": false, "user": {"uid": 0, "gid": 0}, "args": ["/bin/true"], "env": ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"], "cwd": "/"},
  "root": {"path": "rootfs", "readonly": true},
  "hostname": "runsc-direct-probe",
  "linux": {"namespaces": [{"type": "pid"}, {"type": "mount"}, {"type": "ipc"}, {"type": "uts"}]}
}
JSON
    if (cd "$BUNDLE" && timeout 120 /usr/local/bin/runsc.real --platform=ptrace --network=none \
        --ignore-cgroups --debug --debug-log=/tmp/runsc-direct.log run \
        -bundle "$BUNDLE" direct-probe) >/tmp/direct-out.log 2>&1; then
      echo ">> bisect: DIRECT runsc probe PASSED (containerd shim handshake is the failing layer)" >&2
      timeout 30 /usr/local/bin/runsc.real delete direct-probe >/dev/null 2>&1 || true
    else
      echo ">> bisect: DIRECT runsc probe FAILED (runsc/kernel is the failing layer)" >&2
      echo ">> direct runsc output:" >&2
      cat /tmp/direct-out.log >&2 || true
      echo ">> direct runsc debug log tail:" >&2
      tail -n 40 /tmp/runsc-direct.log 2>/dev/null >&2 || true
    fi
  else
    echo ">> bisect: could not assemble busybox bundle (layer=${ROOTFS_LAYER:-none}); skipping direct probe" >&2
  fi
  exit 1
fi
