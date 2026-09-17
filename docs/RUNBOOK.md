# Runbook

Host setup, in order, done once. To start a project afterwards, read
`CLAUDE.md`; for what this is and what it does and does not contain, `README.md`.

Steps 1 and 2 reshape the host and need `sudo`. Stop at any step that fails
rather than working around it: every later step assumes the earlier ones hold.

---

## Step 0 — Prerequisites

Check before touching anything:

```bash
incus version                                 # 6.0 or later, client and server
id -nG | grep -w incus-admin                  # your user can open the socket
incus network list                            # a managed bridge (incusbr0 by default)
nix --version && nix flake --help >/dev/null  # nix, with flakes enabled
go version                                    # 1.26
lspci -nn | grep -i 'nvidia'                  # the card, and its audio function
sudo dmesg | grep -iE 'iommu|dmar|amd-vi' | head   # the IOMMU is on
```

The card's IOMMU group must hold nothing the host needs, or `vfio-pci` cannot
take it. With the card's address from `lspci` (`0000:01:00.0` below):

```bash
g=$(basename "$(readlink /sys/bus/pci/devices/0000:01:00.0/iommu_group)")
ls /sys/kernel/iommu_groups/$g/devices/
```

The GPU, its audio function, and at most the PCIe bridge above them is a clean
group. A NIC or a storage controller in the same group means the card has to
move to another slot; ACS override patches exist and are not covered here.

If the IOMMU is off, enable it in firmware (VT-d, or AMD-Vi/SVM) and, on Intel,
add `intel_iommu=on iommu=pt` to the kernel command line. AMD enables it by
default once the firmware does.

## Step 1 — Storage pool

`rig new` clones the base image. On a `dir` pool that is a byte-for-byte copy
of several gigabytes per VM; on ZFS or btrfs it is a CoW clone that takes a
second and no space. Pools cannot be converted in place, so decide before
creating anything.

With an unused block device or partition, make a real pool:

```bash
incus storage create fast zfs source=/dev/nvme0n1p4
```

Without one, a loop-backed pool works. The reference host runs on one — a
500 GiB file on the root filesystem, because the disk was fully allocated and
shrinking a mounted root is an offline job whose failure mode is an unbootable
machine. Stacking ZFS on a file is inelegant but eliminates the copy per clone,
the pool can move to a real vdev later, and a file on an encrypted root gets
encryption at rest for free:

```bash
sudo apt install -y zfsutils-linux && sudo modprobe zfs
incus storage create fast zfs size=500GiB
```

Either way, make it the default for new instances:

```bash
incus profile device set default root pool=fast
```

Cap the ARC before running guests — ZFS otherwise takes about half of RAM:

```bash
echo "options zfs zfs_arc_max=8589934592" | sudo tee /etc/modprobe.d/zfs.conf
echo 8589934592 | sudo tee /sys/module/zfs/parameters/zfs_arc_max   # this boot
```

Once the image exists (step 3), confirm clones are actually cheap:

```bash
./rig new clonetest && incus storage info fast && ./rig rm clonetest
```

Measured on the reference host: 1.5 s, pool usage unchanged. If it takes a
minute and eats gigabytes, the profile change did not take.

**Watch root usage on a loop-backed pool.** The file is under
`/var/lib/incus/disks/`. A full root means write errors on a ZFS vdev, which is
far worse than a plain "disk full".

## Step 2 — Free the card

Nothing on the host may hold the GPU when a VM claims it. On a headless host
that is already true. On a host with a desktop, one thing is required: stop the
display manager from starting at boot.

```bash
sudo systemctl set-default multi-user.target
```

Then nothing contends for the card and Incus binds `vfio-pci` dynamically at VM
start — proven across many acquisitions on the reference host, including one
that took the card back from the live `nvidia` driver. Reboot to pick this up
along with the ARC cap, then confirm nothing holds it: `sudo fuser -v
/dev/nvidia*` should print nothing.

Afterwards `rig host desktop` and `rig host headless` move the card between the
desktop and VMs without a reboot, and `rig new --no-gpu` makes a VM that never
takes it.

**Console access.** If your only monitor is on the card, you have a console only
while no VM is running, and the card stays bound to `vfio-pci` after a VM stops
(behaviour 1 in `DESIGN.md`), so the screen stays dark until `rig host desktop`.
If SSH breaks in that window you are locked out. Put the console on a second
GPU — the reference host uses the iGPU's HDMI output — or be certain of SSH.

**Optional: static vfio-pci binding.** Makes the binding deterministic. Do *not*
do it while the monitor is on the card and it is firmware's primary display: the
boot framebuffer claims the card's memory regions early and `vfio-pci` can fail
to reserve its BARs. The dynamic path works; take this only if determinism is
worth the setup. The ids are the card's and its audio function's, from
`lspci -nn`:

```bash
sudo tee /etc/modprobe.d/vfio.conf <<'EOT'
options vfio-pci ids=10de:2702,10de:22bb
softdep nvidia pre: vfio-pci
EOT
sudo update-initramfs -u
```

## Step 3 — Build the base image

The image is a build artifact of `base/`. No logging into a VM, no
`incus publish` of mutated state.

```bash
make                            # ./rig
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

## Step 4 — Isolate the guest network

Before creating any project — isolation is inherited from the profile, so
getting it right once covers every VM after it.

```bash
./rig apply --dry-run
./rig apply
```

## Step 5 — Test

```bash
make test              # go vet + unit tests, no daemon needed
make test-integration  # against real Incus and the real card
```

The integration suite is destructive and refuses to run while anything else is
up. Two of its cases carry the weight: one asserts the *victim* is still healthy
after a refused claim rather than merely that the claim was refused, and one
starts a guest with the ACL stripped and requires `rig verify` to call that a
violation — without which a verify that always said "blocked" would pass
everything else. A third, `TestGuestVerbs`, drives the agent, push and forward
verbs through the built binary against a no-GPU guest with a stand-in agent, so
it runs on a host whose card is busy:

```bash
go test -tags integration ./integration/ -run TestGuestVerbs -v
```

Then prove isolation on a real project VM rather than trusting the config:

```bash
./rig verify <instance>
```

Exit 0 proven, 1 violated, 2 could not be proven. Exit 2 is not a pass: those
checks could not distinguish a blocked guest from a broken probe. If everything
is blocked including the public internet, check the egress default action.

## Step 6 — Create a project

See `CLAUDE.md`.
