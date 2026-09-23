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

- **A VM gets nothing by default.** No route to the host or the LAN, no host
  devices, no host directories. Its NIC and root disk come from a `rig`
  profile, so nothing else on the host's Incus is touched.
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
- **Nix** with flakes, to build guest images.
- **Go 1.26**, to build rig.
- For passthrough: the IOMMU on, and each passed device alone in its IOMMU
  group. GPUs must be NVIDIA (`rig.nixosModules.nvidia` is the driver). A
  second GPU (an iGPU will do) lets the host keep a display while a VM has the
  card.
- Optional: `sshfs` for `rig mount`.

## Install

```bash
go install github.com/tbrockman/rig/cmd/rig@latest   # or, in a checkout: make
```

The binary carries the guest image definition and the project template. Then
work through `docs/RUNBOOK.md` once: storage pool, `rig setup`, image.

## Quick start

```bash
rig new scratch --start          # a VM with no host devices
rig verify scratch               # prove the isolation with real packets
rig shell scratch
rig stop scratch && rig rm scratch

rig init myproj --with nvidia    # myproj/rig.yaml granting the card, myproj/guest/ with its driver
$EDITOR myproj/rig.yaml          # grant anything else it needs
rig apply -f myproj/rig.yaml --start
```

`--with` takes `nvidia`, `docker` and `desktop`; without it, `rig init` writes
a manifest that grants nothing. `--image <alias>` skips the guest flake.

`rig new --gpu` is the shortcut for a VM with this host's NVIDIA card and
nothing else; it uses the `rig-nvidia` image.

## A manifest

Anything a VM is given goes in its `rig.yaml`. For example:

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
  flake: ./guest                 # nixosConfigurations.guest in ./guest/flake.nix
  cpus: "4-7,12-15"              # pinned host CPUs, one vCPU each
  memory: 16GiB
  devices: [gpu, audio]
  volumes:                       # survive rm and recreation
    - { source: daw-documents, target: /home/me/Documents, owner: "1000:100" }
  network: none                  # no network device at all
  input: host                    # lend the host's keyboard and mouse while it runs
```

```bash
rig apply -f rig.yaml --start    # create it, or bring it back in line with the file
rig doctor daw                   # what it has, and where it has drifted
rig delete -f rig.yaml           # remove it; its volumes stay unless --volumes
```

| Field | |
|---|---|
| `host.devices.<name>` | `kind: gpu` (an NVIDIA card), `pci` (any PCI function, such as a USB controller; `id:` required) or `usb` (one device by `id: vendor:product`). `return:` is how the host takes a PCI device back |
| `guest.flake` / `image` | A flake reference to build the image from, as `<name>-guest` when missing: `./guest` builds `nixosConfigurations.guest`, `./guest#daw` builds `.daw`, and remote refs (`github:…`) work. Or an image alias already built |
| `guest.cpus` | A count, or a set of host CPUs to pin to (`"4-7,12-15"`) |
| `guest.memory`, `disk` | Sizes |
| `guest.devices` | Which `host.devices` this VM gets |
| `guest.env_file` | `KEY=VALUE` credentials, injected to tmpfs in the guest on start |
| `guest.volumes` | Named Incus volumes: `"name:/path"`, or `{ source, target, size, owner }` |
| `guest.ports` | `"host:guest/proto"`, optionally prefixed with the host address to publish on (a tailnet address keeps it off the LAN) |
| `guest.network` | `none` for no network device |
| `guest.input` | `host` to lend the host's keyboard and mouse from start to stop |

The guest flake (`rig init` writes one) is rig's base plus the project's own
modules. rig provides `rig.nixosModules.nvidia` for a card, `.docker`, and
`.desktop` for an X11 session on the card.

## An agent VM

`examples/cuda-agent` is a GPU VM for an unattended coding agent: a CUDA
devShell, a test that the card really computes, and Claude Code in the image.

```bash
rig apply -f examples/cuda-agent/rig.yaml --start
rig push cuda-agent examples/cuda-agent          # -> /work/cuda-agent
rig agent start cuda-agent --prompt-file brief.md --workdir /work/cuda-agent --until-done
rig agent status cuda-agent      # cheap; check often
rig agent log cuda-agent         # what it said, minus the tool calls
```

Keep credential files outside any repository (`~/.config/rig/`).

## Commands

`rig --help` groups them; each has its own `--help`.

| VMs | |
|---|---|
| `image build`, `image list` | Build a guest image from `base/` or a project's guest flake |
| `init` | Write a `rig.yaml` and a guest flake; `--with nvidia,docker,desktop` adds modules |
| `apply -f`, `delete -f` | Create a manifest's VM or bring it back in line with the file; remove it |
| `new` | Create a VM without a manifest (`--gpu` for this host's card) |
| `start`, `stop`, `restart`, `rm` | Lifecycle. `start` claims devices; `stop` returns them; `rm --volumes` also deletes volumes |
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

| Grants and isolation | |
|---|---|
| `setup` | This host's isolation ACL and the `rig` profile, once |
| `claim`, `release` | Attach a stopped VM's devices; take every device back |
| `host input` | Lend the keyboard and mouse as events (both Ctrl keys toggle) |
| `host return/free` | What `stop` and `start` run for each device (root) |
| `host desktop/headless/status` | Move the NVIDIA card between the host's desktop and VMs (root) |

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

| Path | Description |
|---|---|
| `docs/RUNBOOK.md` | Host setup |
| `docs/DEV_NOTES.md` | Facts about Incus and the host the code depends on, and failures that look like something else |
| `docs/DESIGN.md` | Decisions, what was learned proving them, and what's still weak |
| `CLAUDE.md` | Conventions for working on rig, for a Claude Code session or a person |

## Status

A personal tool, verified on one host (x86_64, one NVIDIA card, Incus 6.0.5).

## License

GNU Affero General Public License, version 3. See `LICENSE`.
