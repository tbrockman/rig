# STATUS

Context for picking this work up. Written 2026-08-23, updated 2026-08-24.

## Goal

An isolated VM with real GPU access, for an unsupervised coding agent to build
and test GPU software in. Architecture:

    monitored claude code on host
      -> Incus VM (GPU passed through, dev tooling)
        -> unsupervised agent working on a project

One VM active at a time, roughly one per project, stopped when unused. Remote
paid models power the agent — the GPU is for the software being built, not for
running models locally.

## Hardware

- AMD Ryzen 7 7800X3D, 60 GiB RAM, Ubuntu 26.04, kernel 7.0.0-29
- RTX 4080 SUPER at `0000:04:00.0` (+ audio `04:00.1`), IDs `10de:2702` / `10de:22bb`
- IOMMU group 13: bridge + both NVIDIA functions. Clean. No ACS override needed.
- AMD Raphael iGPU at `0f:00.0` — drives the console over HDMI
- Samsung 990 PRO 4TB: LUKS -> LVM -> ext4 root, VG fully allocated

**The GPU is in a chipset slot wired for x2.** `max=x2`, not a training failure.
Measured 3.19 GB/s H2D, 3.30 GB/s D2H (~82% of PCIe 4.0 x2, so the link is clean,
just narrow). On-card D2D 316 GB/s, normal.

Slot 1 (the only Gen5 x16) has a snapped retention latch and previously failed to
POST with the card installed. **Unresolved.** Worth diagnosing: monitor on iGPU,
card in slot 1, check for POST; try forcing Gen4/Gen3; inspect for bent pins,
debris, cracked slot body, lifted solder. Matters because the agent will profile
GPU code, and at x2 it would draw wrong conclusions about transfer bottlenecks.
If the card moves, its PCI address changes — update `GPUCTL_PCI` and re-claim.

## Decisions made, and why

- **Incus, not microsandbox.** microsandbox optimises fast disposable microVM
  spawn; this workload is one long-lived VM. Incus has physical GPU passthrough,
  network ACLs, and profiles as first-class features.
- **VFIO passthrough, not virtio-gpu/venus.** Venus is Vulkan-only — no CUDA, no
  NVENC, no OpenGL. Passthrough also has a smaller host attack surface: no
  virglrenderer parsing untrusted guest commands in host userspace.
- **NixOS guest.** One base image; per-project toolchains live in project flakes.
  Avoids per-project image proliferation and version drift.
- **Image is a build artifact.** `nix build` of `system.build.qemuImage` +
  `system.build.metadata`, imported into Incus. Not `incus publish` of a mutated
  instance.
- **No state store.** Incus is the sole source of truth; `gpuctl` derives
  everything from `GET /1.0/instances`. Only persistent artefact is a lock file.
- **No Terraform/OpenTofu.** Nix + `incus admin init --preseed`. Note preseed does
  not cover `network_acls` — those need a separate reconcile pass.
- **Dynamic vfio binding, not static.** Proven working: five acquisitions
  including one that took the card back from the live `nvidia` driver. Static
  binding was considered and rejected because it needs boot-framebuffer
  workarounds when the dGPU is firmware-primary.

## Known Incus behaviours (verified on this host)

1. **GPU is not released on VM stop.** Incus sets `driver_override=vfio-pci` and
   never clears it. Manual rebind procedure is in `hostgpu`, verified working.
2. **Starting a second VM with the same GPU hot-unplugs it from the running one.**
   VFIO sends a device request to the current owner; QEMU's handler unplugs.
   Both VMs lose. Loud on the VM that failed to start, **silent on the one that
   was working** — `incus list` still shows it RUNNING with a healthy IP.
   This is what `gpuctl` exists to prevent.
3. **`dns.nameservers` + `security.acls` are incompatible** on 6.0.5. Incus emits
   the cross product of nameservers x address families, producing invalid nftables
   rules. Unnecessary anyway: Incus inserts DHCP/DNS rules ahead of ACL rules, so
   blocking RFC1918 does not break the bridge resolver. Worth reporting upstream.

## Current state — what works

- ZFS pool `fast` (loop-backed, 500 GiB, on the LUKS root so encrypted at rest).
  ARC capped at 8 GiB via `/etc/modprobe.d/zfs.conf`.
- Host headless (`multi-user.target`), console on iGPU/HDMI, `getty@tty1` active.
  `boot_vga=1` on `0f:00.0`, `fb0` on the iGPU. `amdgpu` added to initramfs.
- `./01-build-image.sh` builds and imports `nixos-gpu-base` successfully.
- **Passthrough verified from inside the guest:** RTX 4080 SUPER, 16376 MiB,
  driver 595.71.05, CUDA 13.2. `hardware.nvidia.open = true` works on Ada.
- `incus exec` works against the built image (agent is fine).
- `gpuctl claim/start/stop/release` all working; `./test-invariants.sh` 7/7.
- **CUDA compute verified end to end, not just `nvidia-smi`.** `project-template`
  builds in a guest devShell and `vectoradd` reports all 4,194,304 elements
  bit-exact under two launch geometries, with the poison and negative-control
  checks passing. Measured 3.11 GB/s H2D, 3.24 GB/s D2H from inside the guest —
  consistent with the host-side x2 measurement, so passthrough costs nothing
  measurable on top of the narrow link.

## Immediate next steps

