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

**A host firewall can drop the guest's traffic before the ACL sees it.** ufw's
default forward policy is DROP and its stock rules pass only ICMP echo, so a
guest pings the internet and reaches nothing else; the kernel log shows
`[UFW BLOCK] IN=incusbr0 OUT=<uplink>`. Let the bridge out through the
default route's interface (`ip route get 1.1.1.1` names it); the ACL still
rejects the private ranges:

```bash
sudo ufw route allow in on incusbr0 out on <uplink>
```

The rule is keyed on the interface name, and a BIOS change that renumbers the
PCI bus renames the uplink too, after which the rule matches nothing. `rig
verify` reports it as `expected reachable, got blocked`.

**NixOS hosts: DHCP needs one firewall rule.** The ACL makes the kernel run
conntrack at the bridge layer. For a broadcast frame the bridge conntrack
drops its entry again (it cannot confirm cloned skbs), so a guest's DHCP
DISCOVER reaches the host firewall with no state; NixOS's nftables firewall
treats stateless packets as invalid and drops them before dnsmasq sees them.
The guest then boots with a link-local address and `rig start` reports "no
IPv4 address". A VM without the ACL gets a lease, which is the tell. Mark DHCP
requests as untracked before conntrack runs:

```nix
networking.nftables.tables.rig-dhcp = {
  family = "bridge";
  content = ''
    chain pre {
      type filter hook prerouting priority -300; policy accept;
      udp dport 67 notrack
    }
  '';
};
```

Unicast traffic keeps its state, so this changes nothing else. Ubuntu's
default firewall does not filter by conntrack state and has no such problem.

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
the desktop and VMs without a reboot. `rig start` names anything on the host
still holding the card, and `rig start --kill-holders` ends it; for a display
manager's shell, stop the display manager instead, which `unit:
display-manager` in the card's `return:` does on every start. If your only monitor is on the card, you
have a console only while no VM holds it; put the console on a second GPU, or
be certain of SSH.

## 6. Proxied credentials (optional)

Only for a manifest with `guest.proxy.credentials`: a credential the guest
never holds, which a proxy on this host puts into requests for it. Each
guest gets a [witmproxy](https://witmproxy.rs) instance of its own, in your
user scope: rig makes it at the guest's first start with witmproxy's own
installer (`witm service install --user --instance rig-<vm>`), starts and
stops it with the VM, removes it with `rig rm`. No root for any of that.
Once per host:

```bash
cargo install witmproxy            # or a release binary; `witm` on PATH, with --user
rig proxy setup                    # the bridge rigproxy0, witm, lingering; the ufw rule under sudo
```

`rig proxy setup` also does the two things the proxies need of the host,
saying what it does; `--dry-run` only says. The proxies are units of your
systemd user manager, which stops at logout unless it lingers, so setup
lets it linger: on systemd 256 and later that is yours to grant and no root
is involved; an older logind asks an admin, and setup then prints the root
form, `sudo loginctl enable-linger $USER`. And a host firewall knows
nothing of the new bridge: with ufw active, setup admits the one range a
guest may reach on it, its proxy's port, under sudo with the rule printed
first (`rig doctor` reports the drops while it is missing):

```bash
sudo ufw allow in on rigproxy0 to 10.207.0.1 port 3102:3354 proto tcp
```

Another firewall is yours to open the same way.

The guest at `10.207.0.<n>` reaches its proxy at `10.207.0.1:<3100+n>` and
nothing else: the NIC's ACL allows that port alone, and the proxy's policy
(`~/.local/state/witmproxy/instances/rig-<vm>/policy.toml`, written by rig)
forbids everything but HTTPS to the hosts the manifest names, from that
guest (or, with `egress: internet`, permits that guest everything else
too), with the isolation ACL's reject list forbidden to anyone. A VM that
proxies credentials needs the image built after the proxy NIC settings
reached `base/` (section 4). `rig doctor <vm>` checks every piece; `rig
proxy log <vm>` shows what its proxy carried and refused.

## 7. Tests

```bash
make test              # go vet and unit tests; no daemon needed
make test-integration  # against real Incus and the real card; destructive
```

The integration suite refuses to run while anything else is up. It needs the
`rig-nvidia` image. `TestGuestVerbs` needs no card:

```bash
go test -tags integration ./integration/ -run TestGuestVerbs -v
```
