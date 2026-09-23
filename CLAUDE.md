# Working in this repo

`README.md` says what this is, what it contains and what a host needs;
`docs/RUNBOOK.md` sets the host up. This file is how to work here once it is.

One GPU, one VM at a time. The VM is where an unattended agent works; the
isolation around it is the product. Two failures are silent, and shape all the
tooling:

- Starting a second VM with the same GPU **hot-unplugs the card from the running
  one**. `incus list` still shows the victim RUNNING with a healthy IP.
- A VM created without the network ACL can reach **this host's sshd** on every
  address the host holds, plus the LAN and any overlay network the host is on.

## Which tool for what

`./rig` does everything: the base image, VM lifecycle, working inside a guest,
and the card and policy verbs. `rig --help` groups them; the third group —
`release`, `apply`, `start --allow-unisolated`, `host` — is the one that can
break an invariant, so reach for it deliberately.

`rig host` is the only part that touches this host rather than a guest. It needs
root, so everything but `status` re-execs itself under sudo, printing the
command first. `desktop` and `headless` are the card-and-display-manager case;
`return` and `free` are the generic routine they are made of, which `rig stop`
and `rig start` run for every device a manifest declares. Returning a device
**resets it** before the driver loads, and then checks the recipe's sign of
life appeared — a DRM node for the card, a USB bus for a controller — rather
than trusting that the driver bound; see docs/DESIGN.md, behaviour 6. Both
matter: without the reset the desktop comes back with no display, and without
the check rig says it succeeded.

`rig host headless` ends the desktop **session**, not just its display: under
GNOME, `gnome-shell` renders on the discrete card even when the monitor is on
the iGPU. It lists what holds the card before stopping anything, so you can see
whose session you are about to end.

What a VM is given is recorded on the instance (`user.rig.devices`), so later
starts honour it without flags. `rig new --no-gpu` records an empty list, for
CPU-only work: without it every `rig start` claims the GPU, so on a host whose
desktop is driving that card, starting any project VM kills the display. A
manifest (below) records exactly what it lists.

`rig stop` hands devices back. It asks the guest to shut down and, if the
guest is still running when `--timeout` expires — a desktop session swallows
the power button — pulls the plug, because a VM left running with the host's
devices inside it is worse than lost guest state. Then it detaches every host
device the VM held and returns each one that has a recipe recorded to the
host's own driver — which writes to sysfs, so expect a sudo prompt. Incus
sometimes rebinds a device itself on detach (a `pci` xHCI does); when the host
driver is already bound and the recipe's sign of life is there, rig says so and
asks for nothing. A device with no recipe stays
parked on vfio-pci, ready for the next VM; that is right for hardware the host
never uses itself. `--keep-devices` leaves everything attached, which is what
`rig restart` does, since returning a device only to claim it again is two
prompts for nothing. A stop rig did not perform — the guest powering itself
off — returns nothing; the next `rig stop` or `rig release` does.

`rig` covers the whole lifecycle, so raw `incus` should not be needed. If you
reach for it, that is a gap in `rig` — say so rather than working around it.

`rig push` will not overwrite a guest file that rig did not itself write. It
records what it wrote (hashes, in the guest at `/var/lib/rig/push.json`) and
refuses anything that changed underneath — naming each file, so you can look
before deciding. Re-pushing an edited staging directory still works; that is the
normal case and the guard is invisible to it. `--force` overrides, and is worth
hesitating over: `/work` and `/seed` live inside the instance, so the guest's
copy is often the only one. `rig pull --out` refuses to clobber a host file for
the same reason. An instance created before the manifest existed has no record,
so its first push asks for `--force` once.

The guest needs Claude Code on the unit's PATH before `rig agent` can run it.
The base image does not carry it, so which build a VM has is a project
decision, and a fresh VM has none until you put one there: `rig agent install
<vm>`. `rig agent start` checks before it creates any state, because the
alternative is a unit that restart-loops with `last exit 127` and says nothing
about why. The agent verbs are built around Claude Code — its flags, its
session resume, its event stream; docs/DESIGN.md lists what another agent would
need.

