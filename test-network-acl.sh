#!/usr/bin/env bash
# Regression test for the guest network isolation ACL.
#
#   ./test-network-acl.sh <instance>
#
# The instance must be RUNNING. Exits non-zero on any mismatch.
#
# The rule this script is built around: a connection that fails because nothing
# is listening is not evidence of blocking. Every negative test therefore proves
# the target is reachable FROM THE HOST first, and SKIPS — never passes — when
# it is not. A silently skipped test that reports success is worse than no test,
# so skips are counted and printed separately, and the summary line says so.
#
# Targets are derived from the live host (default route, bridge address,
# tailnet, docker) rather than hardcoded, because an IP that rots turns a real
# test into a permanent skip.

set -uo pipefail

INST="${1:?usage: ./test-network-acl.sh <instance>}"
BRIDGE_NET="${BRIDGE_NET:-incusbr0}"

PASS=0; FAIL=0; SKIP=0

# A minimal DNS query for example.com. Used instead of dig because the guest
# image has no dig, and because a UDP test needs to distinguish "answered" from
# "silence", which only a real query can do.
DNSQ='\xaa\xaa\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01'

gexec() { timeout -s KILL 30 incus exec "$INST" -- "$@" </dev/null 2>/dev/null; }

# --- probes ---------------------------------------------------------------
# Each returns 0 for "reachable". bash's /dev/tcp is used rather than nc on the
# host side so the test does not depend on which netcat is installed.

# `incus exec ... -- bash -c` runs with a stub PATH (/usr/bin:/bin and friends,
# none of which exist on NixOS). Every guest probe therefore uses `bash -lc`, or
# `timeout` and `nc` come back "command not found" and the probe reports the
# target as blocked when it is wide open — a false PASS on exactly the tests
# that matter.
host_tcp()   { timeout 4 bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null; }
guest_tcp()  { gexec bash -lc "timeout 4 bash -c 'exec 3<>/dev/tcp/$1/$2'"; }

host_icmp()  { ping -n -c2 -W2 "$1" >/dev/null 2>&1; }
guest_icmp() { gexec ping -n -c2 -W2 "$1" >/dev/null; }

host_dns()   { local n; n=$(printf "$DNSQ" | timeout 6 nc -u -w 3 "$1" 53 2>/dev/null | wc -c); [ "${n:-0}" -gt 0 ]; }
guest_dns()  { local n; n=$(gexec bash -lc "printf '$DNSQ' | timeout 6 nc -u -w 3 $1 53 | wc -c"); [ "${n:-0}" -gt 0 ]; }

probe() { # probe <kind> <host|guest> <target> [port]
  case "$1-$2" in
    tcp-host)   host_tcp  "$3" "$4" ;;
    tcp-guest)  guest_tcp "$3" "$4" ;;
    icmp-host)  host_icmp "$3" ;;
    icmp-guest) guest_icmp "$3" ;;
    dns-host)   host_dns  "$3" ;;
    dns-guest)  guest_dns "$3" ;;
    *) echo "internal: bad probe $1-$2" >&2; return 1 ;;
  esac
}

# check <label> <kind> <target> <port> <expect allow|block> <prove-from-host yes|no>
check() {
  local label="$1" kind="$2" tgt="$3" port="$4" expect="$5" prove="$6"

  if [ -z "$tgt" ]; then
    printf '  SKIP  %-44s (no target could be derived on this host)\n' "$label"
    SKIP=$((SKIP+1)); return
  fi

  if [ "$prove" = yes ] && ! probe "$kind" host "$tgt" "$port"; then
    printf '  SKIP  %-44s (host cannot reach it either — proves nothing)\n' "$label"
    SKIP=$((SKIP+1)); return
  fi

  local observed
  if probe "$kind" guest "$tgt" "$port"; then observed=reachable; else observed=blocked; fi

  local want; [ "$expect" = allow ] && want=reachable || want=blocked
  if [ "$observed" = "$want" ]; then
    printf '  PASS  %-44s %s\n' "$label" "$observed"
    PASS=$((PASS+1))
  else
    printf '  FAIL  %-44s expected %s, got %s\n' "$label" "$want" "$observed"
    FAIL=$((FAIL+1))
  fi
}

