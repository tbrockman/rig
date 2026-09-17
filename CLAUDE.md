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
root, so `desktop` and `headless` re-exec themselves under sudo, printing the
command first; `status` does not. Reclaiming the card **resets it** before the
driver loads, and then checks a DRM node appeared rather than trusting that the
driver bound — see docs/DESIGN.md, behaviour 6. Both matter: without the reset the
desktop comes back with no display, and without the check rig says it succeeded.

`rig host headless` ends the desktop **session**, not just its display: under
GNOME, `gnome-shell` renders on the discrete card even when the monitor is on
the iGPU. It lists what holds the card before stopping anything, so you can see
whose session you are about to end.

`rig new --no-gpu` makes a VM that never claims the card. Use it for CPU-only
work: without it every `rig start` claims the GPU, so on a host whose desktop is
driving that card, starting any project VM kills the display. The choice is
recorded on the instance, so later starts honour it without the flag.

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
rig new myproj --image myproj-guest --env secrets/myproj.env --start
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

## Starting a new project

```bash
printf 'ANTHROPIC_API_KEY=sk-ant-...\n' > secrets/myproj.env
chmod 600 secrets/myproj.env

./rig init proj                               # the project template, written out
./rig new myproj --env secrets/myproj.env --start
./rig push myproj proj                        # -> /work/proj
./rig doctor myproj                           # isolation, GPU, agent, address, creds
```

`rig init` writes the same files as `project-template/` from the copy built into
the binary, and points the guest flake at the base this rig was built from. From
a checkout, pushing `project-template` itself does the same job.

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

`rig rm myproj` deletes it — stopped only, and only VMs `rig` created.

## Credentials

`rig start` pushes the `--env` file to `/run/rig/env` (0600, root) once the guest
is up. `/run` is tmpfs: credentials never touch the instance's disk or its Incus
config, and they die with the VM. `run-agent` sources that file.

Scoping the key is your call. `rig` refuses a file others can read, and refuses
lines that are not `KEY=VALUE`, but it cannot judge whether a key is narrow
enough.

To change them on an existing VM: `./rig restart <vm> --env secrets/other.env`.
`start` takes `--env` too. Either one records the path on the instance, so it
sticks — the next plain `rig start` uses it. `secrets/` is gitignored.

If the VM is **running** and only the credential is wrong — the usual case,
since refreshing an OAuth session on this host rotates the refresh token and
invalidates a `CLAUDE_CREDENTIALS_B64` snapshot — use
`./rig creds <vm> secrets/myproj.env` instead. It rewrites `/run/rig/env` in
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