`rig agent` runs an unattended agent in the guest and keeps a channel to it.
It is a systemd unit, so it outlives your shell; its session UUID is fixed and
stored on the instance, so a crash **resumes the conversation** rather than
restarting the brief and redoing finished work. `rig agent send` queues a
message as a file — delivered at the next restart, and at the next turn if the
brief tells the agent to poll it. A second mission in the same VM wants the
machine, not the conversation, so `start --new-session` mints a fresh UUID and
leaves the old transcript on disk; without it the agent resumes a finished brief
and is told to redo work it has already done. A file, not a pipe: it survives a crash and
neither process can hang waiting for the other. `rig agent status` and
`rig agent log` are bounded reads, so checking often is cheap; `log` prints only
what the agent said, not the megabytes of tool calls around it.

`claude -p` returns when the model stops talking, which for a brief with several
missions in it is a turn boundary and not a result — the unit then stops, and an
engagement hours from done sits idle. `--until-done` treats a clean exit as a
turn boundary and resumes, until the agent itself creates
`/var/lib/rig-agent/DONE`. Only the agent's own marker ends it; rig does not
guess. Turn boundaries spend the restart budget, so `--until-done` raises
`--max-restarts` unless you set it yourself, and `rig agent status` reports the
marker — otherwise "finished" and "ran out of restarts" both read as
`unit inactive`.

The unit caps its own memory and sets `OOMPolicy=continue`, so a runaway child
— a geometry sidecar, a compiler — is killed alone instead of taking the agent
down with it. That combination is not decoration: without the cap the guest
reaches a global OOM and the kernel picks a victim by heuristic, and without the
policy systemd tears down the whole unit when it does.

`rig mount` gives you a live view of a guest directory on this host, without
copying anything out and without opening the isolation. Every network route in
is closed on purpose — the NIC rejects ingress, so ssh, vnc and network sshfs
are all refused — but the Incus API socket is a unix socket, not a network path,
which is why `rig exec` keeps working and why this can too. It needs `sshfs`
(`nix shell nixpkgs#sshfs --command rig mount <vm>`), it blocks while it holds
the mount, and the tree is root-owned so git wants a `safe.directory` line —
`rig mount` prints it when the directory is a repo. Treat it as read-only:
writing into a tree an agent is editing races that agent.

`rig forward` carries one TCP port between this host and a guest over **vsock**,
which is not IP — so the ACL that rejects every private range is untouched, and
`rig verify` still reports PROVEN with a tunnel up. Same reasoning as `rig
mount`: the way in is a channel that was never a network path. Incus proxy
devices cannot do this on a VM (6.0.5 allows only NAT mode there, which is
host-to-guest through the NIC, where the ACL lives).

```bash
rig forward myvm 9222              # guest:9222 -> 127.0.0.1:9222 here
rig forward myvm 8787 --to-guest   # here:8787  -> 127.0.0.1:8787 in the guest
```

It blocks while it holds the tunnel and tears down both ends on Ctrl-C. The
guest half is `socat`, which the base image carries; a guest made from an older
image is told so, and wants recreating rather than a package installed by hand.

The case it was built for is a **captcha**. An unattended agent cannot solve an
interactive challenge, and neither can an operator handed a screenshot: those
challenges are bound to the browser session that raised them and expire in under
a minute. Forward the browser's `--remote-debugging-port` instead and drive that
exact page, in that session, with its cookies. From a third machine, tunnel over
the ssh you already have (`ssh -L 9222:127.0.0.1:9222 <host>`) rather than
reaching for `--bind-all`, which hands a guest port to the whole LAN.

`doctor` and `verify` answer different questions. `doctor` reads configuration:
is this VM set up right? `verify` sends real packets from inside the guest: is
that setup actually true? Configuration has been right here while the effect was
absent, so the second is not implied by the first.

