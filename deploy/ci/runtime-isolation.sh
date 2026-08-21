#!/usr/bin/env bash
# Assert the security boundary of the real containerd + gVisor runtime. These
# checks execute inside the sandbox; a successful process launch alone is not
# accepted as proof that the requested OCI isolation controls took effect.
set -euo pipefail

command -v timeout >/dev/null 2>&1 || { echo "coreutils timeout is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
: "${CONTAINERD_ADDRESS:?CONTAINERD_ADDRESS is required}"
: "${AGENTOS_OCI_IMAGE:?AGENTOS_OCI_IMAGE must be a digest-pinned image}"

NAMESPACE="${AGENTOS_OCI_CONTAINERD_NAMESPACE:-agentos-ci}"
RUNTIME="${AGENTOS_OCI_RUNTIME:-io.containerd.runsc.v1}"
RUNTIME_CONFIG="${AGENTOS_OCI_RUNTIME_CONFIG:-/etc/containerd/runsc.toml}"
SNAPSHOTTER="${AGENTOS_OCI_SNAPSHOTTER:-overlayfs}"
PROBE_ID="agentos-isolation-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}-$$"
HOST_PROBE_ID="agentos-host-isolation-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}-$$"
ORPHAN_ID="agentos-orphan-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}-$$"

cleanup() {
  timeout 15 ctr -n "$NAMESPACE" tasks delete --force "$PROBE_ID" >/dev/null 2>&1 || true
  timeout 15 ctr -n "$NAMESPACE" containers delete "$PROBE_ID" >/dev/null 2>&1 || true
  timeout 15 ctr -n "$NAMESPACE" tasks delete --force "$HOST_PROBE_ID" >/dev/null 2>&1 || true
  timeout 15 ctr -n "$NAMESPACE" containers delete "$HOST_PROBE_ID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Expansion is intentionally deferred until /bin/sh runs inside the sandbox.
# shellcheck disable=SC2016
probe='set -eu
grep -qi gvisor /proc/version
test "$(awk "/^CapEff:/{print \$2}" /proc/self/status)" = "0000000000000000"
test "$(awk "/^NoNewPrivs:/{print \$2}" /proc/self/status)" = "1"
test "$(awk "/^Seccomp:/{print \$2}" /proc/self/status)" = "2"
if touch /agentos-rootfs-must-remain-read-only 2>/dev/null; then
  echo "root filesystem is writable" >&2
  exit 1
fi
dd if=/dev/zero of=/agentos/workspace/within-limit bs=1024 count=1 2>/dev/null
if dd if=/dev/zero of=/agentos/workspace/over-limit bs=1048576 count=9 2>/dev/null; then
  echo "workspace tmpfs exceeded its 8 MiB limit" >&2
  exit 1
fi
test "$(ls -1 /sys/class/net | sort | tr "\n" " ")" = "lo "
echo "gvisor identity, read-only rootfs, bounded workspace, zero capabilities, no-new-privileges, seccomp, and network isolation: PASS"'

echo ">> asserting OCI/gVisor sandbox isolation (bounded 120s)"
if ! timeout --signal=KILL 120 ctr -n "$NAMESPACE" run \
  --rm \
  --runtime "$RUNTIME" \
  --runtime-config-path "$RUNTIME_CONFIG" \
  --snapshotter "$SNAPSHOTTER" \
  --read-only \
  --cap-drop ALL \
  --seccomp \
  --cpu-quota 100000 \
  --memory-limit 67108864 \
  --mount type=tmpfs,dst=/agentos/workspace,options=size=8388608 \
  "$AGENTOS_OCI_IMAGE" "$PROBE_ID" \
  /bin/sh -c "$probe"; then
  echo ">> OCI/gVisor isolation assertion: FAIL" >&2
  echo ">> containerd log tail:" >&2
  tail -n 120 /tmp/agentos-containerd.log >&2 || true
  echo ">> runsc/shim logs:" >&2
  find /tmp/agentos-runsc -maxdepth 4 -type f -print -exec tail -n 80 {} \; 2>/dev/null >&2 || true
  exit 1
fi

echo ">> OCI/gVisor isolation assertion: PASS"

# Keep a sandbox alive long enough to validate the host-side OCI contract: the
# limits must be present in the stored spec, and the sandbox process must run
# in both a non-host user namespace and a dedicated cgroup.
echo ">> asserting host-side namespace, cgroup, and resource isolation"
timeout 120 ctr -n "$NAMESPACE" run \
  --detach \
  --runtime "$RUNTIME" \
  --runtime-config-path "$RUNTIME_CONFIG" \
  --snapshotter "$SNAPSHOTTER" \
  --read-only \
  --cap-drop ALL \
  --seccomp \
  --cpu-quota 100000 \
  --memory-limit 67108864 \
  "$AGENTOS_OCI_IMAGE" "$HOST_PROBE_ID" \
  /bin/sleep 120

timeout 15 ctr -n "$NAMESPACE" containers info --spec "$HOST_PROBE_ID" |
  jq -e '.linux.resources.cpu.quota == 100000 and .linux.resources.memory.limit == 67108864' >/dev/null
TASK_PID="$(ctr -n "$NAMESPACE" tasks list | awk -v id="$HOST_PROBE_ID" '$1 == id { print $2 }')"
if [ -z "$TASK_PID" ] || [ ! -r "/proc/${TASK_PID}/status" ]; then
  echo "cannot resolve live gVisor sandbox PID" >&2
  exit 1
fi
if [ "$(readlink "/proc/${TASK_PID}/ns/user")" = "$(readlink /proc/1/ns/user)" ]; then
  echo "gVisor sandbox shares the host user namespace" >&2
  exit 1
fi
if cmp -s "/proc/${TASK_PID}/cgroup" /proc/1/cgroup; then
  echo "gVisor sandbox shares the host root cgroup" >&2
  exit 1
fi
echo ">> host-side namespace, cgroup, and resource isolation: PASS"
timeout 30 ctr -n "$NAMESPACE" tasks delete --force "$HOST_PROBE_ID" >/dev/null
timeout 30 ctr -n "$NAMESPACE" containers delete "$HOST_PROBE_ID" >/dev/null

# Leave a namespaced provider-owned container without a task. The subsequent
# conformance execution must reap it during Prepare, proving crash recovery
# against real containerd state rather than only a mocked ID list.
echo ">> creating orphan-recovery fixture ${ORPHAN_ID}"
timeout 60 ctr -n "$NAMESPACE" containers create \
  --snapshotter "$SNAPSHOTTER" \
  "$AGENTOS_OCI_IMAGE" "$ORPHAN_ID"
