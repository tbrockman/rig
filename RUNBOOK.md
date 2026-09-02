# Runbook

Host setup, in order. Already done on this machine; this is for a rebuild.
To start a project instead, read `CLAUDE.md`.

Steps 0–1 reshape the host and need `sudo`. Stop at any step that fails rather
than working around it.

---

## Step 0 — Storage pool

The `default` pool is `dir`, so every clone is a full byte-for-byte copy. The
per-project model depends on cheap CoW clones, and pools cannot be converted in
place.

Loop-backed ZFS, because the alternatives are all ruled out on this machine:
`nvme0n1p3` spans the disk with no unallocated space, `ubuntu-vg` has `VFree 0`,
`sda` is the MSI driver-utility USB device, and shrinking a mounted ext4 root on
LUKS+LVM means an offline resize from a live USB where the failure mode is an
unbootable host. Stacking ZFS on a file is inelegant but eliminates the full copy
per clone, and the pool can move to a real vdev later. It also lands on the
LUKS-encrypted root, so VM disks get encryption at rest for free.

```bash
sudo apt install -y zfsutils-linux && sudo modprobe zfs
incus storage create fast zfs size=500GiB
incus profile device set default root pool=fast
```

Cap the ARC before running guests — ZFS otherwise takes about half of RAM:

```bash
echo "options zfs zfs_arc_max=8589934592" | sudo tee /etc/modprobe.d/zfs.conf
echo 8589934592 | sudo tee /sys/module/zfs/parameters/zfs_arc_max   # this boot
```

Once the image exists (step 2), confirm clones are actually cheap:

```bash
./rig new clonetest && incus storage info fast && ./rig rm clonetest
```

Measured 2026-08-24: 1.5 s, pool usage unchanged. If it takes a minute and eats
60 GB, the profile change did not take.

**Watch root usage.** The pool file is under `/var/lib/incus/disks/`. A full root
means write errors on a ZFS vdev, which is far worse than a plain "disk full".

## Step 1 — Go headless

One thing is required: stop a display manager from holding the GPU.

```bash
sudo systemctl set-default multi-user.target
```

Then nothing contends for the card and Incus can bind `vfio-pci` dynamically at
VM start — proven here across many acquisitions, including one that took the card
back from the live `nvidia` driver. Reboot to pick this up along with the ARC cap,
then confirm nothing holds it: `sudo fuser -v /dev/nvidia*` should be empty.

**Console access.** With the monitor on the 4080's DisplayPort you have a console
only while no VM is running, and the card stays pinned to `vfio-pci` after a VM
stops (behaviour #1 in `STATUS.md`), so it stays dark until `rig host desktop`. If
SSH breaks in that window you are locked out. The monitor is on the iGPU's HDMI
output here, which removes the risk.

**Optional: static vfio-pci binding.** Makes the binding deterministic. Do *not*
do it while the monitor is on the 4080 and it is firmware's primary display: the
boot framebuffer claims the card's memory regions early and `vfio-pci` can fail
to reserve its BARs. The dynamic path works; take this only if determinism is
worth the setup.

```bash
sudo tee /etc/modprobe.d/vfio.conf <<'EOF'
options vfio-pci ids=10de:2702,10de:22bb
softdep nvidia pre: vfio-pci
EOF
sudo update-initramfs -u
```

## Step 2 — Build the base image

The image is a build artifact of `base/`. No logging into a VM, no
`incus publish` of mutated state.

```bash
make                            # the two binaries
./rig image build
```

It uses `nixos/modules/virtualisation/incus-virtual-machine.nix` from nixpkgs for
the bootloader, filesystem layout, guest agent and build outputs. The first
qcow2 build is slow; later ones are cached.

Smoke-test:

```bash
./rig new smoke --start
./rig doctor smoke
./rig stop smoke && ./rig rm smoke
```

If the GPU check fails the driver did not build:
`./rig exec smoke journalctl -b -u gpu-present`.

Updating the image later is a flake edit plus `./rig image build`. It stamps each
image with the store path it came from, so a rebuild that changes nothing is a
no-op, and `rig doctor` tells you when a VM predates the current image. Existing
instances keep the image they were made from; recreate them to move.

## Step 3 — Isolate the guest network

Before creating any project — isolation is inherited from the `default` profile,
so getting it right once covers every VM after it.

```bash
./rig apply --dry-run
./rig apply
```

## Step 4 — Test

```bash
make test              # go vet + unit tests, no daemon needed
make test-integration  # against real Incus and the real card
```

The integration suite is destructive and refuses to run while anything else is
up. Two of its cases carry the weight: one asserts the *victim* is still healthy
after a refused claim rather than merely that the claim was refused, and one
starts a guest with the ACL stripped and requires `rig verify` to call that a
violation — without which a verify that always said "blocked" would pass
everything else.

Then prove isolation on a real project VM rather than trusting the config:

```bash
./rig verify <instance>
```

Exit 0 proven, 1 violated, 2 could not be proven. Exit 2 is not a pass: those
checks could not distinguish a blocked guest from a broken probe. If everything
is blocked including the public internet, check the egress default action.

## Step 5 — Create a project

See `CLAUDE.md`.

---

## Still open

- **Slot 1 diagnostic.** Monitor on the iGPU, card in slot 1, primary display set
  to IGD in firmware. POST output means the board is fine and the problem is
  card-in-slot-1 specific; then try forcing Gen4/Gen3.