The base image carries the NVIDIA driver and, as the one deliberate exception,
**docker** — socket-activated, so a VM that never speaks docker never runs a
daemon. A daemon is the one dependency a project flake cannot supply on NixOS,
which is why it is here and nvcc is not. The trade: containers declaring
`restart: unless-stopped` do not come back by themselves after a guest reboot.

A project that needs more of the *machine* than the base gives it can ship a
**guest flake** — optional, and most projects should not have one:

```bash
rig image build --flake ./guest --alias myproj-guest
rig new myproj --image myproj-guest --env ~/.config/rig/myproj.env --start
```

`project-template/guest/` is a working example. It is three lines of flake
calling `rig.lib.mkGuest [ ./guest.nix ]`, because `rig image build` builds
`nixosConfigurations.gpubase` and that single output is the whole contract.

Reach for it exactly when you would otherwise run setup commands against a
running guest by hand — `nix profile install`, a wrapper dropped on PATH, a
directory that must exist at boot. That path produces a VM nothing describes,
and it is how one VM ended up with a wrapper and a directory nothing recorded:
the agent was handed a document describing a machine nobody had built. It also
fixes the two-PATH wart for free, because `writeShellScriptBin` in
`systemPackages` lands in `/run/current-system/sw/bin`, which both the login
shell and the agent unit search. Toolchains still belong in the project's
devShell flake; this is for the machine, not the build.

The alias a VM was created from is recorded on the instance, so `rig doctor`
measures drift against the image it was actually built from rather than against
whatever the default alias points at today.

Build with `make`. Go 1.26, one binary from one module.

## The manifest

`rig.yaml` says what a project wants its VM to be given, in Compose's shape
where Compose has one. `rig init` writes one with this host's card filled in:

```yaml
host:
  devices:
    gpu:
      kind: gpu
      pci: 0000:2b:00.0
      id: 10de:abcd                # what must be at that address (lspci -nn)
      return:                      # how the host takes it back after rig stop
        modules: [nvidia_drm, nvidia_modeset, nvidia_uvm, nvidia]
        reset: true
        alive: drm/card*           # sysfs proof the host driver brought it up
    desk-usb:                      # a whole USB controller: a monitor's hub, a dock
      kind: pci
      pci: 0000:3c:00.3
      id: 1022:1111                # required for kind pci
      return: { reset: true, alive: "usb*" }
    mouse:                         # one USB device by identity; hotpluggable
      kind: usb
      id: 1234:5678
guest:
  name: myproj
  build: ./guest                   # or image: <alias>; built as myproj-guest when missing
  cpus: 8                          # or a host CPU set to pin to: "4-7,12-15"
  memory: 16GiB
  disk: 40GiB
  env_file: ~/.config/rig/myproj.env
  devices: [gpu, desk-usb]
  volumes:                         # named volumes only; they outlive the VM
    - "myproj-data:/srv/data"
  ports:                           # host:guest/proto, published on the host's LAN address
    - "47989:47989/tcp"
    - "47998-48000:47998-48000/udp"
```

```bash
rig new -f rig.yaml --start        # create it from the file
rig apply -f rig.yaml              # bring an existing VM back to the file
rig doctor myproj                  # reports where the VM has drifted from it
```

Incus stays the record of what is true. Everything `rig new -f` reads it
records on the instance, so every later verb works from Incus alone; the file
is consulted again only to reconcile or to report drift. The `host:` block
repeats per project, on purpose: a project's file then describes everything
its VM needs. Unknown keys are errors — a typo silently ignored is a VM
configured differently from what its file says.

The three kinds are the whole vocabulary. `gpu` and `pci` pass one PCI
function through VFIO: **exclusive**, so at most one VM holds one and the host
loses it meanwhile, and the same silent hot-unplug applies to both if a second
VM names the same address. `usb` redirects one device by vendor and product
id, is hotpluggable, and leaves the host its controller. A controller must sit
**alone in its IOMMU group**; `docs/RUNBOOK.md` shows how to check. Which
rear port belongs to which controller is not knowable from sysfs: plug
something in and read `lsusb -t`.

