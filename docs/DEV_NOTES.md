# Dev notes

Things that were learned the hard way and would be learned again: facts about
Incus, the kernel and the tools that the code depends on, and failures that
look like something else. Decisions and their history are in DESIGN.md.

## Failures that stay silent

- **A second VM naming the same PCI address hot-unplugs the device from the
  first**, which keeps showing RUNNING with a healthy IP. This is why devices
  are claimed through one allocator under a lock, and why rig refuses rather
  than warns.
- **A VM without the ACL reaches this host's sshd** on every address the host
  holds, plus the LAN and any overlay network. `rig start` refuses an
  unisolated VM; `rig verify` is what proves the ACL does its job.
- **PCI addresses get renumbered.** A BIOS setting that removed one device
  moved the devices behind it, and an address-only manifest handed a VM the
  SATA controller. Hence `id:` and `devices.CheckIdentity` before any claim.
- **`rig doctor` can pass while the effect is absent.** It reads
  configuration. `rig verify` sends packets. Keep them separate.

## Devices

- Incus sets `driver_override=vfio-pci` on detach and does not clear it, so a
  device stays on vfio-pci after its VM stops. Except a `pci`-type xHCI, which
  Incus rebinds to `xhci_hcd` itself; `hostdev.Back` spots that and skips sudo.
- Returning a device **resets it** before loading the driver, then checks the
  recipe's sign of life (`alive:`) rather than trusting that the driver bound.
  Without the reset, an NVIDIA card comes back with no display; without the
  check, rig reports success anyway.
- Under GNOME, `gnome-shell` renders on the discrete card even with the monitor
  on the iGPU, so freeing the card means ending the session, not just the
  display manager. `rig host headless` lists what holds the card first.
- Passing a CPU-side AMD xHCI that shares a PCI device with the host's iGPU
  hung the host hard within the hour.
- A desktop session swallows the ACPI power button: xfce4-power-manager holds
  a low-level `handle-power-key` inhibitor, which `PowerKeyIgnoreInhibited`
  cannot override (logind.conf(5)). The desktop module leaves it out. `rig
  stop` pulls the plug when the timeout expires regardless, because a running
  VM holding host devices is worse than lost guest state.

## Network

- Incus 6.0 proxy devices on a VM support only NAT mode, which is host to guest
  through the NIC, where the ACL lives. That is why `rig forward` is vsock, not
  a proxy device, and why it leaves `rig verify` PROVEN.
- A published port crosses four things: the Incus NAT proxy, the per-instance
  ACL (`rig-fwd-<vm>`; without it the port hangs), the guest's own firewall
  (NixOS enables one), and the host's ufw FORWARD chain. The last drops LAN
  traffic while tests from the host itself pass, because the host's own
  traffic never crosses FORWARD. `rig doctor` checks the kernel log for ufw
  drops. Tailscale's `ts-forward` chain accepts before ufw, so a tailnet-only
  port can work while the LAN one does not.
- Don't run Tailscale inside a guest: it would be a tailnet node that can
  reach every other one, past the ACL. Publish on the host's tailnet address.
- Docker's default `172.17.0.1` exists on host and guest alike, so `rig verify`
  probing it from inside the guest hits the guest itself and reads as a
  breach. The docker module moves the guest's bridges to 10.201/10.202.

## Guests

- Incus creates a volume's missing parent directories as root. A volume at
  `/home/me/.config/app` leaves `~/.config` root-owned, and an XFCE session
  that cannot write its settings fails with "Unable to load a failsafe
  session". Create the parents in the image (a tmpfiles `d` rule).
- `nix develop path:<dir>` copies the whole directory into the store on every
  call, `.gitignore` and all: ten copies of one repo, 163 GiB, filled a VM. A
  bare path inside a git tree honours `.gitignore`.
- `claude-code` is unfree and flake evaluation is pure, so installing it by
  hand needs both `NIXPKGS_ALLOW_UNFREE=1` and `--impure`; the image's own
  `allowUnfree` does not reach a flake ref.
- Claude Code refuses `--dangerously-skip-permissions` as root unless
  `IS_SANDBOX=1` is set; every guest runs the agent as root.
- `claude -p` returns at a turn boundary, not at the end of the work, hence
  `rig agent start --until-done` and the agent-written DONE marker.
- The agent unit caps its memory and sets `OOMPolicy=continue`. Without the
  cap the guest hits a global OOM and the kernel picks a victim; without the
  policy systemd stops the whole unit when a child is killed.
- Refreshing an OAuth session on the host rotates the refresh token, which
  invalidates a `CLAUDE_CREDENTIALS_B64` snapshot in a running guest. `rig
  creds` replaces it without a restart.

## Live audio in a guest

From running a DAW with a passed-through USB audio interface.

- USB redirection of a single device (`kind: usb`) cannot carry isochronous
  audio at low latency. Passing the whole controller (`kind: pci`) measured
  the same round-trip latency as bare metal.
- Packet loss with no xruns anywhere: the controller's interrupt landed on
  vCPU 0 with everything else. Giving it a vCPU of its own (irq affinity, and
  systemd's `CPUAffinity` keeping everything else off it) took loss from over
  a thousand events per two minutes to a handful. xrun counters don't see this
  kind of loss; a loopback sample counter does.
