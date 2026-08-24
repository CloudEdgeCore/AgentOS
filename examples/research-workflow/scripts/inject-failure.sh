#!/usr/bin/env bash
# Inject a failure into the running research deployment.
#
#   scripts/inject-failure.sh crash  [research-worker-00]  SIGKILL a runtime
#       adapter mid-flight; lease-expiry recovery requeues its attempts and
#       any surviving/restarted instance finishes them (the recovery path the
#       e2e suite exercises).
#
#   scripts/inject-failure.sh cordon <pool-id>            drain a pool via the
#       durable registry so placement stops there without killing leases.
#
#   scripts/inject-failure.sh uncordon <pool-id>          re-admit the pool.
set -euo pipefail

KIND="${1:?usage: inject-failure.sh crash|cordon|uncordon [target]}"
TARGET="${2:-}"

case "$KIND" in
  crash)
    INSTANCE="${TARGET:-research-worker-00}"
    echo "[inject] SIGKILL runtime adapter $INSTANCE"
    pkill -9 -f "runtime-instance-id $INSTANCE" || {
      echo "[inject] no process matched '$INSTANCE'"; exit 1; }
    ;;
  cordon|uncordon)
    POOL="${TARGET:?pool id required}"
    STATUS="$([ "$KIND" = cordon ] && echo CORDONED || echo ACTIVE)"
    : "${DATABASE_URL:?set DATABASE_URL for direct registry updates}"
    echo "[inject] setting pool $POOL status=$STATUS"
    psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
      -c "UPDATE runtime_pools SET status='$STATUS', updated_at=now() WHERE id='$POOL'"
    ;;
  *)
    echo "unknown failure kind: $KIND" >&2; exit 2;;
esac
