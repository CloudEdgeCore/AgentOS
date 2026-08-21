#!/usr/bin/env bash
# v0.7: install and start a pinned containerd + runsc on a Linux host for the
# OCI/gVisor real-machine leg. Must run as root (GitHub Actions: sudo).
#
# Pinned sources:
#   - containerd release tarball from GitHub Releases;
#   - the complete gVisor release tarball from GCS, including runsc, the
#     containerd shim, and gvisor-bin sidecars. gVisor releases after 2026-07
#     are not valid binary-only installations.
set -euo pipefail

# Every network or daemon-facing command below is bounded: a wedged registry
# connection or a stuck sandbox must fail loudly (with the timeout's stderr in
# the step log) instead of hanging until the CI job timeout kills the step.
command -v timeout >/dev/null 2>&1 || { echo "coreutils timeout is required" >&2; exit 1; }

CONTAINERD_VERSION="${CONTAINERD_VERSION:?set a pinned containerd release (e.g. 2.0.2)}"
CONTAINERD_SHA256="${CONTAINERD_SHA256:?set the approved containerd archive SHA-256}"
RUNSC_TAG="${RUNSC_TAG:?set a pinned gVisor release tag (e.g. 20260810.0)}"
GVISOR_ARCH="$(uname -m)"
RUNSC_BASE="https://storage.googleapis.com/gvisor/releases/release/${RUNSC_TAG}/${GVISOR_ARCH}"

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
  echo "${CONTAINERD_SHA256}  $TMPDIR/containerd.tar.gz" | sha256sum -c -
  tar -C /usr/local -xzf "$TMPDIR/containerd.tar.gz"
else
  echo ">> containerd ${CONTAINERD_VERSION} already installed"
fi
containerd --version

# --- complete gVisor release -------------------------------------------------
echo ">> installing complete gVisor ${RUNSC_TAG} release"
timeout 300 curl -fsSL -o "$TMPDIR/gvisor.tar.bz2" "${RUNSC_BASE}/gvisor.tar.bz2"
timeout 120 curl -fsSL -o "$TMPDIR/gvisor.tar.bz2.sha512" "${RUNSC_BASE}/gvisor.tar.bz2.sha512"
(cd "$TMPDIR" && sha512sum -c gvisor.tar.bz2.sha512)
tar -xjf "$TMPDIR/gvisor.tar.bz2" -C /usr/local/bin
for required in /usr/local/bin/runsc /usr/local/bin/containerd-shim-runsc-v1; do
  if [ ! -x "$required" ]; then
    echo "gVisor release is missing executable: $required" >&2
    exit 1
  fi
done
if [ ! -d /usr/local/bin/gvisor-bin ] || ! find /usr/local/bin/gvisor-bin -maxdepth 1 -type f -perm /111 -print -quit | grep -q .; then
  echo "gVisor release is missing executable gvisor-bin sidecars" >&2
  exit 1
fi
RUNSC_VERSION_OUTPUT="$(runsc --version 2>&1 || true)"
echo "$RUNSC_VERSION_OUTPUT"
if ! grep -Fq "release-${RUNSC_TAG}" <<<"$RUNSC_VERSION_OUTPUT"; then
  echo "installed runsc does not match pinned release ${RUNSC_TAG}" >&2
  exit 1
fi

# --- gVisor shim + containerd configuration ---------------------------------
mkdir -p /etc/containerd
RUNSC_CONFIG_PATH="${RUNSC_CONFIG_PATH:-/etc/containerd/runsc.toml}"
RUNSC_PLATFORM="${RUNSC_PLATFORM:-ptrace}"
RUNSC_NETWORK="${RUNSC_NETWORK:-none}"
RUNSC_SYSTEMD_CGROUP="${RUNSC_SYSTEMD_CGROUP:-true}"
RUNSC_IGNORE_CGROUPS="${RUNSC_IGNORE_CGROUPS:-false}"
CONTAINERD_SOCKET_GID="${CONTAINERD_SOCKET_GID:-${SUDO_GID:-0}}"
mkdir -p /tmp/agentos-runsc
cat > "$RUNSC_CONFIG_PATH" <<EOF
log_path = "/tmp/agentos-runsc/%ID%/shim.log"
log_level = "debug"

[runsc_config]
  platform = "${RUNSC_PLATFORM}"
  network = "${RUNSC_NETWORK}"
  systemd-cgroup = "${RUNSC_SYSTEMD_CGROUP}"
  ignore-cgroups = "${RUNSC_IGNORE_CGROUPS}"
  debug = "true"
  debug-log = "/tmp/agentos-runsc/%ID%/gvisor.%COMMAND%.log"
EOF

