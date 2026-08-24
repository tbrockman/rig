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
# Give gputest-a a local device named gpu0 first. This is the case that used to
# slip through: an instance-level gpu0 masks the profile's gpu0, so scanning
# instances' expanded devices reports all-clear. Claiming before the bad profile
# exists also keeps the test deterministic — after test 5 the winner is a race.
$GPUCTL claim gputest-a >/dev/null 2>&1
incus profile create gputest-bad 2>/dev/null || true
incus profile device add gputest-bad gpu0 gpu gputype=physical \
  pci="$GPUCTL_PCI" >/dev/null 2>&1

# No instance uses the profile yet. It is still primed to misconfigure the next
# instance created, so it must be rejected on its own.
if $GPUCTL status >/dev/null 2>&1; then
  fail "poisoned profile not detected while unused"
else
  pass "poisoned profile rejected while unused"
fi

incus profile add gputest-a gputest-bad >/dev/null 2>&1
if $GPUCTL status >/dev/null 2>&1; then
  fail "profile-borne GPU not detected (masked by the instance's own gpu0)"
else
  pass "profile-borne GPU rejected even when masked by a local gpu0"
fi
incus profile remove gputest-a gputest-bad >/dev/null 2>&1 || true
incus profile delete gputest-bad >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
hdr "7. start refuses an instance with no network isolation"
# The ACL is expected to come from the default profile. Override the NIC onto
# the instance and drop the ACL key to simulate someone creating an instance
# before the profile was fixed.
$GPUCTL release >/dev/null 2>&1
incus config device override gputest-a eth0 >/dev/null 2>&1
incus config device unset gputest-a eth0 security.acls >/dev/null 2>&1

if $GPUCTL start gputest-a >/dev/null 2>&1; then
  fail "start SUCCEEDED on an instance with no isolation ACL"
else
  pass "start refused an unisolated instance"
fi

# The isolation check runs before the claim, so a refusal must not have moved
# the card. Otherwise a refused start still perturbs GPU ownership.
HOLDERS=$($GPUCTL status --json | python3 -c \
  'import json,sys; print(len(json.load(sys.stdin)["holders"]))')
if [ "$HOLDERS" = "0" ]; then
  pass "refused start did not claim the GPU"
else
  fail "refused start left the GPU attached ($HOLDERS holders)"
fi

if $GPUCTL start gputest-a --allow-unisolated >/dev/null 2>&1; then
  pass "--allow-unisolated overrides the refusal"
  $GPUCTL stop gputest-a >/dev/null 2>&1
else
  fail "--allow-unisolated did not start the instance"
fi

# ---------------------------------------------------------------------------
printf '\n=== %d passed, %d failed ===\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
