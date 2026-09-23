# rig

`rig` runs things you don't fully trust in NixOS VMs on a Linux host, and
lets you decide exactly which of the host's resources each VM gets: the GPU,
a USB controller, one USB device, published ports, storage, pinned CPUs, your
keyboard and mouse. It's built on [Incus](https://linuxcontainers.org/incus/),
and is pretty much entirely vibe-coded.

It started as a sandbox for unattended coding agents
([Claude Code](https://claude.com/claude-code), running with permission prompts
off, where the VM is the only real boundary). The agent tooling is still here,
but rig has since become a general tool for giving a VM hardware and access
one declared grant at a time. It was last used to run [Septabee](https://www.youtube.com/watch?v=lW8Z98pXuSE), with a
passed-through audio interface and no network at all.

The general idea:

- **A VM gets almost nothing by default.** No route to the host or the LAN,
  no host directories, and no devices except the GPU, which `rig new` grants
  unless you pass `--no-gpu`.
- **Every grant is declared**, in `rig.yaml` or on the command line, and
  recorded on the instance, so `rig doctor` can tell you what a VM has and
  whether that still matches the file.
- **Grants are checked before they're made.** rig refuses to start a VM when
  its device is held by another VM, when a PCI address now holds a different
  device, or when the isolation ACL is missing.
- **Grants are handed back.** `rig stop` returns each device that has a
  return recipe to the host's driver, and checks that it actually came back.

One GPU, one VM at a time: sharing a card between running VMs isn't
supported.

## What you need

- Linux with systemd. Developed on Ubuntu 26.04.
- **Incus 6.0+** with a managed bridge, and your user in `incus-admin`. Use a
  storage pool with cheap clones (ZFS, btrfs); on `dir`, every `rig new` copies
  the whole image.
- **Nix** with flakes, to build guest images (2.26+ for the project template).
- **Go 1.26**, to build rig.
- For passthrough: the IOMMU on, and each passed device alone in its IOMMU
  group. GPUs must be NVIDIA; the guest image carries that driver. A second GPU
  (an iGPU will do) lets the host keep a display while the VM has the card.
- Optional: `sshfs` for `rig mount`.

## Install

```bash
go install github.com/tbrockman/rig/cmd/rig@latest   # or, in a checkout: make
```

The binary carries the guest image definition and the project template. Then
work through `docs/RUNBOOK.md` once: storage pool, image, ACL.

## An agent VM

```bash
mkdir -p -m 700 ~/.config/rig          # credentials live outside any repository
printf 'ANTHROPIC_API_KEY=sk-ant-...\n' > ~/.config/rig/myproj.env && chmod 600 ~/.config/rig/myproj.env

rig init proj                           # project template, plus a rig.yaml for this host
rig new myproj --env ~/.config/rig/myproj.env --start
rig verify myproj                       # prove the isolation with real packets
rig push myproj proj                    # -> /work/proj in the guest
rig agent install myproj                # Claude Code into the guest
rig agent start myproj --prompt-file brief.md --workdir /work/proj --until-done
rig agent status myproj                 # cheap; check often
rig agent log myproj                    # what it said, minus the tool calls
```

## A manifest

For anything beyond "a VM with the GPU", write the grants down. For example:

```yaml
host:
  devices:
    gpu:
      kind: gpu
      pci: 0000:2b:00.0
      id: 10de:abcd              # what must be at that address
      return:                    # how the host takes it back on stop
        modules: [nvidia_drm, nvidia_modeset, nvidia_uvm, nvidia]
        reset: true
        alive: drm/card*
    audio:                       # a whole USB controller, for an audio interface
      kind: pci
      pci: 0000:11:00.0
      id: 1022:2222
      return: { reset: true, alive: "usb*" }
guest:
  name: daw
  build: ./guest                 # a guest flake, built into an image when missing
  cpus: "4-7,12-15"              # pinned host CPUs, one vCPU each
  memory: 16GiB
  devices: [gpu, audio]
  volumes:                       # survive rm and recreation
    - { source: daw-documents, target: /home/me/Documents, owner: "1000:100" }
  network: none                  # no network device at all
  input: host                    # lend the host's keyboard and mouse while it runs
```

```bash
rig new -f rig.yaml --start      # create and start it
rig apply -f rig.yaml            # bring an existing VM back in line with the file
rig doctor daw                   # what it has, and where it has drifted
```

Why bother:

- **The file is the review.** Everything the VM can touch is in one place, and
  unknown keys are errors, so a typo can't quietly grant something different.
- **Recreating is cheap.** Every image change means a new VM; volumes carry the
  data across, and the file puts everything else back.
- **Addresses move.** A BIOS setting renumbered this host's PCI bus, and the
  next start would have handed a VM the SATA controller. Each `id:` makes rig
  refuse to start and name the device's new address instead.

`CLAUDE.md` covers every field: device kinds, `ports:` (including
Tailscale-only), volumes, CPU sets, the desktop module.

## Commands

`rig --help` groups them; each has its own `--help`.

| VMs | |
|---|---|
| `image build`, `image list` | Build a guest image from `base/` or a project's guest flake |
| `init` | Write the project template and a `rig.yaml` for this host |
| `new`, `start`, `stop`, `restart`, `rm` | Lifecycle. `start` claims devices; `stop` returns them; `rm --volumes` also deletes volumes |
| `status`, `logs` | Who holds which device; the console of a VM that won't boot |
| `doctor`, `verify` | Check the configuration; prove the isolation from inside |

| Inside a guest | |
|---|---|
| `exec`, `shell` | Run a command, or open a shell |
| `push`, `pull` | Copy files in and out; `push` won't overwrite what it didn't write |
| `mount`, `unmount` | A live view of a guest directory on the host |
| `forward` | One TCP port, either direction, over vsock |
| `creds` | Swap the credential in a running VM |
| `agent install/start/status/log/send/stop` | Claude Code as a systemd unit that resumes its session after a crash |

| Grants | |
|---|---|
| `apply` | The isolation ACL; with `-f`, a VM to its manifest |
| `claim`, `release` | Attach a stopped VM's devices; take every device back |
| `host input` | Lend the keyboard and mouse as events (both Ctrl keys toggle) |
| `host desktop/headless/return/free/status` | Move devices between the host and VMs (root) |

`verify` exits 2 for "could not be proven", which is not a pass; `exec`
passes the guest command's exit status through.

## Isolation

What holds:

- The guest's NIC rejects all ingress, and egress to every private range
  (`10/8`, `172.16/12`, `192.168/16`, `169.254/16`, `100.64/10`). `network:
  none` removes the NIC entirely. `ports:` opens only the ports it names.
- There's no sshd. Every way in (`exec`, `mount`, `forward`, `host input`) is
  vsock or the Incus socket, which the host opens and the guest can't.
- Credentials go to tmpfs in the guest: never on its disk or in Incus config.
- `host input` lends a keyboard and mouse as events rather than passing them
  through, so a guest can't reprogram a device and have it type into the host
  later.

What doesn't:

- The public internet is reachable unless you use `network: none`. A guest can
  send out anything it holds, including its credential, so scope the key.
- The agent is root in the guest.
- A passed-through device belongs to the guest. The IOMMU confines its DMA,
  but assume a guest could rewrite its firmware. Pass through only what you'd
  accept that for.
- The bridge's DNS resolver (udp/53) is reachable. Incus's own rules come
  before the ACL.
- IPv4 only: `verify` fails a guest that has a global IPv6 address.

## Docs

| | |
|---|---|
| `CLAUDE.md` | How to use it: which verb for what, and why. Written for a Claude Code session driving rig, and reads fine for a person |
| `docs/RUNBOOK.md` | Host setup |
| `docs/DESIGN.md` | Decisions, what was learned proving them, and what's still weak |

## Status

A personal tool, verified on one host (x86_64, one NVIDIA card, Incus 6.0.5).

## License

GNU Affero General Public License, version 3. See `LICENSE`.
