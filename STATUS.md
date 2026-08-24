# STATUS

State, decisions and open work. Written 2026-08-23, updated 2026-08-24.
Start at `CLAUDE.md` for how to actually use this.

## Goal

An isolated VM with real GPU access for an unsupervised coding agent:

    monitored claude code on host  ->  Incus VM (GPU passed through)
                                        ->  unsupervised agent on a project

One VM at a time, one per project, stopped when unused. Remote paid models power
the agent; the GPU is for the software being built.

## Hardware

- AMD Ryzen 7 7800X3D, 60 GiB RAM, Ubuntu 26.04, kernel 7.0.0-29
- RTX 4080 SUPER at `0000:04:00.0` (+ audio `04:00.1`), `10de:2702` / `10de:22bb`
- IOMMU group 13: bridge + both NVIDIA functions. Clean, no ACS override needed.
- AMD Raphael iGPU at `0f:00.0` drives the console over HDMI
- Samsung 990 PRO 4TB: LUKS -> LVM -> ext4 root, VG fully allocated

**The GPU is in a chipset slot wired for x2.** `max=x2`, not a training failure:
3.19 GB/s H2D, 3.30 GB/s D2H is ~82% of PCIe 4.0 x2. On-card D2D 316 GB/s.

**Slot 1 (the only Gen5 x16) is unresolved** — snapped retention latch, and the
card previously failed to POST there. Diagnose with the monitor on the iGPU:
check for POST, try forcing Gen4/Gen3, inspect for bent pins or a cracked slot.
It matters because an agent profiling GPU code at x2 draws wrong conclusions
about transfer bottlenecks. The tools discover the PCI address, so moving the
card needs no config change.

## Decisions, and why

- **Incus, not microsandbox.** microsandbox optimises disposable microVM spawn;
  this is one long-lived VM. Incus has passthrough, ACLs and profiles natively.
- **VFIO passthrough, not virtio-gpu/venus.** Venus is Vulkan-only — no CUDA.
  Also a smaller host attack surface: no virglrenderer parsing guest commands.
- **NixOS guest.** One base image; per-project toolchains in project flakes.
- **Image is a build artifact** of `base/flake.nix`, not `incus publish` of a
  mutated instance.
- **No state store.** Incus is the source of truth; everything is derived. The
  only persistent artefact is a lock file.
- **No Terraform.** Nix + `incus admin init --preseed`, except preseed does not
  cover `network_acls` — hence `rig apply`.
- **Dynamic vfio binding.** Static binding needs boot-framebuffer workarounds
  when the dGPU is firmware-primary.
- **Go, not shell.** Typed, testable, and the Incus REST API is reachable
  directly, so nothing parses CLI output. One module, two binaries: `rig` for
  everything about VMs, `hostgpu` for the host itself — root, driver rebinds,
  and the desktop. `rig` was briefly split into a second binary to encode which
  verbs are dangerous; command groups in `--help` do that without the split.
- **Credentials are env vars in a host file**, injected to tmpfs at start.
  Scoping them is the operator's job; nothing else can judge it.

## Known Incus behaviours (verified here)

Behaviours the code now handles are documented at their handling site, not here.
What remains:

1. **The GPU is not released on VM stop.** Incus sets `driver_override=vfio-pci`
   and never clears it. `hostgpu desktop` does the rebind.
2. **Starting a second VM with the same GPU hot-unplugs it from the running
   one.** Loud on the VM that failed to start, **silent on the victim** — it
   still shows RUNNING with a healthy IP. This is what `rig` prevents.
3. **`dns.nameservers` + `security.acls` are incompatible** on 6.0.5: Incus emits
   the cross product of nameservers x address families and produces invalid
   nftables rules. `dns.nameservers` is unset, so this is not being triggered.
   Unnecessary anyway — Incus inserts DHCP/DNS rules ahead of ACL rules.
4. **Attaching any ACL flips the NIC to default-reject in both directions**, so a
   denylist ACL is a blackout until `egress.action=allow` is set. It reads as
   "the internet is a bit broken", because the bridge resolver keeps working.
   See `internal/policy`.
5. **An ACL's rules cannot be edited while it is attached** — Incus flushes an
   nftables chain it never created and fails. `rig apply` detaches, rewrites
   and reattaches, and refuses while a consumer is running. See
   `policy.rewriteACL`.

## Current state

- ZFS pool `fast` (loop-backed, 500 GiB, on the LUKS root, so encrypted at rest),
  ARC capped at 8 GiB. CoW clones measured: `rig new` takes 1.5 s and adds no
  pool usage.
- Host headless, console on iGPU/HDMI. `boot_vga=1` on `0f:00.0`.
- `./01-build-image.sh` builds and imports `nixos-gpu-base`.
- **Passthrough verified in-guest:** RTX 4080 SUPER, 16376 MiB, driver
  595.71.05, CUDA 13.2. `hardware.nvidia.open = true` works on Ada.
- **CUDA compute verified, not just `nvidia-smi`:** all 4,194,304 elements
  bit-exact under two launch geometries, with poison and negative-control checks
  passing. 3.11 GB/s H2D / 3.24 GB/s D2H in-guest, matching the host-side x2
  measurement — passthrough costs nothing measurable on top of a narrow link.
- **Isolation verified empirically:** `test-network-acl.sh` 23 passed, 0 failed,
  2 skipped. `test-invariants.sh` 11/11. `go test ./...` green.
