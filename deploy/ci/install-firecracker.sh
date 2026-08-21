#!/usr/bin/env bash
# Install the checksum-pinned Firecracker release used by the Linux runtime
# acceptance leg. The archive is verified before either binary is installed.
set -euo pipefail

command -v timeout >/dev/null 2>&1 || { echo "coreutils timeout is required" >&2; exit 1; }

FIRECRACKER_VERSION="${FIRECRACKER_VERSION:?set a pinned Firecracker version (e.g. v1.9.0)}"
FIRECRACKER_SHA256="${FIRECRACKER_SHA256:?set the approved Firecracker archive SHA-256}"

case "$(uname -m)" in
  x86_64) FIRECRACKER_ARCH="x86_64" ;;
  *) echo "unsupported Firecracker architecture: $(uname -m)" >&2; exit 1 ;;
esac

archive="firecracker-${FIRECRACKER_VERSION}-${FIRECRACKER_ARCH}.tgz"
release_dir="release-${FIRECRACKER_VERSION}-${FIRECRACKER_ARCH}"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

echo ">> installing Firecracker ${FIRECRACKER_VERSION} (${FIRECRACKER_ARCH})"
timeout 300 curl -fsSL -o "$tmp_dir/$archive" \
  "https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/${archive}"
echo "${FIRECRACKER_SHA256}  $tmp_dir/$archive" | sha256sum -c -
tar -xzf "$tmp_dir/$archive" -C "$tmp_dir"

for binary in firecracker jailer; do
  source_path="$tmp_dir/$release_dir/${binary}-${FIRECRACKER_VERSION}-${FIRECRACKER_ARCH}"
  if [ ! -x "$source_path" ]; then
    echo "verified Firecracker archive is missing executable: $source_path" >&2
    exit 1
  fi
  install -m 0755 "$source_path" "/usr/local/bin/$binary"
done

version_output="$(firecracker --version 2>&1)"
echo "$version_output"
if ! grep -Fq "Firecracker ${FIRECRACKER_VERSION}" <<<"$version_output"; then
  echo "installed Firecracker does not match pinned release ${FIRECRACKER_VERSION}" >&2
  exit 1
fi