# check_guest <label> <expect pass|fail> <command...>  — run in the guest
check_guest() {
  local label="$1" expect="$2"; shift 2
  local observed
  if gexec bash -lc "$*"; then observed=ok; else observed=failed; fi
  local want; [ "$expect" = pass ] && want=ok || want=failed
  if [ "$observed" = "$want" ]; then
    printf '  PASS  %-44s %s\n' "$label" "$observed"
    PASS=$((PASS+1))
  else
    printf '  FAIL  %-44s expected %s, got %s\n' "$label" "$want" "$observed"
    FAIL=$((FAIL+1))
  fi
}

# --- target discovery -----------------------------------------------------

if ! incus info "$INST" >/dev/null 2>&1; then
  echo "no such instance: $INST" >&2; exit 2
fi
if [ "$(incus list "$INST" -c s -f csv)" != "RUNNING" ]; then
  echo "$INST is not running" >&2; exit 2
fi
if ! gexec true; then
  echo "cannot reach the agent in $INST" >&2; exit 2
fi

BRIDGE=$(incus network get "$BRIDGE_NET" ipv4.address 2>/dev/null | cut -d/ -f1)
GW=$(ip route show default | awk '{print $3; exit}')

# A LAN peer that is not the gateway: the neighbour table, filtered to ones the
# host can actually ping.
LAN_IF=$(ip route show default | awk '{print $5; exit}')
LANPEER=""
for cand in $(ip -4 neigh show dev "$LAN_IF" 2>/dev/null | awk '{print $1}'); do
  [ "$cand" = "$GW" ] && continue
  if host_icmp "$cand"; then LANPEER="$cand"; break; fi
done

# A tailnet peer that is currently online. 100.64.0.0/10 is not covered by
# RFC1918 and was a real gap once; keep testing it.
TSPEER=$(tailscale status 2>/dev/null \
         | awk '$1 ~ /^100\./ && $0 !~ /offline/ {print $1; exit}')

# Every host address that falls inside a blocked range. The host's sshd listens
# on 0.0.0.0, so each of these answers on port 22 — which makes them the
# strongest negative targets available:
#
#   - they cover all five blocked ranges at once (bridge 10/8, docker bridges
#     172.16/12, LAN 192.168/16, and the host's own tailnet address 100.64/10),
#   - reaching the host is the breach that actually matters, and
#   - traffic to a host address is INPUT, not FORWARD, so nothing else on this
#     machine is silently doing the blocking for us. A pass here is the ACL.
HOST_ADDRS=$(ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1 |
  awk -F. '($1==10) || ($1==172 && $2>=16 && $2<=31) || ($1==192 && $2==168) ||
           ($1==169 && $2==254) || ($1==100 && $2>=64 && $2<=127)' | sort -u)

# Docker bridges live in 172.16.0.0/12 and usually have something listening.
# Caveat, and the reason these are not the primary evidence: Docker installs its
# own FORWARD rules that already drop traffic arriving from another bridge, so
# these targets test the outcome (the guest cannot reach them) but do NOT
# attribute it to the ACL — they stay blocked with the ACL removed.
DOCKER_TARGETS=()
if command -v docker >/dev/null 2>&1; then
  while read -r cid; do
    [ -n "$cid" ] || continue
    read -r dip dports < <(docker inspect -f \
      '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}} {{range $p, $_ := .NetworkSettings.Ports}}{{$p}} {{end}}' \
      "$cid" 2>/dev/null)
    [ -n "${dip:-}" ] || continue
    for pp in ${dports:-}; do
      case "$pp" in *?/tcp) DOCKER_TARGETS+=("$dip:${pp%/tcp}") ;; esac
    done
  done < <(docker ps -q 2>/dev/null)
