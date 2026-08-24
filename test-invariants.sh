#!/usr/bin/env bash
# Invariant tests for gpuctl. Runs against REAL Incus — the behaviour being
# guarded lives in VFIO and QEMU, and a mock will cheerfully tell you
# everything is fine.
#
#   export GPUCTL_PCI=0000:04:00.0
#   ./test-invariants.sh <base-image-alias>
#
# Creates instances gputest-a / gputest-b and deletes them at the end.

set -uo pipefail

IMAGE="${1:-nixos-gpu-base}"
GPUCTL="${GPUCTL:-./gpuctl}"
PASS=0; FAIL=0

pass() { echo "  PASS: $*"; PASS=$((PASS+1)); }
fail() { echo "  FAIL: $*"; FAIL=$((FAIL+1)); }
hdr()  { printf '\n--- %s ---\n' "$*"; }

: "${GPUCTL_PCI:?set GPUCTL_PCI first, e.g. 0000:04:00.0}"

cleanup() {
  hdr "cleanup"
  incus delete -f gputest-a gputest-b 2>/dev/null || true
}
trap cleanup EXIT

hdr "setup"
incus delete -f gputest-a gputest-b 2>/dev/null || true
incus init "$IMAGE" gputest-a --vm -c limits.cpu=4 -c limits.memory=8GiB \
  -c security.secureboot=false
incus init "$IMAGE" gputest-b --vm -c limits.cpu=4 -c limits.memory=8GiB \
  -c security.secureboot=false
echo "created gputest-a, gputest-b"

# ---------------------------------------------------------------------------
hdr "1. claim on a stopped instance attaches the device"
$GPUCTL claim gputest-a >/dev/null
if incus config device get gputest-a gpu0 pci 2>/dev/null | grep -q .; then
  pass "device present on gputest-a"
else
  fail "device missing after claim"
fi

# ---------------------------------------------------------------------------
hdr "2. migration between two STOPPED instances is allowed"
if $GPUCTL claim gputest-b >/dev/null 2>&1; then
  if incus config device get gputest-b gpu0 pci >/dev/null 2>&1 \
     && ! incus config device get gputest-a gpu0 pci >/dev/null 2>&1; then
    pass "device moved a -> b, and only b has it"
  else
    fail "device ended up on both or neither"
  fi
else
  fail "claim refused between two stopped instances"
fi

# ---------------------------------------------------------------------------
hdr "3. THE BIG ONE: cannot steal the GPU from a RUNNING instance"
$GPUCTL start gputest-b >/dev/null
sleep 20

if $GPUCTL claim gputest-a >/dev/null 2>&1; then
  fail "claim SUCCEEDED against a running holder — invariant violated"
else
  pass "claim refused while holder is running"
fi

# The victim must still be healthy. Asserting only the refusal is not enough:
# the failure mode we care about is silent damage to the running instance.
if incus exec gputest-b -- gpu-check >/dev/null 2>&1; then
  pass "gputest-b still has a working GPU"
else
  fail "gputest-b's GPU is gone or unhealthy after the refused claim"
fi

# ---------------------------------------------------------------------------
hdr "4. release refuses a running holder without --force"
if $GPUCTL release >/dev/null 2>&1; then
  fail "release detached from a running instance without --force"
else
  pass "release refused"
fi

# ---------------------------------------------------------------------------
hdr "5. concurrent claims serialise (exactly one winner)"
$GPUCTL stop gputest-b >/dev/null
( $GPUCTL claim gputest-a >/tmp/gc-a 2>&1 ) &
( $GPUCTL claim gputest-b >/tmp/gc-b 2>&1 ) &
wait
HOLDERS=$($GPUCTL status --json | python3 -c \
  'import json,sys; print(len(json.load(sys.stdin)["holders"]))')
if [ "$HOLDERS" = "1" ]; then
  pass "exactly one holder after concurrent claims"
else
  fail "ended with $HOLDERS holders (expected 1)"
fi

# ---------------------------------------------------------------------------
hdr "6. a GPU in a profile is rejected outright"
incus profile create gputest-bad 2>/dev/null || true
incus profile device add gputest-bad gpu0 gpu gputype=physical \
  pci="$GPUCTL_PCI" >/dev/null 2>&1
incus profile add gputest-a gputest-bad >/dev/null 2>&1
if $GPUCTL status >/dev/null 2>&1; then
  fail "profile-borne GPU was not detected"
else
  pass "profile-borne GPU rejected"
fi
incus profile remove gputest-a gputest-bad >/dev/null 2>&1 || true
incus profile delete gputest-bad >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
printf '\n=== %d passed, %d failed ===\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