**A PCI address is not a name.** Removing a device ahead of others on a bus
renumbers everything behind it, and a BIOS setting is enough: disabling the
Wi-Fi card here moved the chipset USB controller from `12:00.0` to `11:00.0`
and put the SATA controller at `12:00.0`, and the next start handed an
untrusted VM the host's disk controller. So `gpu` and `pci` devices carry
`id: vendor:device` (as `lspci -nn` prints it; required for `pci`, and `rig
init` fills in the card's), and `rig start` refuses, before claiming anything,
when the address holds something else, naming where the declared device is
now. `rig doctor` reports the same while the VM is stopped. A card without an
id must at least be an NVIDIA display controller.

`ports:` publishes guest ports to the LAN. Four things stand between a packet
at this host and a service in the guest, and the manifest arranges two: Incus
maps the port from the host's LAN address to the guest's, which needs the
guest to hold a fixed address, so rig pins one on its NIC; and the mapped
traffic still arrives at the guest's NIC as ingress, which the isolation
rejects, so rig attaches a second, per-instance ACL (`rig-fwd-<name>`) that
allows exactly those ports. **Verified here: without that ACL the mapped port
hangs.** The third thing is the guest's own firewall, which NixOS turns on by
default and which only the guest image can open (`openFirewall`, or
`networking.firewall.allowedTCPPorts`). The fourth is **this host's own
firewall**: ufw filters the forward path a mapped packet takes from the LAN
into the guest and drops it by default, while traffic from the host itself
takes a path ufw never filters — so a test from the host passes and a
laptop on the LAN fails, which is exactly what happened here. `rig doctor`
checks the first two, prints the `ufw route allow` lines for the fourth
when ufw is active, and calls it a failure when the kernel log shows ufw
dropping packets to the guest right now. A service that still does not
answer is the third. The vsock tunnel `rig forward` uses cannot stand in
for this: it carries one TCP port and nothing UDP.

Publishing on the LAN is not the only choice. Compose's `host_ip:` prefix
names the host address a port is published on, and the host's **Tailscale
address** is the useful one: `"100.64.0.7:47989:47989/tcp"` exists only
for tailnet peers, reaches the guest from anywhere the client has Tailscale,
and opens nothing on the LAN. It changes nothing about the isolation: the
guest still cannot reach the tailnet, and the mapped connections are
answered through the same connection tracking the LAN case uses. Do not
put Tailscale *inside* the guest instead — that makes the guest a tailnet
node that can reach every other one through the tunnel, past the ACL.

`volumes:` mounts Incus **custom storage volumes**, in Compose's short
form (`"name:/path"`) or a long one that adds `size:` and `owner: "uid:gid"`
(the volume is created owned by it; created root-owned otherwise, and an
application running as the desktop user cannot write to it). They live in
the VM's storage pool, so they survive `rig rm` and a new VM from a new image
mounts them again, which is how data outlives the recreation every image
change needs. `rig rm --volumes` deletes them too. Only named volumes: a host
path on the left, Compose's bind mount, is refused, because a directory of
this host inside the guest is a path out of the isolation. Mind the parents
of the path: Incus creates any that do not exist yet **as root**, so a volume
at `/home/me/.config/app` on a fresh VM leaves `~/.config` root-owned, and a
desktop session that cannot write its own settings does not start. Have the
guest image create them first (a tmpfiles `d` rule with the user as owner).

`cpus:` takes a count or a **pinned set** of host CPUs (`"4-7,12-15"`, one
vCPU each), for a guest that needs its vCPUs where it left them: a DAW,
anything with deadlines. Pinning is not an isolation boundary, but it can
hurt availability, so `rig new -f`, `apply -f`, `start` and `doctor` check it
against this host: every CPU must be online, the host must keep at least one
whole physical core, and taking only one thread of a core is warned about,
since the guest then shares that core with host work.

`network: none` goes the other way: no network device at all. `rig new -f`
masks the profile's NIC with a `none` device, `rig apply -f` puts the mask on
or lifts it (on a stopped VM, and around `ports:`, which a VM with no network
cannot have), and `rig doctor` reports "no network device" and any drift. This
is the posture for untrusted software rather than an agent: the isolation ACL
is an egress *denylist* and leaves the public internet open, which a VM with no
NIC does not have to reason about. Every rig verb still works, over vsock;
`rig verify` has nothing to probe from, so expect exit 2 there, and read
`incus config show --expanded` instead.

**A desktop on the card.** For a VM someone sits at — a monitor on the card's
DisplayPort, its peripherals on a USB controller passed through — the guest
flake adds `rig.nixosModules.desktop` to its module list and sets
`rig.desktop.user` in `guest.nix`. It is X11 on the NVIDIA card with XFCE and
autologin; `base/desktop.nix` says why X11 and what to expect. Expect a black
monitor from power-on until the guest's driver loads: the VM's firmware cannot
draw on the card. `rig.desktop.sunshine.enable = true` adds Sunshine so a
Moonlight client can use the session from another machine; its six ports are
the commented block in the template's `rig.yaml`, and the first image build
compiles Sunshine with CUDA, which is slow. Pair a client on Sunshine's own
page in the guest (port 47990, in the guest's browser at the monitor, or over
`rig forward <vm> 47990`) rather than publishing that port too. When the
release in the pinned nixpkgs has an advisory against it, a project pins a
newer one through `rig.desktop.sunshine.package`; the desk project's
`guest.nix` shows the shape, a source tag with its hash and a regenerated
`package-lock.json` for the web UI, since upstream ships none.

With a monitor that has a KVM switch, the day-to-day flow is `rig start`,
wait for the driver, switch the monitor to the card's input, and the keyboard
and mouse follow. Switch back and they return to the other machine. Two
things to know before choosing how they follow. Passing the whole controller
(`kind: pci`) is the cleanest, and on the reference host it **hung the
machine** within the hour; that controller shares a PCI device with the iGPU
driving the host's console, and AMD's integrated xHCI has a record of this
(docs/DESIGN.md, 2026-09-20). Passing the keyboard and mouse by identity
(`kind: usb`) leaves the controller with the host. Either way, the monitor's
host-side USB cable is also the host console's keyboard, so while the VM has
the devices the host's own screen has no input; drive the host over SSH, or
keep a keyboard on a controller no VM is given.

For a guest running software you do not trust, pass neither: **`rig host
input <vm>`** lends the host's keyboard and mouse as events instead. Both
Ctrl keys, pressed together and released, move input to the guest and back
(Scroll Lock lights while it is there, on a keyboard that has one); while the
guest has it the devices are grabbed and the host sees nothing. Events travel
one way over vsock to `rig-input` in the guest, which replays them through
uinput, so the guest sees a virtual keyboard and mouse and never the
hardware. That is the point: keyboards and mice with vendor configuration
interfaces (both on the reference host) can be programmed by whoever holds
them, and a macro stored in the keyboard by a guest would later type into the
host. `rig-input` comes with the desktop module (`rig.desktop.input.enable`,
on by default). The host half holds the devices, so it re-execs under sudo,
blocks while it runs, and drops input back to the host by itself if the guest
stops reading. `input: host` under `guest:` has `rig start` run it in the
background, as a transient unit (`rig-input-<vm>`) that ends by itself when
the VM stops; `--background` and `--stop` do the same by hand, and input goes
to one VM at a time. It is not QEMU's own evdev forwarding: Incus runs every
QEMU as `nobody`, which would then have to be able to read your keyboard,
and `nobody` is also Incus's dnsmasq; internal/input has the rest.

## Starting a new project

```bash
mkdir -p -m 700 ~/.config/rig                # credentials live outside any repository
printf 'ANTHROPIC_API_KEY=sk-ant-...\n' > ~/.config/rig/myproj.env
chmod 600 ~/.config/rig/myproj.env

./rig init proj                               # the project template, written out
./rig new myproj --env ~/.config/rig/myproj.env --start
./rig push myproj proj                        # -> /work/proj
./rig doctor myproj                           # isolation, devices, agent, address, creds
```

`rig init` writes the same files as `project-template/` from the copy built into
the binary, and points the guest flake at the base this rig was built from. From
a checkout, pushing `project-template` itself does the same job. It also writes
a `rig.yaml` naming the project and this host's card, so the same start reads
`./rig new -f proj/rig.yaml --start` when the file is the way you want to work.

`run-agent` gets the agent from the project flake. `rig agent` does not — it runs
a systemd unit with a fixed PATH, so the binary has to be in the guest's own nix
profile:

```bash
./rig agent install myproj
```

which runs `NIXPKGS_ALLOW_UNFREE=1 nix profile install --impure nixpkgs#claude-code`
in the guest. Both halves of that are load-bearing. `claude-code` is unfree, and
a flake reference does not read `~/.config/nixpkgs/config.nix` — flake evaluation
is pure — so the base image's own `allowUnfree` does not reach it either. Without
them nix fails with three suggested fixes, all of which are for the non-flake
path. A project that would rather pin its agent ships it in its guest flake.

Then prove the two things `doctor` cannot: that a kernel returns correct results,
and that the isolation holds against real traffic.

```bash
./rig exec --dir /work/proj myproj nix develop "path:." -c make run
./rig verify myproj    # 0 proven, 1 violated, 2 could not be proven
./rig exec --dir /work/proj myproj nix develop "path:." -c ./run-agent
```

`verify` exit 2 is not a pass. It means some check could not tell a blocked
guest from a broken probe, so it says nothing either way.

**Never put `path:` in front of a project directory in `nix develop`.** A
`path:` flakeref copies the whole directory into `/nix/store` on every
invocation, ignoring `.gitignore`, so a project with a large `target/` or
`node_modules/` writes it to the store again on each build. It fills the disk,
and the failure surfaces as ENOSPC from something unrelated long afterwards —
observed here as ten copies of one repo, 163 GiB, on a 197G VM. A bare path
inside a git working tree is resolved as a git tree and honours `.gitignore`,
so `git init` in the guest is load-bearing. `path:.` is safe only for a tree
that never accumulates build output, which is why `project-template` gets away
with it.

`rig rm myproj` deletes it — stopped only, and only VMs `rig` created. Its volumes stay
unless you add `--volumes`.

## Credentials

`rig start` pushes the `--env` file to `/run/rig/env` (0600, root) once the guest
is up. `/run` is tmpfs: credentials never touch the instance's disk or its Incus
config, and they die with the VM. `run-agent` sources that file.

Scoping the key is your call. `rig` refuses a file others can read, and refuses
lines that are not `KEY=VALUE`, but it cannot judge whether a key is narrow
enough.

To change them on an existing VM: `./rig restart <vm> --env ~/.config/rig/other.env`.
`start` takes `--env` too. Either one records the path on the instance, so it
sticks — the next plain `rig start` uses it. Keep these files outside any
repository (`~/.config/rig/` here), so none can be committed by accident;
`env_file:` in a manifest and `--env` both expand a leading `~/`.

If the VM is **running** and only the credential is wrong — the usual case,
since refreshing an OAuth session on this host rotates the refresh token and
invalidates a `CLAUDE_CREDENTIALS_B64` snapshot — use
`./rig creds <vm> ~/.config/rig/myproj.env` instead. It rewrites `/run/rig/env` in
place and stops there, so an unattended agent is not interrupted: the running
process keeps its unexpired token, and its next restart authenticates fresh.
Restarting the VM to fix one environment variable is the thing an agent cannot
survive.

The env file is always named, never inferred from what the instance last
recorded, and a relative path resolves against your current directory. Verbs
that take a VM say `<vm>`; `rig` does not assume it was run from this
directory, so the only thing tying a command to this checkout is a path you
type.

## Notes

`.claude/settings.json` allowlists `./rig` so routine work does not prompt. It is
convenience, not containment — the boundary is the VM and the network ACL.

`docs/DESIGN.md` has the decisions and why, what was learned proving them, and
what is still weak. `docs/RUNBOOK.md` is host setup. `project-template/README.md`
covers the CUDA toolchain.
