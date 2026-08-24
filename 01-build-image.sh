#!/usr/bin/env bash
# Build the base image declaratively and import it into Incus.
# No `incus exec`, no `incus publish`, no mutated state.
#
#   ./01-build-image.sh [alias]

set -euo pipefail

ALIAS="${1:-nixos-gpu-base}"
HERE="$(cd "$(dirname "$0")" && pwd)"

# The guest module lives in base/ alongside the flake that imports it. It used
# to be kept in guest/ and copied here on every build, which meant two committed
# copies of the same file waiting to drift apart.

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
  ./rig new smoke
  ./rig start smoke
  incus exec smoke -- gpu-check
  ./rig stop smoke && ./rig rm smoke
EOF