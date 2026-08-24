#!/usr/bin/env bash
# Build the base image declaratively and import it into Incus.
# No `incus exec`, no `incus publish`, no mutated state.
#
#   ./01-build-image.sh [alias]

set -euo pipefail

ALIAS="${1:-nixos-gpu-base}"
HERE="$(cd "$(dirname "$0")" && pwd)"

cp "$HERE/guest/gpu-dev.nix" "$HERE/base/gpu-dev.nix"

echo "=== building qcow2 disk (slow the first time) ==="
DISK=$(nix build --no-link --print-out-paths \
  "path:$HERE/base#nixosConfigurations.gpubase.config.system.build.qemuImage")

echo "=== building metadata tarball ==="
META=$(nix build --no-link --print-out-paths \
  "path:$HERE/base#nixosConfigurations.gpubase.config.system.build.metadata")

DISK_FILE="$DISK/nixos.qcow2"
META_FILE="$META/tarball/nixos-system-x86_64-linux.tar.xz"

# Fall back to a search if the layout ever shifts.
[ -f "$DISK_FILE" ] || DISK_FILE=$(find "$DISK" -name '*.qcow2' | head -1)
[ -f "$META_FILE" ] || META_FILE=$(find "$META" -name '*.tar.xz' | head -1)

[ -f "$DISK_FILE" ] || { echo "no qcow2 produced under $DISK" >&2; exit 1; }
[ -f "$META_FILE" ] || { echo "no metadata tarball under $META" >&2; exit 1; }

echo "  disk:     $DISK_FILE"
echo "  metadata: $META_FILE"

echo "=== importing as $ALIAS ==="
incus image delete "$ALIAS" 2>/dev/null || true
incus image import "$META_FILE" "$DISK_FILE" --alias "$ALIAS"
incus image list "$ALIAS"

cat <<EOF

Smoke test:
  incus init $ALIAS smoke --vm -c security.secureboot=false \\
    -c limits.cpu=8 -c limits.memory=16GiB
  export GPUCTL_PCI=0000:04:00.0
  ./gpuctl start smoke && sleep 30
  incus exec smoke -- gpu-check
  ./gpuctl stop smoke && ./gpuctl release && incus delete -f smoke
EOF