fi

echo "instance     : $INST"
echo "bridge       : ${BRIDGE:-<none>}"
echo "lan gateway  : ${GW:-<none>}"
echo "lan peer     : ${LANPEER:-<none found>}"
echo "tailnet peer : ${TSPEER:-<none online>}"
echo "host addrs   : $(echo $HOST_ADDRS | tr '\n' ' ')"
echo "docker       : ${#DOCKER_TARGETS[@]} candidate target(s)"

# --------------------------------------------------------------------------
echo
echo "--- the host itself must be unreachable on every address it holds ---"
echo "    (sshd listens on 0.0.0.0; this is the breach that matters)"

for addr in $HOST_ADDRS; do
  check "host $addr tcp/22" tcp "$addr" 22 block yes
done

# --------------------------------------------------------------------------
echo
echo "--- must be BLOCKED (each proven reachable from the host first) ---"

check "LAN gateway  tcp/80"        tcp  "$GW"      80   block yes
check "LAN gateway  icmp"          icmp "$GW"      ""   block yes
check "LAN peer     icmp"          icmp "$LANPEER" ""   block yes
check "tailnet peer icmp"          icmp "$TSPEER"  ""   block yes
check "LAN gateway  udp/53 (DNS)"  dns  "$GW"      ""   block yes
check "link-local 169.254.169.254" tcp  "169.254.169.254" 80 block yes

# Outcome-only: see the DOCKER_TARGETS comment above. Kept because the property
# is worth regressing, but never cite these as evidence the ACL works.
for t in "${DOCKER_TARGETS[@]:-}"; do
  [ -n "$t" ] || continue
  check "docker ctr $t (not ACL-attributable)" tcp "${t%:*}" "${t#*:}" block yes
done

# --------------------------------------------------------------------------
echo
echo "--- must be ALLOWED (the guest is useless otherwise) ---"

check "public IPv4  tcp/443 (1.1.1.1)" tcp "1.1.1.1" 443 allow yes
check_guest "DNS resolution"            pass "getent hosts cache.nixos.org >/dev/null"
check_guest "public HTTPS"              pass "curl -sSf -m 20 -o /dev/null https://cache.nixos.org/nix-cache-info"
# `nix store info --store https://cache.nixos.org` is NOT a network test — it
# answers from nix's local cache and passed while HTTPS was fully blocked. A
# cache-busting query string forces a real fetch every run, because the URL has
# never been seen before and so cannot be in the store.
check_guest "nix fetches from the cache" pass \
  'nix store prefetch-file --json "https://cache.nixos.org/nix-cache-info?acltest=$RANDOM$RANDOM" >/dev/null'

# The bridge resolver is a deliberate, scoped exception: Incus inserts its
# DHCP/DNS rules ahead of ACL rules, so port 53 on the bridge stays reachable
# even though the rest of 10.0.0.0/8 is rejected. That is the intended
# behaviour — the pairing with the tcp/22 test above is what makes it a scoped
# exception rather than a hole.
check "bridge resolver udp/53" dns "$BRIDGE" "" allow yes

# --------------------------------------------------------------------------
echo
echo "--- bypass checks ---"

# IPv4-only rules are only sufficient because the guest has no IPv6 stack at
# all. If a global v6 address ever appears, every rule above is bypassable and
# this is the test that says so.
check_guest "no global IPv6 address in guest" pass \
  '[ -z "$(ip -6 addr show scope global 2>/dev/null)" ]'
check_guest "no IPv6 default route in guest"  pass \
  '[ -z "$(ip -6 route show default 2>/dev/null)" ]'

# --------------------------------------------------------------------------
echo
printf '=== %d passed, %d failed, %d skipped ===\n' "$PASS" "$FAIL" "$SKIP"
if [ "$SKIP" -gt 0 ]; then
  echo "note: skipped tests are NOT passes — their targets were unreachable"
  echo "      from the host, so guest failure would have proven nothing."
fi
[ "$FAIL" -eq 0 ]
