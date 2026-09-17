# rig

On Linux, `rig` creates isolated NixOS VMs, 
runs an unattended coding agent inside them, and proves, to the best of its ability using packets, 
that guest agents cannot reach the host, the LAN, or anything else they have no business reaching.

It is pretty much entirely vibe-coded, built on [Incus](https://linuxcontainers.org/incus/), 
supports GPU passthrough through VFIO, and relies on [Claude Code](https://claude.com/claude-code) as the
guest agent (but you could probably add support for others if you were so inclined). 


Sharing the GPU concurrently _is not_ supported, so you are only expected to work on one "project" 
(whatever that means to you) at a time.

It is best used by mentioning its existence, pointing to `rig --help`, talking a bit about how it should be used, 
and telling Claude to check in on the work of the unattended agent every so often.

## What the isolation is, and is not

An unattended agent runs with permission prompts off — there is nobody to
answer one. That is defensible only if the boundary is somewhere other than the
agent's own restraint. Here it is the VM and a network ACL on its NIC.

What is contained:

- **Egress** to every private range is rejected: `10/8`, `172.16/12`,
  `192.168/16`, `169.254/16` (link-local, and every cloud metadata address) and
  `100.64/10` (CGNAT, which Tailscale uses and RFC 1918 does not cover). The
  public internet is reachable, on purpose.
- **Ingress** to the guest is rejected entirely, and the guest runs no sshd.
  Every way in — `rig exec`, `rig shell`, `rig mount`, `rig forward` — is the
  Incus agent over vsock or the Incus API socket: channels the host opens and
  the guest cannot.
- **Credentials** are injected onto tmpfs in the guest at start. They are never
  in Incus config, never on the instance's disk, and gone when it stops.
- **Two silent failures** Incus permits are refused by the tooling: starting a
  second VM with the same GPU, which hot-unplugs the card from the first while
  the first keeps showing RUNNING; and starting any VM without the ACL.

What is not, stated so nobody has to discover it:

- The agent can reach the internet, so it can send out whatever it holds —
  including the credential it was handed. That credential sits in the guest's
  environment, readable by the agent and everything it runs; nothing keeps it
  out of the agent's context. The design that would — a host-side proxy over
  vsock that holds the key and injects it per request, so the guest holds only
  a proxy address and stripping the proxy setting yields a 401 rather than a
  bypass — is not implemented. Scope the credential; rig cannot.
- The agent is root inside the guest.
- The guest drives the GPU's DMA engine. The IOMMU confines it; rig adds
  nothing there.
- The bridge's resolver on udp/53 is reachable, because Incus's own DHCP/DNS
  rules precede any ACL. tcp/22 on the same address is blocked, which is what
  makes it a scoped exception rather than a hole.
- IPv4 only. A guest holding a global IPv6 address fails `rig verify` rather
  than being waved through by a denylist that does not cover it.

`rig verify` proves all of the above from inside a running guest and refuses
to call anything a pass that it could not tell from a broken probe. Its
targets are whatever this host actually has: its own addresses, the LAN
gateway and a neighbour, a Tailscale peer or a docker bridge when those exist.
A reject range with nothing of the host's in it is reported as moot rather
than proven; there is nothing there for the guest to reach. An integration
test strips the ACL from a guest and requires `verify` to notice, because a
verifier that always says "blocked" would pass everything else.

## What you need

- A Linux host with systemd. Developed on Ubuntu 26.04, kernel 7.0.
- **IOMMU** on, in firmware and kernel, and an **NVIDIA** GPU whose IOMMU group
  holds nothing the host needs — `vfio-pci` has to be able to take it. rig
  finds the card by PCI vendor, and the guest image carries the NVIDIA driver;
  nothing else is supported. A second GPU (an iGPU is enough) lets `rig host`
  give the card back to the host's desktop between runs; a headless host needs
  none.
- **Incus 6.0** or later with a managed bridge network, and your user in the
  `incus-admin` group. A storage pool with cheap clones (ZFS, btrfs) matters:
  on a `dir` pool every `rig new` copies the whole image.
- **Nix** with flakes enabled, to build the guest image. The project template
  uses a relative flake input, which needs Nix 2.26 or later.
- **Go 1.26** to build rig, unless you `go install` a release.
- Optional: `sshfs` for `rig mount`.

## Install

```bash
go install github.com/tbrockman/rig/cmd/rig@latest   # or, in a checkout: make
rig --help
```

The binary carries the image definition and the project template, so neither
needs a checkout. Then `docs/RUNBOOK.md`, once: a storage pool, a headless
host, the image, the ACL.

## Quick start

```bash
printf 'ANTHROPIC_API_KEY=sk-ant-...\n' > secrets/myproj.env
chmod 600 secrets/myproj.env

rig init proj                        # the project template, written out
rig new myproj --env secrets/myproj.env --start
rig doctor myproj                    # is it configured the way you think?
rig verify myproj                    # 0 proven, 1 violated, 2 could not be proven
rig push myproj proj                 # -> /work/proj in the guest; your own repos the same way
rig agent install myproj             # Claude Code, into the guest's nix profile; the image does not carry it
rig agent start myproj --prompt-file brief.md --workdir /work/proj --until-done
rig agent status myproj              # bounded and cheap; check often
rig agent log myproj                 # what it said, without the tool-call noise
```

`CLAUDE.md` is the operator's guide: which verb for what, and why each one
behaves as it does. It is written for a Claude Code session on the host that
drives `rig`, and reads the same for a person.

## Commands

`rig --help` groups them; each verb's `--help` says what it does and why.

| Project VMs | |
|---|---|
| `image build`, `image list` | Build the NixOS guest image from `base/` (or a project's guest flake) and import it, stamped with the store path it came from |
| `init` | Write the project template into a directory, pointed at this build's base |
| `new`, `start`, `stop`, `restart`, `rm` | Lifecycle. `start` claims the card and injects credentials; `rm` deletes only VMs rig created |
| `status`, `logs` | Who holds the card and which VMs exist; the console log for a VM that never came up |
| `doctor`, `verify` | `doctor` reads configuration; `verify` sends real packets from inside the guest |

| Working inside a guest | |
|---|---|
| `exec`, `shell` | A command, or a login shell, through the Incus agent |
| `push`, `pull` | Files in and out. `push` refuses to overwrite a guest file it did not itself write |
| `creds` | Replace the credential in a running VM without restarting it |
| `agent install/start/status/log/send/stop` | Claude Code, unattended, as a systemd unit in the guest, with a fixed session that survives crashes |
| `mount`, `unmount`, `forward` | A live view of a guest directory, or one TCP port, over channels that are not IP |

| The card and the policy | |
|---|---|
| `apply` | Declare and reconcile the isolation ACL and the profile's NIC settings |
| `claim`, `release` | Move the card between stopped VMs; `--force` is the only way to hot-unplug |
| `host desktop/headless/status` | Give the card back to this host's desktop, or take it away. Needs root |

Exit status is 0 or 1 except where a verb says otherwise: `verify` exits 2 for
"could not be proven", and `exec` carries the guest command's status out.

## Layout

| Path | Purpose |
|---|---|
| `cmd/rig` | The whole CLI surface |
| `embed.go` | `base/` and `project-template/`, embedded so a `go install`ed rig has them |
| `internal/incus` | Typed REST client over the unix socket; exec over websockets |
| `internal/gpu` | Card arbitration, PCI discovery, the lock |
| `internal/policy` | The declared isolation policy and its reconcile |
| `internal/verify` | Probes a live guest; refuses to pass what it cannot prove |
| `internal/agent` | The unattended agent: unit, runner script, log reading |
| `internal/creds` | Credential validation and injection |
| `internal/pushguard` | What `rig push` wrote, so it can refuse to destroy what it did not |
| `internal/hostgpu` | Moving the card between this host's desktop and VMs |
| `internal/vsock` | AF_VSOCK on raw file descriptors, for `rig forward` |
| `base/` | The guest image: a flake plus one NixOS module |
| `project-template/` | A per-project devShell, a CUDA correctness test, and an optional guest flake |
| `integration/` | Build-tagged tests against real Incus and the real card |
| `secrets/` | Gitignored. Per-project credential files |

| Document | |
|---|---|
| `CLAUDE.md` | How to work here: which verb for what, and the reasons |
| `docs/RUNBOOK.md` | Host setup, in order |
| `docs/DESIGN.md` | Decisions and why, what was learned proving them, and what is still weak |
| `project-template/README.md` | The CUDA toolchain and the correctness test |

## Status

A personal tool, developed and verified on one host: one NVIDIA card, x86_64,
Incus 6.0.5. The agent verbs are built around Claude Code; `docs/DESIGN.md`
lists what another agent would need, and ends with what would make the tool
harder to use over time, ranked by when it starts to hurt.

## License

GNU Affero General Public License, version 3. See `LICENSE`.
