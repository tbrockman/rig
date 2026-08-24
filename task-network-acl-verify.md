# Task: verify and prove out the VM network isolation ACL

> **Executed 2026-08-24.** Findings are in the "Network isolation" section of
> `STATUS.md`; the phase 5 script is `test-network-acl.sh`. Headline: the ACL
> existed but was attached to nothing, and attaching it flips the NIC to
> default-reject in both directions, so a denylist ACL needs
> `security.acls.default.egress.action=allow` to mean what it says. One item was
> not completed — the nftables dump needs a sudo password.

Audit the Incus network ACL applied to the GPU dev VMs and **prove the policy
empirically** rather than asserting it from the config. IPv4 only — see phase 2.

Read `STATUS.md` first for context. The intended posture: the guest reaches the
public internet, and cannot reach the LAN, the Incus bridge, link-local, or the
Tailscale network.

## Rules

- **Do not change host networking, SSH config, or host firewall rules.** The
  operator is connected over SSH. Incus network and ACL objects are in scope;
  host `ufw`/`nftables` rules are not.
- Do not disable or weaken an existing block to make a test pass.
- Report findings you cannot fix rather than working around them.
- Stop at each **CHECKPOINT** and wait.

## Phase 1 — Establish ground truth

Do not assume the ACL from earlier work is still applied or correct.

```bash
incus network acl list
incus network acl show vm-isolate          # or whatever exists
incus network list
incus network show incusbr0
incus config device show <instance>        # is the ACL on the NIC?
```

Report:

- Which ACLs exist, their exact rules, and which NICs they are attached to.
- `security.acls.default.ingress.action` / `.egress.action` on each NIC — is
  this a denylist with default-allow, or default-deny with an allowlist?
- Whether `ipv6.address` is set to `none` on the bridge.
- Whether `dns.nameservers` is set. **Known Incus 6.0.5 bug:** combined with
  `security.acls` it emits the cross product of nameservers x address families,
  producing invalid nftables rules. If it is set, flag it.

Confirm the known-no-IPv6 state still holds (expected: no default v6 route, no
global v6 address). If any of this has changed, stop and report — it would mean
the ISP started delegating, which changes the plan:

```bash
ip -6 route show default
ip -6 addr show scope global
ping6 -c2 2606:4700:4700::1111
curl -6 -sI --max-time 5 https://cloudflare.com
```

**CHECKPOINT 1.** Report.

## Phase 2 — Design the rule set

IPv4 reject list (verify all are present):

    10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 100.64.0.0/10

**IPv6 is out of scope.** The ISP does not delegate a prefix (see the "Parked"
section of `STATUS.md`), so `ipv6.address=none` stays set on `incusbr0`. Do not
enable bridge IPv6, do not design IPv6 rules, and do not spend time on ISP
prefix tracking. Confirm in phase 1 that `ipv6.address` is still `none` and
report if it is not.

The IPv6 rules that *would* be needed, and the reasoning for the chosen
approach, are recorded in `STATUS.md` for whenever the ISP situation changes.

**CHECKPOINT 2.** Present the proposed rules and reasoning. Wait for approval
before applying anything.

## Phase 3 — Apply

Apply approved rules to a **test instance only**, not to any instance the
operator is using. Create one from `nixos-gpu-base` if needed.

## Phase 4 — Prove it (the important part)

A connection that fails because nothing is listening is **not** evidence of
blocking. Every negative test must target something **known reachable from the
host**, so that failure from the guest is meaningful.

Build the target list by first proving reachability from the host:

- the LAN gateway (from `ip route`)
- a live LAN peer that answers on some port
- the Incus bridge address
- a live tailnet peer (`tailscale status`) — its `100.x` address
- the bridge's own link-local address

For each: confirm the host reaches it, then confirm the guest does not. Record
both. A test where the host also fails proves nothing — discard it and pick
another target.

Positive tests, which must all pass:

- public IPv4 HTTP/HTTPS
- DNS resolution
- `nix` can fetch from cache.nixos.org (the guest is useless otherwise)

Bypass attempts to run explicitly:

- raw ICMP to blocked targets, not just TCP
- confirm the guest has no global IPv6 address at all (`ip -6 addr`), since that
  is what makes IPv4-only rules sufficient
- UDP to a blocked target, not just TCP
- the bridge on port 53 — expected reachable, since Incus inserts DHCP/DNS rules
  ahead of ACL rules. Confirm every *other* port on the bridge is blocked, and
  state clearly whether this is an acceptable scoped exception.

Also dump the generated host rules and check they match intent:

```bash
sudo nft list ruleset | grep -A30 -i incus
```

Look specifically for malformed rules mixing address families (the
`dns.nameservers` bug).

**CHECKPOINT 4.** Report a table: target, protocol, expected,
observed, and whether the host could reach it. Flag every mismatch.

## Phase 5 — Regression script

Write `test-network-acl.sh` alongside `test-invariants.sh` that re-runs the
phase 4 checks against a named instance and exits non-zero on any mismatch. It
must skip, not pass, any test whose target is unreachable from the host — a
silently skipped test that reports success is worse than no test.

## Deliverable

Append findings to `STATUS.md`: the final rule set, what was proven, what could
not be proven, and any residual gaps with their reasoning. IPv6 is already
documented as parked in `STATUS.md`; do not duplicate that discussion.
