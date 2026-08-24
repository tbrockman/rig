# Runbook

Run in order. Stop at any step that fails rather than working around it.

---

## Step 0 — Spike (do this before anything else)

```bash
chmod +x 00-spike.sh && ./00-spike.sh
```

The one result that matters: **does `incus exec spike -- true` succeed?** If
not, the agent isn't up in the NixOS VM, and `gpuctl`'s transport plus the test
script's `incus exec` calls need swapping for SSH.

Override the release with `NIXOS_RELEASE=unstable ./00-spike.sh` if needed.

Clean up: `incus delete -f spike`

---

## Step 0.5 — Convert the storage pool off `dir`

The `default` pool is `dir`, so every `incus copy` is a full byte-for-byte copy.
The per-project model depends on cheap CoW clones. Pools cannot be converted in
place — create a new one and repoint the default profile.

### Why loop-backed, on this machine

Investigated and ruled out:

- **Dedicated partition** — `nvme0n1p3` spans the disk; no unallocated space.
- **LVM logical volume** — `ubuntu-vg` has `VFree 0`; the single LV owns it all.
- **Second disk** — `sda` is the MSI driver-utility USB device (22 K), not storage.
- **Shrinking root** — 2.5 TB free is inside a mounted ext4 root on LUKS+LVM.
  Reclaiming it means an offline resize from a live USB, where the failure mode
  is an unbootable host. Not worth it for a storage backend.

Loop-backed it is. It stacks ZFS on a file on ext4 on LVM on LUKS, which is
inelegant, but it eliminates the full copy per clone — the thing actually
blocking the workflow. The overhead is a tax, not a wall, and the pool can be
migrated to a real vdev later without rebuilding anything above it.

Upside: the pool file lives on the LUKS-encrypted root, so VM disks get
encryption at rest for free.

### Create the pool

```bash
sudo apt install -y zfsutils-linux
sudo modprobe zfs && echo "zfs module ok"

incus storage create fast zfs size=500GiB
incus profile device set default root pool=fast
incus storage list
```

### Cap the ARC

ZFS defaults its cache to about half of system RAM (~30 GB of your 60 GB), which
competes with VM memory. Bound it before running guests:

```bash
echo "options zfs zfs_arc_max=8589934592" | sudo tee /etc/modprobe.d/zfs.conf
echo 8589934592 | sudo tee /sys/module/zfs/parameters/zfs_arc_max   # this boot
```

The reboot in step 1 makes the modprobe setting permanent.

### Clean up and verify

```bash
incus delete -f gputest 2>/dev/null   # throwaway from the earlier report
incus delete -f spike   2>/dev/null   # from the spike
```

After the image exists (step 2), confirm clones are actually cheap — near-instant
and almost no space:

```bash
incus copy nixos-gpu-base clonetest --vm
incus storage info fast
incus delete -f clonetest
```

If it takes a minute and eats 60 GB, the profile change didn't take.

### Watch root usage

The pool file is under `/var/lib/incus/disks/` on your root filesystem. A full
root means write errors on a ZFS vdev, which is a much worse failure than a
plain "disk full". Keep headroom.

## Step 1 — Host: go headless

Only one thing here is required: stop a display manager from holding the GPU.
Everything else is a choice.

```bash
sudo systemctl set-default multi-user.target
```

With no display manager, nothing contends for the card, and Incus can unbind
`nvidia` and bind `vfio-pci` dynamically at VM start. This is the path already
proven on this host — five successful acquisitions, including one that took the
card back from the *live* `nvidia` driver.

Reboot to pick this up (and the ZFS ARC cap from step 0.5).

After rebooting, confirm nothing holds the GPU:

```bash
lspci -nnk -s 04:00.0        # driver in use: nvidia, but idle
sudo fuser -v /dev/nvidia*   # expect no holders
```

### Optional: static vfio-pci binding

Makes the binding deterministic rather than incidental, since the card never
returns to the host on its own anyway.

**Do not do this while the monitor is on the 4080 and it is firmware's primary
display device.** The kernel boot framebuffer (`efifb`/`simpledrm`) claims the
card's memory regions early and `vfio-pci` can fail to reserve its BARs. Move
the monitor to the iGPU's HDMI output and set the iGPU primary in firmware
first, or add `video=efifb:off` to the kernel command line — but note that
debugging a bad kernel parameter on a headless box over SSH is exactly the
situation you cannot recover from.

```bash
sudo tee /etc/modprobe.d/vfio.conf <<'EOF'
options vfio-pci ids=10de:2702,10de:22bb
softdep nvidia pre: vfio-pci
EOF
sudo update-initramfs -u
```

The dynamic path works. Take this only if determinism is worth the setup.

### Console access, and the one window where you lose it

