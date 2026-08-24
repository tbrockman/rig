#!/usr/bin/env bash
# Phase 0 spike: verify the NixOS-guest assumptions before building on them.
#
# The question that matters: does `incus exec` work in a NixOS VM? If not, the
# control-plane transport changes from the Incus agent to SSH, and gpuctl plus
# test-invariants.sh need reworking.
#
# Safe to re-run. Creates and destroys an instance named `spike`.

set -uo pipefail

RELEASE="${NIXOS_RELEASE:-26.05}"

say() { printf '\n=== %s ===\n' "$*"; }
ok()  { printf '  [ok]   %s\n' "$*"; }
bad() { printf '  [FAIL] %s\n' "$*"; }

say "1. Incus version and storage backend"
incus --version
POOL=$(incus profile device get default root pool 2>/dev/null || echo "?")
DRIVER=$(incus storage show "$POOL" 2>/dev/null | awk '/^driver:/{print $2}')
echo "  default pool: $POOL (driver: ${DRIVER:-unknown})"
case "$DRIVER" in
  zfs|btrfs|lvm) ok "CoW-capable pool — incus copy will be a cheap clone" ;;
  dir)  bad "dir pool: every project clone is a full copy. See RUNBOOK step 0.5." ;;
  *)    bad "could not determine pool driver" ;;
esac

say "2. NixOS VM images available"
incus image list images:nixos type=virtual-machine 2>/dev/null \
  || incus image list images:nixos
echo "  using release: $RELEASE (override with NIXOS_RELEASE=...)"

say "3. Launch a NixOS VM"
incus delete -f spike 2>/dev/null
if ! incus launch "images:nixos/$RELEASE" spike --vm \
     -c limits.cpu=4 -c limits.memory=4GiB -d root,size=20GiB \
     -c security.secureboot=false; then
  bad "launch failed — check the release exists in the list above"
  exit 1
fi
say "4. CRITICAL: does incus-agent come up? (decides the transport)"
# A hung `incus exec` waiting on the agent ignores SIGTERM, so kill hard and
# poll rather than blocking on a single long call.
AGENT=no
for i in $(seq 1 24); do   # 24 x 5s = 2 minutes
  if timeout -s KILL 5 incus exec spike -- true </dev/null >/dev/null 2>&1; then
    AGENT=yes
    ok "incus exec works after ~$((i*5))s — gpuctl can use the agent"
    break
  fi
  printf '  waiting for agent... %ds\r' $((i*5))
done
echo

if [ "$AGENT" = no ]; then
  bad "agent did not come up within 120s"
  echo
  echo "  --- instance state ---"
  incus list spike
  echo "  --- last 40 lines of console log ---"
  # cat -v renders escapes literally so raw console output cannot
  # corrupt the terminal (run `reset` if it already has).
  incus console --show-log spike 2>/dev/null | tail -40 | cat -v
  echo
  echo "  Read the log above:"
  echo "    * still printing kernel/systemd lines -> just slow, re-run"
  echo "    * reached a login prompt              -> agent problem, use SSH"
  echo "    * panic / emergency shell             -> image or boot problem"
fi

say "5. Does the guest get an IP?"
incus list spike -c ns4

say "6. Guest version and rebuild capability"
if [ "$AGENT" = yes ]; then
  timeout -s KILL 30 incus exec spike -- nixos-version </dev/null
  if timeout -s KILL 60 incus exec spike -- nixos-rebuild --help </dev/null >/dev/null 2>&1; then
    ok "nixos-rebuild present (used for in-guest iteration, not image builds)"
  else
    bad "nixos-rebuild missing"
  fi
else
  echo "  skipped (no agent)"
fi

say "7. Host: GPU location and current driver"
lspci -nnk | grep -A3 -iE '\[10de:' || bad "no NVIDIA device found"

say "RESULT"
echo "  agent transport usable: $AGENT"
echo "  storage driver:         ${DRIVER:-unknown}"
echo
echo "  Clean up with: incus delete -f spike"