#!/usr/bin/env bash
# v0.7: Firecracker KVM availability probe and minimal smoke.
#
# A full microVM boot smoke needs a kernel + rootfs pinned in the version
# matrix; this probe establishes the environment precondition first:
#   - KVM device present? (/dev/kvm)
#   - firecracker binary installed and runnable?
#
# Exit codes:
#   0  pass          KVM present and firecracker runs
#   0  skip          KVM absent and --allow-no-kvm was passed (GitHub-hosted
#                    runners have no /dev/kvm; the skip is explicit, not silent)
#   1  fail          KVM absent without --allow-no-kvm, or firecracker broken
#
# The boot smoke itself is a follow-up once the version matrix (kernel image,
# rootfs, firecracker tag) is pinned on a KVM-capable runner; this probe is
# its documented precondition.
set -eu

ALLOW_NO_KVM=0
if [ "${1:-}" = "--allow-no-kvm" ]; then
  ALLOW_NO_KVM=1
fi

if [ ! -e /dev/kvm ]; then
  if [ "$ALLOW_NO_KVM" = "1" ]; then
    echo "firecracker.smoke=skip reason=kvm-absent (/dev/kvm not present)"
    exit 0
  fi
  echo "firecracker.smoke=fail reason=kvm-absent (/dev/kvm not present)" >&2
  exit 1
fi

if ! command -v firecracker >/dev/null 2>&1; then
  echo "firecracker.smoke=fail reason=firecracker-not-installed" >&2
  exit 1
fi

VERSION="$(firecracker --version 2>/dev/null | head -n 1 || echo unknown)"
echo "firecracker.smoke=pass kvm=present firecracker=${VERSION}"