With the monitor on the 4080's DisplayPort, you have a text console whenever no
VM is running. Once a VM claims the card the outputs go dark, and because the
card stays pinned to `vfio-pci` after the VM stops (finding #6), the console
stays dark until you run the rebind procedure.

If SSH breaks during that window, you are locked out. Moving the monitor to the
iGPU's HDMI output removes the risk entirely — worth doing if you open the case
for the slot-1 diagnostic anyway, but not a prerequisite.

## Step 2 — Build the base image (declaratively)

The image is a build artifact of `base/flake.nix` + `guest/gpu-dev.nix`. No
logging into a VM, no `incus publish` of mutated state.

```bash
chmod +x 01-build-image.sh
./01-build-image.sh nixos-gpu-base
```

Uses `nixos/modules/virtualisation/incus-virtual-machine.nix` from nixpkgs,
which supplies the bootloader, filesystem layout, the Incus guest agent, and
the `qemuImage` / `metadata` build outputs.

The first qcow2 build is slow. Subsequent ones are cached.

Smoke-test it:

```bash
export GPUCTL_PCI=0000:04:00.0
chmod +x gpuctl
incus init nixos-gpu-base smoke --vm -c security.secureboot=false \
  -c limits.cpu=8 -c limits.memory=16GiB
./gpuctl start smoke
sleep 30
incus exec smoke -- gpu-check     # expect card name, 16376 MiB, driver version
```

If `gpu-check` fails, the driver didn't build:
`incus exec smoke -- journalctl -b -u gpu-present`.
If `incus exec` itself fails, it's the agent — see the module note above.

```bash
./gpuctl stop smoke && ./gpuctl release && incus delete -f smoke
```

**Updating the image later** is a flake edit plus `./01-build-image.sh`. Existing
project instances keep their current image; recreate them from the new alias
when you want the update.

### Fallback

If the declarative import fights you, you can launch `images:nixos/25.05` as a
VM, push `gpu-dev.nix` into `/etc/nixos/`, `nixos-rebuild switch`, and
`incus publish`. It works, but the resulting image is a snapshot of mutated
state rather than a reproducible artifact — treat it as a stopgap while you
sort out the build, not the steady state.

---

## Step 3 — Test the invariants

```bash
chmod +x test-invariants.sh
export GPUCTL_PCI=0000:04:00.0
./test-invariants.sh nixos-gpu-base
```

Test 3 is the one that matters — it asserts the *victim* is still healthy after
a refused claim, not merely that the claim was refused.

---

## Step 4 — Create a project

```bash
incus copy nixos-gpu-base proj-foo --vm -d root,size=40GiB   # CoW clone
incus config set proj-foo user.project=foo

./gpuctl start proj-foo
incus file push -r project-template/. proj-foo/work/foo/
incus exec proj-foo -- bash -lc 'cd /work/foo && nix develop "path:."'
```

The base image carries the driver; the toolchain is per-project, in
`project-template/flake.nix`. Copy that directory into each project repo and pin
its CUDA version there. See `project-template/README.md`.

Size the root disk explicitly. The default volume is 10 GiB and the base system
plus a CUDA toolchain is 6.2 GiB — it fits, but not with room to work in.

### Step 4.5 — Prove the GPU actually computes

`gpu-check` proves the card is *attached*. It does not prove a kernel produces
correct results. Run the real test once per new project VM:

```bash
incus exec proj-foo -- bash -lc 'cd /work/foo && nix develop "path:." -c make run'
```

Expect `PASS: 4194304 elements bit-exact under 2 launch geometries`. Exit 2 with
"No CUDA device" means the GPU is not attached or `libcuda.so.1` is not on the
library path — see the template's README, which explains why those two failures
look identical and how to tell them apart.

The test also prints H2D/D2H bandwidth. On this host expect ~3.1–3.3 GB/s: that
is the chipset **x2** slot, not a fault. Anything much lower is worth chasing.

---

## Step 5 — Isolate the guest network

```bash
./gpuctl apply --dry-run     # what would change
./gpuctl apply               # reconcile the ACL and the profile NIC
```

`apply` declares the policy (the five reject ranges, the three NIC keys) and
reconciles Incus to it, because `incus admin init --preseed` does not cover
`network_acls`. It is idempotent, and it reports any instance whose own NIC
override leaves it uncovered rather than overwriting it.

The equivalent by hand — three settings on the NIC, since the ACL alone is
**not** the policy:

```bash
incus profile device set default eth0 \
  security.acls=vm-isolate \
  security.acls.default.egress.action=allow \
  security.acls.default.ingress.action=reject
```

Use `incus config device override <instance> eth0 ...` with the same keys if one
instance needs to differ. `gpuctl start` refuses an instance whose NIC has no
ACL, so a missed one surfaces at start rather than silently.

`egress.action=allow` is the one that looks wrong and is not. Attaching any ACL
flips the NIC to **default-reject in both directions**, which turns a denylist
into a blackout — and it presents as "the internet is a bit broken" rather than
"nothing works", because Incus's DHCP/DNS pre-rules keep the bridge resolver
alive so names still resolve. `allow` restores denylist semantics; the five
reject rules in `vm-isolate` then do the actual work.

Attaching the ACL needs the instance stopped. Changing the default actions
afterwards applies live.

Then prove it, rather than trusting the config:

```bash
./test-network-acl.sh proj-foo
```

Expect `0 failed`. Skips are fine and are not passes — they mean the target was
unreachable from the host too, so blocking it proves nothing.

If everything is blocked including the public internet, the egress default
action is the first thing to check.

---

## Daily use

```bash
./gpu status                 # who has the card
./gpu stop proj-foo
./gpu start proj-bar         # claims + starts; refuses if foo were running
```

`gpu` is the four-verb surface and the one to reach for. `gpuctl` underneath it
adds `release`, `apply`, `--force`, and `--allow-unisolated` — the operations
that can break an invariant, which is exactly why they are not in `gpu`.

---

## Still open

- **Slot 1 diagnostic.** Put the GPU in slot 1, monitor on the iGPU, set primary
  display to IGD in firmware. If you get POST output, the board is fine and the
  problem is card-in-slot-1 specific — then try forcing Gen4/Gen3 for that slot.
  Do this before several projects have `0000:04:00.0` baked in; if the card
  moves, update `GPUCTL_PCI` and re-`claim` each instance.
- **Changing the ACL rules later.** Do not edit `vm-isolate` in place: Incus
  refuses any rule change on an ACL with `USED BY` > 0, with an `nft` error about
  a missing `acl.incusbr0` chain. Create `vm-isolate-v2`, point the NIC at it,
  delete the old one. See known behaviour #6 in `STATUS.md`.
- **Agent-facing wrapper.** Expose only `status`, `start`, `stop`, `claim`.