1. **Network ACL (IPv4 only).** `vm-isolate` rejecting RFC1918 +
   `169.254.0.0/16` **and `100.64.0.0/10`** — the Tailscale range was a real gap
   found earlier and is not covered by RFC1918. Keep bridge IPv6 disabled
   (`ipv6.address=none`); see "Parked" below.
2. **ACL reconcile pass** in whatever `apply` verb gets written, since preseed
   does not cover ACLs.
3. **Agent-facing wrapper**: expose only `status`, `start`, `stop`, `claim` — not
   the 204-operation Incus MCP server.

Note for the ACL work: `cuda-smoke` is left in place, stopped, holding the GPU
device. It is a ready-made subject — it has the CUDA toolchain in its store
already, so `nix develop` there is offline-fast and will keep working once egress
is restricted. Re-running `vectoradd` after applying the ACL is a cheap check
that the rules did not break anything the guest actually needs.

## Parked — IPv6 (do not re-investigate without new information)

**The ISP does not delegate an IPv6 prefix.** Verified 2026-08-23 on the
ISP-supplied router:

- WAN: `WAN link status(v6): Linking` — never reaches Up. No v6 WAN address or
  gateway. IPv4 only (`64.46.8.186`).
- LAN: `Prefix Config` offers "Use WAN provided prefix", but
  `Received IPv6 prefix: -`, and the setting reverts to `Static` on save —
  the router cannot hold the delegated mode because no prefix arrives.

Consequence: `ipv6.address=none` stays set on `incusbr0`. This is the correct
setting, not a workaround — bridge IPv6 with no upstream route would add a
bypass path around IPv4-only ACL rules and cause AAAA-first stalls (DNS returns
AAAA records for e.g. archive.ubuntu.com, which a dual-stack guest would try
first and time out on).

Note this does **not** block testing that software handles IPv6 correctly: a
bridge ULA gives a working v6 stack for socket code, dual-stack listeners, and
happy-eyeballs behaviour. Only reachability to real IPv6 internet hosts is
unavailable.

If the ISP ever enables delegation, the decision is already made: enable bridge
IPv6 with **default-deny egress on IPv6 only** plus a short allowlist, keeping
IPv4 as a denylist. No ISP-prefix tracking — a delegated prefix changes, and a
denylist keyed to it fails open. Verify first that Incus supports a per-address-
family default egress action; if not, the hybrid is not expressible as stated.

Also considered and rejected: policy routing to give guests a routing table with
only a host route to the gateway and a default via it, so LAN destinations have
no route. Prefix-agnostic and guest-only (matches `iif incusbr0`, so host
traffic is unaffected), but it lives outside the Incus ACL model and fails open
if the rule is lost. Not worth it while there is no IPv6 at all.

## Deferred

Snapshots/rollback (Incus does it, wrap later), remote API access, multi-GPU
generalisation, any scheduler. The constraint is one card, one active project.

## Files

| File | Purpose |
|---|---|
| `RUNBOOK.md` | Ordered setup procedure |
| `gpuctl` | GPU arbitration. Stdlib Python over the Incus REST socket. |
| `hostgpu` | Move the card between host desktop and VM use |
| `base/flake.nix` | Declarative image definition |
| `guest/gpu-dev.nix` | Guest module: driver, agent, `gpu-present` unit, `gpu-check` |
| `01-build-image.sh` | Build + import the image |
| `test-invariants.sh` | Invariant tests against real Incus |
| `project-template/flake.nix` | Per-project devShell template (CUDA toolchain) |
| `project-template/vectoradd.cu` | GPU correctness test: poisoned buffers, bit-exact, negative control |
| `project-template/README.md` | How to use the template, and why the toolchain is not in the image |
| `00-spike.sh` | Phase 0 verification. Served its purpose; kept for reference. |

## Gotchas

- **Fixed sleeps are wrong.** The agent takes variable time to come up; a hung
  `incus exec` ignores SIGTERM. Poll with `timeout -s KILL`, and pass
  `</dev/null` so exec does not allocate a TTY.
- **`writeShellScriptBin` provides no PATH.** Reference binaries by store path.
  This bug produced a false "no NVIDIA device" report.
- **Guest PCI address differs from host** (guest sees `06:00.0`, host `04:00.0`).
  Expected — the guest has its own PCI topology.
- **`libcuda.so.1` comes from the driver, not the toolkit.** Nothing in
  `cudaPackages` provides it. On NixOS it is in `/run/opengl-driver/lib`, which
  the template's `shellHook` adds to `LD_LIBRARY_PATH`. Without it you get a
  binary that compiles and links cleanly and then fails at runtime with
  `cudaErrorInsufficientDriver` — which reads like a driver problem and is not.
- **Toolkit and driver versions do not have to match.** Verified: a CUDA 12.9
  toolkit against the guest's 13.2 driver. Drivers are backward compatible with
  older runtimes; that is what lets the toolchain live in the project.
- **Give project instances more than the default 10 GiB root.** The base system
  plus a CUDA toolchain is 6.2 GiB of `/nix/store`. Use `-d root,size=40GiB`.
- **DHCP is not up when the agent is.** `incus exec` succeeded several seconds
  before the guest had an IPv4 address, so the first network call in a freshly
  started VM can fail with a DNS error that means nothing. Poll for the address,
  not for the agent, before assuming the network is broken.
- **Never put a GPU device in a profile.** Every instance would then be configured
  to grab the same card. `gpuctl` refuses to operate if it finds one.
- **Watch root filesystem usage.** The ZFS pool file is under
  `/var/lib/incus/disks/`. A full root means write errors on a ZFS vdev.