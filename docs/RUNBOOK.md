# Runbook

Host setup, done once, in order. Stop at any step that fails rather than
working around it: each later step assumes the earlier ones hold.

## 1. Prerequisites

```bash
incus version                                 # 6.0 or later, client and server
id -nG | grep -w incus-admin                  # your user can open the socket
incus network list                            # a managed bridge (incusbr0 by default)
nix --version && nix flake --help >/dev/null  # nix, with flakes enabled
go version                                    # 1.26
```

## 2. Storage pool

`rig new` clones the guest image. On a `dir` pool that is a full copy of
several gigabytes per VM; on ZFS or btrfs it is a copy-on-write clone that takes
a second. Pools cannot be converted in place, so decide first.

```bash
incus storage create fast zfs source=/dev/nvme0n1p4   # a spare partition, or:
incus storage create fast zfs size=500GiB             # loop-backed, on the root fs
incus profile device set default root pool=fast
```

`rig setup` (next step) copies `default`'s root disk and NIC into the `rig`
profile, so set the pool on `default` first, or on `rig` afterwards.

With ZFS, cap the ARC, which otherwise takes about half of RAM:

```bash
echo "options zfs zfs_arc_max=8589934592" | sudo tee /etc/modprobe.d/zfs.conf
echo 8589934592 | sudo tee /sys/module/zfs/parameters/zfs_arc_max   # this boot
```

A loop-backed pool lives under `/var/lib/incus/disks/`; a full root filesystem
then means write errors on a ZFS vdev, so watch it.

## 3. Isolation

```bash
make                    # ./rig
./rig setup --dry-run
./rig setup
```

This creates the `vm-isolate` ACL and the `rig` profile (the NIC and root disk
of `default`, with the ACL on the NIC). rig's VMs use that profile; nothing
else on the host is touched.

## 4. The guest image

```bash
./rig image build                                          # rig-base
./rig image build --attr guest-nvidia --alias rig-nvidia   # only for GPU VMs
```

The first build is slow; later ones are cached, and a rebuild that changes
nothing is a no-op. Existing VMs keep the image they were made from; `rig
doctor` says when one predates the current image.

Smoke test, and check clones are cheap (a second, and no pool growth):

```bash
./rig new smoke --start && ./rig doctor smoke && ./rig verify smoke
./rig stop smoke && ./rig rm smoke
```

`rig verify` exits 0 proven, 1 violated, 2 could not be proven. Exit 2 is not a
pass.

## 5. Passthrough (optional)

Only for VMs that will be given a PCI device: a GPU or a USB controller. `usb`
devices by vendor:product need none of this.

**The IOMMU** must be on, in firmware (VT-d, or AMD-Vi/SVM) and on Intel also
`intel_iommu=on iommu=pt` on the kernel command line:

```bash
sudo dmesg | grep -iE 'iommu|dmar|amd-vi' | head
```

**Each device must sit alone in its IOMMU group**, apart from its own functions
and the bridge above it. `rig.yaml` records its address and `lspci -nn` id:

```bash
lspci -Dnn | grep -iE 'vga|usb'
g=$(basename "$(readlink /sys/bus/pci/devices/0000:2b:00.0/iommu_group)")
ls /sys/kernel/iommu_groups/$g/devices/
```

For a USB controller, find which one a port belongs to by plugging something
in and reading `lsusb -t`; sysfs does not say which rear port is which. Avoid a
controller that is a function of the same device as the GPU driving the host's
console: on AMD this has hung the host hard (docs/DESIGN.md).

**Free the card.** Nothing on the host may hold a GPU when a VM claims it. On a
host whose desktop uses it, stop the display manager starting at boot, reboot,
and check nothing holds it:

```bash
sudo systemctl set-default multi-user.target
sudo fuser -v /dev/nvidia*          # should print nothing
```

Afterwards `rig host desktop` and `rig host headless` move the card between
the desktop and VMs without a reboot. If your only monitor is on the card, you
have a console only while no VM holds it; put the console on a second GPU, or
be certain of SSH.

## 6. Tests

```bash
make test              # go vet and unit tests; no daemon needed
make test-integration  # against real Incus and the real card; destructive
```

The integration suite refuses to run while anything else is up. It needs the
`rig-nvidia` image. `TestGuestVerbs` needs no card:

```bash
go test -tags integration ./integration/ -run TestGuestVerbs -v
```