- The whole path works end to end: `rig new` → `start` (GPU claimed, credentials
  injected) → `doctor` → CUDA test → ACL suite → `run-agent`.

### What the isolation is

`vm-isolate` egress-rejects `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
`169.254.0.0/16` and `100.64.0.0/10` (CGNAT/Tailscale — not covered by RFC1918
and a real gap once), with `egress.action=allow` and `ingress.action=reject` on
the NIC. Declared in `internal/policy`, reconciled by `rig apply`.

Before it was attached, the guest could reach the host's sshd on all five
addresses the host holds — including its tailnet address — plus the LAN gateway,
a LAN peer, an online tailnet peer and the router's DNS.

What is proven and what is not:

- **Attributable to the ACL:** every host address on tcp/22 (INPUT, not FORWARD,
  so nothing else is doing the blocking), the LAN gateway on tcp/80, ICMP to the
  gateway / a LAN peer / a tailnet peer, UDP/53 to the router.
- **Not attributable:** the docker *container* targets — Docker's own FORWARD
  rules drop cross-bridge traffic anyway. Kept as outcome checks, labelled.
- **Not proven:** `169.254.0.0/16`. Nothing on it is reachable from the host, so
  the test can only skip.
- **Not inspected:** the generated nftables ruleset — `sudo` needs a password.
- **Residual, not exploitable:** the WAN address is in no reject range, so a
  guest could hairpin to anything port-forwarded. The router does not hairpin, so
  it cannot be demonstrated from inside. No rule added: one keyed to a dynamic
  ISP address fails open silently, which is worse than a documented gap.
- The bridge resolver on **udp/53 is reachable by design** (Incus inserts
  DHCP/DNS rules ahead of ACL rules). Scoped, not a hole: tcp/22 on the same
  address is blocked.

## Open

1. **Slot 1 diagnostic** (see Hardware). Physical work.
2. See "What would make this harder over time" below.

## Parked — IPv6 (do not re-investigate without new information)

**The ISP does not delegate an IPv6 prefix.** Verified 2026-08-23 on the router:
the WAN v6 link never comes up, and `Received IPv6 prefix` is empty. So
`ipv6.address=none` stays on `incusbr0`. That is correct, not a workaround:
bridge IPv6 with no upstream route would add a bypass around IPv4-only ACL rules
and cause AAAA-first stalls.

This does not block testing that software handles IPv6 — a bridge ULA gives a
working v6 stack for socket code and happy-eyeballs. Only reachability to real
v6 hosts is unavailable.

If delegation ever appears: enable bridge IPv6 with **default-deny egress on
IPv6 only** plus a short allowlist, keeping IPv4 as a denylist. No ISP-prefix
tracking — a delegated prefix changes, and a denylist keyed to it fails open.
Verify first that Incus supports a per-address-family default egress action.

Also rejected: policy routing to give guests a table with only a host route.
Prefix-agnostic and guest-only, but it lives outside the Incus ACL model and
fails open if the rule is lost.

## Deferred

Snapshots/rollback, remote API access, multi-GPU, any scheduler. The constraint
is one card, one active project.

## Files

| Path | Purpose |
|---|---|
| `CLAUDE.md` | Which tool for what, and how to start a project |
| `RUNBOOK.md` | Host setup, in order |
| `cmd/rig` | The whole surface: lifecycle, guest access, card and policy |
| `cmd/hostgpu` | Move the card between the desktop and VMs |
| `internal/incus` | Typed REST client over the unix socket; exec over websockets |
| `internal/policy` | Declared isolation policy and the reconcile |
| `internal/gpu` | Arbitration, PCI discovery, the lock |
| `internal/creds` | Credential validation and injection |
| `base/` | Declarative image: flake + guest module |
| `01-build-image.sh` | Build and import the image |
| `test-invariants.sh` | GPU arbitration invariants against real Incus |
| `test-network-acl.sh` | Proves the ACL empirically; skips, never passes, unprovable tests |
| `project-template/` | Per-project devShell, the CUDA correctness test, `run-agent` |
| `secrets/` | Gitignored. Per-project credential files. |

## What would make this harder over time

Ranked by when it starts hurting.

1. **The base image has no version.** `nixos-gpu-base` is rebuilt in place, so
   two VMs created a month apart can differ with nothing recording it. Stamp the
   image with the flake lock revision and record it on each instance at `rig
   new`, so `rig doctor` can say "this VM predates the current image".
2. **Nothing garbage-collects.** Stopped project VMs accumulate at ~6 GiB each on
   a 500 GiB loop file, and a full root means write errors on a ZFS vdev. `rig
   status` should show age and size, and there should be a way to list what is
   stale.
3. **`/work` lives inside the instance.** `rig rm` destroys the project with the
   VM, so the VM is the only copy until someone pushes a git remote. Either put
   `/work` on a separate volume that outlives the instance, or have `rig rm`
   refuse when the tree has uncommitted changes.
4. **One card, one project is enforced but not scheduled.** With several
   projects, "who had it last, and does something want it now" becomes guesswork
   at the point where it is most annoying to add.
5. **The ACL is a denylist.** Every new private range someone invents is a gap
   until noticed. The IPv6 plan already says default-deny plus an allowlist; the
   same argument applies to IPv4 once there is any appetite for the churn.
6. **`test-network-acl.sh` skips silently-shaped tests.** It is honest about
   skips, but a target disappearing (the docker stack going away) quietly
   reduces coverage. It should fail when *coverage* drops below what it had.