cat > /etc/containerd/config.toml <<EOF
version = 3

[debug]
  level = "debug"

[grpc]
  gid = ${CONTAINERD_SOCKET_GID}

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc.options]
  TypeUrl = "io.containerd.runsc.v1.options"
  ConfigPath = "${RUNSC_CONFIG_PATH}"
EOF

# --- start containerd and wait for readiness -------------------------------
# Run an isolated daemon so the host's Docker/system containerd state cannot
# win a socket race or leak stale shims into this acceptance leg.
CONTAINERD_ADDRESS="${CONTAINERD_ADDRESS:-/run/agentos-containerd/containerd.sock}"
CONTAINERD_ROOT="${CONTAINERD_ROOT:-/var/lib/agentos-containerd}"
CONTAINERD_STATE="${CONTAINERD_STATE:-/run/agentos-containerd}"
export CONTAINERD_ADDRESS
mkdir -p "$CONTAINERD_ROOT" "$CONTAINERD_STATE"
echo ">> starting isolated containerd ${CONTAINERD_VERSION} at ${CONTAINERD_ADDRESS}"
nohup containerd \
  --address "$CONTAINERD_ADDRESS" \
  --root "$CONTAINERD_ROOT" \
  --state "$CONTAINERD_STATE" \
  --config /etc/containerd/config.toml \
  >/tmp/agentos-containerd.log 2>&1 &
CONTAINERD_PID=$!
for attempt in $(seq 1 30); do
  if timeout 5 ctr version >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$CONTAINERD_PID" 2>/dev/null; then
    echo "isolated containerd exited during startup; log:" >&2
    cat /tmp/agentos-containerd.log >&2 || true
    exit 1
  fi
  sleep 1
  if [ "$attempt" -eq 30 ]; then
    echo "containerd did not become ready; log:" >&2
    tail -n 50 /tmp/agentos-containerd.log >&2 || true
    exit 1
  fi
done
echo ">> containerd ready"
VERSION_OUTPUT="$(ctr version 2>&1 || true)"
if ! echo "$VERSION_OUTPUT" | grep -q "v${CONTAINERD_VERSION}"; then
  echo "containerd on the socket is not the pinned ${CONTAINERD_VERSION}; full version output:" >&2
  echo "$VERSION_OUTPUT" >&2
  exit 1
fi
echo ">> containerd on socket is pinned ${CONTAINERD_VERSION}"

# --- verify the runsc runtime end to end ------------------------------------
SNAPSHOTTER="${SNAPSHOTTER:-overlayfs}"
PROBE_IMAGE="${PROBE_IMAGE:-docker.io/library/busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0}"
echo ">> pulling digest-pinned probe image (bounded 300s)"
timeout 300 ctr -n "${AGENTOS_OCI_CONTAINERD_NAMESPACE:-agentos-ci}" images pull \
  --snapshotter "$SNAPSHOTTER" "$PROBE_IMAGE" >/dev/null
echo ">> running fail-closed runsc probe (bounded 120s)"
if ! timeout --signal=KILL 120 ctr -n "${AGENTOS_OCI_CONTAINERD_NAMESPACE:-agentos-ci}" run \
  --rm \
  --runtime io.containerd.runsc.v1 \
  --runtime-config-path "$RUNSC_CONFIG_PATH" \
  --snapshotter "$SNAPSHOTTER" \
  --read-only \
  --cap-drop ALL \
  --seccomp \
  --cpu-quota 100000 \
  --memory-limit 67108864 \
  "$PROBE_IMAGE" agentos-runtime-probe \
  /bin/true \
  >/tmp/probe-out.log 2>&1; then
  echo ">> runsc runtime probe: FAIL (the pinned matrix must be re-validated)" >&2
  echo ">> probe output:" >&2
  cat /tmp/probe-out.log 2>/dev/null >&2 || true
  echo ">> runsc/shim processes:" >&2
  # Keep ps here because state and wait-channel are part of the diagnostics.
  # shellcheck disable=SC2009
  ps -eo pid,ppid,user,stat,wchan,cmd | grep -E 'runsc|shim|ctr' | grep -v grep >&2 || true
  echo ">> containerd log tail:" >&2
  tail -n 200 /tmp/agentos-containerd.log >&2 || true
  echo ">> runsc/shim logs:" >&2
  find /tmp/agentos-runsc -type f -maxdepth 4 -print -exec tail -n 80 {} \; 2>/dev/null >&2 || true
  echo ">> dmesg tail:" >&2
  dmesg 2>/dev/null | tail -n 30 >&2 || true
  exit 1
fi
echo ">> runsc runtime probe: PASS (snapshotter=${SNAPSHOTTER}, platform=${RUNSC_PLATFORM})"
