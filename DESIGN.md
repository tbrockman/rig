# Design notes

Decisions and why, what was learned proving them, and what is still weak.
Written 2026-08-23; last updated 2026-09-16. `README.md` says what this is;
`CLAUDE.md` is how to use it.

## Goal

An isolated VM with real GPU access for an unsupervised coding agent:

    monitored claude code on host  ->  Incus VM (GPU passed through)
                                        ->  unsupervised agent on a project

One VM at a time, one per project, stopped when unused. Remote paid models power
the agent; the GPU is for the software being built.

## Reference host

Everything here was developed and verified on one machine:

- AMD Ryzen 7 7800X3D, 60 GiB RAM, Ubuntu 26.04, kernel 7.0
- RTX 4080 SUPER (Ada, `10de:2702`, audio function `10de:22bb`) in an IOMMU
  group of its own plus the bridge above it — no ACS override needed
- An AMD iGPU driving the console, so the card can leave without taking the
  only display with it
- Incus 6.0.5, Nix 2.34, Go 1.26
- A loop-backed ZFS pool on the LUKS-encrypted root, ARC capped at 8 GiB

**The card is in a chipset slot wired for x2.** 3.19 GB/s H2D and 3.30 GB/s D2H
is ~82% of PCIe 4.0 x2, not a training failure; on-card D2D is 316 GB/s. It
matters because an agent profiling GPU code at x2 draws wrong conclusions about
transfer bottlenecks. Nothing in rig depends on it: PCI addresses are
discovered, so moving the card needs no config change.

## Decisions, and why

- **Incus, not microsandbox.** microsandbox optimises disposable microVM spawn;
  this is one long-lived VM. Incus has passthrough, ACLs and profiles natively.
- **VFIO passthrough, not virtio-gpu/venus.** Venus is Vulkan-only — no CUDA.
  Also a smaller host attack surface: no virglrenderer parsing guest commands.
- **NixOS guest.** One base image; per-project toolchains in project flakes.
- **Image is a build artifact** of `base/flake.nix`, not `incus publish` of a
  mutated instance.
- **No state store.** Incus is the source of truth; everything is derived. The
  only persistent artefact is a lock file.
- **No Terraform.** Nix + `incus admin init --preseed`, except preseed does not
  cover `network_acls` — hence `rig apply`.
- **Dynamic vfio binding.** Static binding needs boot-framebuffer workarounds
  when the dGPU is firmware-primary.
- **Go, not shell.** Typed, testable, and the Incus REST API is reachable
  directly, so nothing parses CLI output. One module, one binary. `rig` was
  briefly split in two to encode which verbs are dangerous, and `hostgpu` was a
  third for the one thing that needs root; command groups in `--help` and a
  sudo re-exec do both jobs without the splits. Reclaiming the card is an
  ordinary step in the lifecycle, and a tool you have to remember exists
  separately is one you forget at the moment it matters.
- **A hand-written Incus client, not the Incus Go module.** `internal/incus`
  is under a thousand lines over the REST API on the unix socket, with two
  dependencies: a websocket library for exec and `x/net` for ICMP. Importing
  `github.com/lxc/incus/v6/client` means importing the Incus repository as a
  module — its `shared/api` types and everything they pull in — for a dozen
  endpoints. It is also why nothing shells out to the `incus` CLI except
  `rig mount`, which wraps `incus file mount` because the SFTP-over-agent
  plumbing behind it is not worth reimplementing. The wire contract the client
  depends on — request shapes, ETag round-trips, the envelope's quirks, the
  exec websocket handshake — is pinned by unit tests against a fake daemon on
  a unix socket; whether Incus then does the right thing is the integration
  suite's question, and a fake cannot answer it.
- **Incus is a prerequisite, not embedded.** Incus is a root daemon that owns
  QEMU, VFIO binding, nftables, dnsmasq, the storage pools and its own
  database. It is system infrastructure the way docker is, and it comes with
  the distro's packaging, its updates, and the guest agent the image needs.
  rig is the unprivileged client of that. Embedding it would make rig the
  daemon — root, long-running, and welded to one Incus version's internals —
  to save an operator one package install and one `incus admin init`. The
  licence (Apache-2.0) would allow it; nothing else recommends it.
- **The base and the template are embedded in the binary.** A `go install`ed
  rig has no checkout, so `rig image build` had nothing to build and a new
  project had nothing to copy. Both directories are `go:embed`ded (`embed.go`)
  with explicit patterns that a test holds to `git ls-files`, so a stray build
  product can never ship. `rig init` writes the template out and points its
  guest flake at the base this build came from — a release names its tag on
  GitHub, a development build a copy under the cache directory — and
  `rig image build` falls back to that same copy when there is no `./base`.
  The cost is that the template a binary writes is the template it was built
  with, which is the point: it is the one that binary's image build agrees
  with.
- **socat is in the base image.** It was a project's job to ship it, so
  `rig forward` failed on any fresh guest until someone remembered — the
  provisioning-by-memory the image exists to end. It is rig's dependency, and
  rig's image carries it.
- **Checking and proving are separate verbs.** `doctor` reads configuration;
  `verify` sends real packets from inside the guest. Config has been right here
  while the effect was absent, so one does not imply the other.
- **The acceptance suite is a CLI verb, the invariant suite is a Go test.**
  `verify` is something you run on a VM before handing it over. The invariants
  are something you run after changing `rig`, and they need to build states
  `rig` exists to refuse, so they are build-tagged out of `go test ./...`.
- **`rig push` refuses to destroy guest-side work.** It hashes what it writes
  into a manifest in the guest and refuses to overwrite anything that changed
  since. The alternative — requiring `--force` for any overwrite — was rejected:
  re-pushing an edited staging directory is the normal workflow, so that rule
  would be forced off within a day and protect nothing. The distinction that
  matters is not "does this exist" but "did anyone but rig touch it". Learned
  the expensive way: a stale staging directory silently destroyed an unattended
  agent's decision log, and `/work` is the only copy of anything.
- **The unattended agent is a job, not a conversation.** `rig agent` runs it as
  a systemd unit writing to files, and the operator pulls from those files on
  demand — nothing streams into the operator's context by default. The session
  UUID is fixed by rig and stored on the instance, which is what makes
  `Restart=on-failure` correct rather than destructive: a restart resumes the
  transcript instead of re-reading the brief from the top. Learned by losing a
  run to an OOM and cold-restarting it, where only the agent's git discipline
  saved the work.
- **Transcripts live on disk, credentials on tmpfs.** These were conflated at
  first — the whole config dir sat on `/run`, so a VM stop would have destroyed
  every transcript and with it any chance of `--resume`. `projects/` is now a
  symlink to `/var/lib/rig-agent/projects`; the credential still dies with the
  VM.
- **Credentials are env vars in a host file**, injected to tmpfs at start.
  Scoping them is the operator's job; nothing else can judge it.
- **Docker is in the base image, socket-activated.** The stated rule is that the
  image carries the driver and toolchains come from project flakes, and docker
  breaks it — deliberately, because it is a *daemon*, and on NixOS enabling a
  service is system configuration that no devShell can provide. The alternatives
  were a per-project guest module applied with `nixos-rebuild` in the guest (the
  doctrinally correct answer, and a whole subsystem for one dependency) and
  rootless podman from a flake (which still wants `/etc/containers` wired up at
  system level). `enableOnBoot = false` is what makes it honest: a VM that never
  speaks docker runs no daemon and gets no bridge, so the cost is disk in one
  image every instance CoW-shares. The trade is that `restart: unless-stopped`
  containers do not return by themselves after a guest reboot.
- **A clean exit is a turn boundary, not a result — when asked.** `claude -p`
  returns whenever the model stops talking, so a brief with four missions in it
  stops after the first turn and waits for a human who is not watching. The
  microsandbox harness solved this with a shell loop that re-invoked
  `--continue` and guessed at "finished" by grepping the transcript. `rig agent
  start --until-done` keeps the loop and fixes its termination condition: the
  runner reports EX_TEMPFAIL on a clean exit so systemd resumes the session, and
  only the agent's own `DONE` marker ends it. It also leaves a note saying which
  kind of exit it was — a turn boundary and a crash are both non-zero and both
  bump NRestarts, and telling an agent it crashed when it merely paused sends it
  to reconcile a tree nothing interrupted.

## Provisioning is a flake, not a remembered sequence (2026-09-02)

Bringing one project's VM up needed `nix profile install claude-code socat`, a
devShell wrapper installed on PATH, and a `mkdir` — none of which was written
down anywhere. The agent then found the wrapper and the directory missing while
its environment document promised both, and spent a turn creating them. A VM
assembled from remembered commands is a VM nothing describes, which
is the exact state the "image is a build artifact" decision exists to prevent;
it was simply being violated one `rig exec` at a time.

Rejected: a Containerfile-style imperative layer list. The guest is a VM, Incus
does not build VM images that way, and NixOS already has the declarative
mechanism — the base is a flake already.

`base/flake.nix` now exposes `nixosModules.gpu-dev` and `lib.mkGuest`, so a
project can ship an optional guest flake adding its own modules. `rig image
build` already took `--flake`/`--attr`/`--alias` and `rig new` already took
`--image`, so no new verbs were needed; the missing piece was only that the base
was not importable. `project-template/guest/` is the worked example.

It also dissolves the two-PATH wart below: a wrapper in `systemPackages` lands
in `/run/current-system/sw/bin`, which both the login shell and the agent unit
search, so there is no longer any reason to write to `/usr/bin` by hand.

`rig new` now records the alias it used on the instance, and `rig doctor`
measures drift against that rather than the default — otherwise a project with
its own guest image is reported as drifted from a base it was never built from,
which is the same "reports on something other than what it names" failure as the
three below.

## `path:` flakerefs fill the disk (found by the agent, 2026-09-02)

The devShell wrapper handed to one agent ran
`nix develop "path:/work/<project>"`. A `path:` flakeref copies the entire
directory into `/nix/store`, ignoring `.gitignore`, on every invocation — so
each build wrote the repo's 19G `target/` to the store again. The VM reached
191G/197G and every command started failing ENOSPC; `/nix/store` held ten copies
of one repo, 163 GiB. The agent diagnosed it, switched to a bare flakeref (a git
tree, which honours `.gitignore`), reclaimed 163.2 GiB, and pinned the devShell
as a GC root so collection would not then discard the toolchain.

Two things worth keeping from it. The failure arrives as ENOSPC from something
unrelated, long after the cause, so the symptom points nowhere near the bug. And
a bare path only helps if the directory is a **git working tree** — otherwise
nix falls back to copying everything, so `git init` is load-bearing rather than
incidental. `project-template` uses `path:.` and gets away with it only because
that tree never accumulates build output.

## What `verify` could not see (found 2026-09-01, fixed)

Three bugs, all found by adding docker to the base image, and all the same
shape: something reported on a thing other than the one it named. Each was
invisible while a guest held exactly one global address.

1. **A probe at an address the guest also holds never leaves the guest.** Docker
   picks `172.17.0.1/16` for `docker0` on every machine, so a host running
   docker and a guest running docker hold the same address. `verify` probes the
   host's addresses from inside the guest; that one reached the *guest's* sshd,
   one hop, never touching the wire — and was reported as the guest reaching the
   host, the most serious verdict the tool has. Failing loudly was luck: the same
   collision on a target expected to be *reachable* would have produced a
   comfortable PASS for a path that was never tested. `verify` now asks the guest
   which addresses it holds and refuses to draw a conclusion from those; the base
   image moves docker to `10.201.0.1/16` so the collision does not arise.

2. **`rig doctor` reported a docker bridge as the guest's address.**
   `GlobalIPv4` returned the first global IPv4 it found while iterating a Go
   map, which is random order. That was deterministic only while a guest had
   exactly one global address; with docker in the image there are two, and the
   reported address started changing between calls. It now matches the NIC by
   the MAC in `volatile.<device>.hwaddr`, which is the only unambiguous link
   between the device rig configured and whatever name the guest's kernel chose.

3. **`--allow-gap` was decoration.** An acknowledged range still produced a
   non-advisory `Unproven` check, so the run stayed INCONCLUSIVE for exactly the
   reason the operator had already accepted — the flag could never let anything
   pass. It went unnoticed because `169.254.0.0/16` had a real proof on the day
   the suite was written: something on this host answered on
   `169.254.169.254:80`. It stopped answering, and a flag that had always been
   decoration became visible. An acknowledged range now makes its own probes
   advisory when they come out unproven — and only then. A guest that *reaches*
   an acknowledged range is still a breach.

## Wart: a rig guest has two PATHs (resolved by guest flakes)

`rig exec` runs a login shell, whose PATH on NixOS is nix profiles plus
`/run/current-system/sw/bin` — and no `/usr/bin`. The agent's systemd unit
declares its own PATH, which *does* include `/usr/bin`. So a helper installed
by hand at `/usr/bin/foo` is on the agent's PATH and not on the operator's, and
the same command works for one and not the other. Hit while installing a
devShell wrapper by hand.

`/usr/bin` is the only writable directory on either list — every other entry is a
read-only nix profile — so there is nowhere to put a helper *by hand* that both
find by name. The guest flake is the answer: a package in
`environment.systemPackages` lands in `/run/current-system/sw/bin`, which both
search. Until a project has one, `rig exec` needs the absolute path.

A later correction (2026-09-17, found by an integration test with a stand-in
`claude` in `/usr/bin`): the unit's declared PATH did not reach the agent
either. The runner starts under a login shell so `/etc/profile` can set up the
nix environment, and NixOS's `/etc/profile` exports PATH *absolutely* — measured
by starting a unit with `PATH=/marker/bin` and reading `$PATH` from its login
shell: no `/marker/bin`. So the agent searched the operator's list all along,
`nix profile install` worked only because both lists contain
`/root/.nix-profile/bin`, and `rig agent start`'s preflight checked a list
nothing used — a binary in `/usr/bin` passed the check and then restart-looped
on exit 127. The unit now also passes its list as `RIG_AGENT_PATH`, and the
runner puts it back in front after the login shell; the preflight and the
runner finally search the same thing.

## A guest can reach this host over vsock (2026-09-01)

Relevant to anything that wants a host-side service without opening the ACL.

- **Incus proxy devices cannot do it for VMs.** `bind=instance` is
  container-only; 6.0.5 refuses with "Only NAT mode is supported for proxies on
  VM instances", and NAT mode is host→guest through nftables — the wrong
  direction, and through the NIC where the ACL lives.
- **vsock does.** Verified end to end: `socat VSOCK-LISTEN:8787` on the host,
  `socat TCP-LISTEN:8787 VSOCK-CONNECT:2:8787` in the guest, HTTP over it, while
  the same guest still could not reach `10.187.156.1` over IP. It is the channel
  the incus agent already uses, so it is not a new hole so much as an existing
  one named.
- This is the transport for a credential-injecting proxy: the guest holds no
  token, and an agent that strips its proxy settings gets a 401 rather than a
  bypass.
- **Both directions verified**, and `rig forward` now owns it. Host half is Go
  on raw AF_VSOCK file descriptors — Go's `net` package cannot wrap one, since
  `net.FileConn` returns EINVAL for any family but UNIX/INET/INET6 — and the
  guest half is `socat`, which has to run in the guest and is not worth shipping
  a binary for. `rig verify` still reports PROVEN with a tunnel up, which is the
  point: vsock is not IP, so there is nothing for an IP denylist to say about it.
- The motivating case was a **captcha**, and it exposed a design error of mine:
  the brief told the agent to screenshot a challenge and ask. That cannot work.
  Interactive challenges are bound to the session that raised them and expire in
  under a minute, so a PNG is unsolvable by the time anyone sees it — and
  screenshotting then tearing down the browser destroys the only thing worth
  handing over. The agent now keeps the browser alive with CDP exposed, and the
  operator drives the real page through the tunnel.

## Known Incus behaviours (verified here)

Behaviours the code now handles are documented at their handling site, not here.
What remains:

1. **The GPU is not released on VM stop.** Incus sets `driver_override=vfio-pci`
   and never clears it. `rig host desktop` does the rebind.
2. **Starting a second VM with the same GPU hot-unplugs it from the running
   one.** Loud on the VM that failed to start, **silent on the victim** — it
   still shows RUNNING with a healthy IP. This is what `rig` prevents.
3. **`dns.nameservers` + `security.acls` are incompatible** on 6.0.5: Incus emits
   the cross product of nameservers x address families and produces invalid
   nftables rules. `dns.nameservers` is unset, so this is not being triggered.
   Unnecessary anyway — Incus inserts DHCP/DNS rules ahead of ACL rules.
4. **Attaching any ACL flips the NIC to default-reject in both directions**, so a
   denylist ACL is a blackout until `egress.action=allow` is set. It reads as
   "the internet is a bit broken", because the bridge resolver keeps working.
   See `internal/policy`.
5. **An ACL's rules cannot be edited while it is attached** — Incus flushes an
   nftables chain it never created and fails. `rig apply` detaches, rewrites
   and reattaches, and refuses while a consumer is running. See
   `policy.rewriteACL`.

6. **A card handed back from a guest must be reset, or the driver binds to a
   dead adapter.** Incus does not reset on release, and the guest's driver
   leaves state the host's cannot boot on top of. On Ada the GSP firmware
   fails: `kgspWaitForGfwBootOk_TU102 ... (the GPU may be in a bad state and
   may need to be reset)`, then `RmInitAdapter failed! (0x62:0x65:2028)`.
   Everything shallow still looks healthy — `lspci` reports
   `driver in use: nvidia`, `/dev/nvidia0` exists — while `nvidia-smi` finds no
   devices, no DRM node appears, and the DisplayPort stays dark. The card also
   keeps issuing DMA against the guest's old mappings: 120 `AMD-Vi
   IO_PAGE_FAULT` events a minute until it is reset. An FLR
   (`reset_method: flr bus`) clears all of it without a reboot. `rig host
   desktop` resets between unbind and modprobe, and verifies a DRM node
   appears, because binding is not working. Diagnosed 2026-08-25 after the
   first real reclaim; the fault was silent in exactly the way this project
   exists to prevent.

## What has been verified

- CoW clones: `rig new` takes 1.5 s and adds no pool usage.
- `rig image build` builds and imports `nixos-gpu-base`, stamping it with the
  store path it came from. A rebuild that changes nothing is a no-op, and
  `rig doctor` reports a VM created from an older image.
- **Passthrough verified in-guest:** RTX 4080 SUPER, 16376 MiB, driver
  595.71.05, CUDA 13.2. `hardware.nvidia.open = true` works on Ada.
- **CUDA compute verified, not just `nvidia-smi`:** all 4,194,304 elements
  bit-exact under two launch geometries, with poison and negative-control checks
  passing. 3.11 GB/s H2D / 3.24 GB/s D2H in-guest, matching the host-side x2
  measurement — passthrough costs nothing measurable on top of a narrow link.
- **Isolation verified empirically:** `rig verify` 28 passed, 0 failed, verdict
  PROVEN — every declared reject range has at least one attributable proof,
  including 169.254.0.0/16, which the old shell suite could only skip.
  `go test ./...` green; the integration suite green against the real card.
- The whole path works end to end: `rig new` → `start` (GPU claimed, credentials
  injected) → `doctor` → CUDA test → `rig verify` → `run-agent`.
- **The guest verbs, through the built binary** (`TestGuestVerbs`, 2026-09-17,
  no card needed): `rig agent start` refuses a guest with no agent binary; with
  a stand-in `claude` in `/usr/bin` the unit starts a session, exits at a turn
  boundary, is resumed by systemd with `--resume` and the "your previous turn
  ended" note rather than the crash note, receives a queued operator message,
  and completes on the agent's own `DONE` marker; `rig push` refuses to clobber
  a guest-side edit and `--force` overrides; `rig forward` carries a round trip
  and its guest half is gone after Ctrl-C. Writing it found three bugs in an
  afternoon: the login shell discarding the unit's PATH, a backgrounded list
  that hung `rig forward`, and the tunnel announced before its listener was up.

### What the isolation is

`vm-isolate` egress-rejects `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
`169.254.0.0/16` and `100.64.0.0/10` (CGNAT/Tailscale — not covered by RFC1918
and a real gap once), with `egress.action=allow` and `ingress.action=reject` on
the NIC. Declared in `internal/policy`, reconciled by `rig apply`.

Before it was attached, the guest could reach the host's sshd on all five
addresses the host holds — including its tailnet address — plus the LAN gateway,
a LAN peer, an online tailnet peer and the router's DNS.

What is proven and what is not:

- **Attributable to the ACL:** every host address on tcp/22 (INPUT, not FORWARD,
  so nothing else is doing the blocking), the LAN gateway on tcp/80, ICMP to the
  gateway / a LAN peer / a tailnet peer, UDP/53 to the router.
- **Not attributable:** the docker *container* targets — Docker's own FORWARD
  rules drop cross-bridge traffic anyway. Kept as outcome checks, labelled.
- **Proven, unexpectedly:** `169.254.0.0/16`. Something on this host answers on
  169.254.169.254:80, so the range has a real attributable proof rather than the
  permanent skip the shell suite reported. `verify` still carries it as an
  acknowledged gap by default, because on a host where nothing answers there the
  honest outcome is "unproven", not "passed".
- **Not inspected:** the generated nftables ruleset — `sudo` needs a password.
- **The detector itself is tested.** `TestVerifyDetectsAnUnisolatedGuest` starts
  a guest with the ACL stripped and requires `verify` to call it a violation.
  Without that, a `verify` that always answered "blocked" would pass everything.
- **Residual, not exploitable:** the WAN address is in no reject range, so a
  guest could hairpin to anything port-forwarded. The router does not hairpin, so
  it cannot be demonstrated from inside. No rule added: one keyed to a dynamic
  ISP address fails open silently, which is worse than a documented gap.
- The bridge resolver on **udp/53 is reachable by design** (Incus inserts
  DHCP/DNS rules ahead of ACL rules). Scoped, not a hole: tcp/22 on the same
  address is blocked.

## Parked — IPv6 (do not re-investigate without new information)

**The reference host's ISP delegates no IPv6 prefix** (verified 2026-08-23 on
the router: the WAN v6 link never comes up), so `ipv6.address=none` stays on
the bridge. That is correct, not a workaround:
bridge IPv6 with no upstream route would add a bypass around IPv4-only ACL rules
and cause AAAA-first stalls.

This does not block testing that software handles IPv6 — a bridge ULA gives a
working v6 stack for socket code and happy-eyeballs. Only reachability to real
v6 hosts is unavailable.

If delegation ever appears: enable bridge IPv6 with **default-deny egress on
IPv6 only** plus a short allowlist, keeping IPv4 as a denylist. No ISP-prefix
tracking — a delegated prefix changes, and a denylist keyed to it fails open.
Verify first that Incus supports a per-address-family default egress action.

Also rejected: policy routing to give guests a table with only a host route.
Prefix-agnostic and guest-only, but it lives outside the Incus ACL model and
fails open if the rule is lost.

## Deferred

Snapshots/rollback, remote API access, multi-GPU, any scheduler. The constraint
is one card, one active project.

## What would make this harder over time

Ranked by when it starts hurting. Two earlier items are now closed: the base
image is stamped and `rig doctor` reports drift, and `rig verify` fails when
coverage of a declared reject range drops to nothing rather than skipping
quietly.

1. **VMs are not garbage-collected.** Stopped project VMs accumulate at ~6 GiB
   each on a 500 GiB loop file, and a full root means write errors on a ZFS
   vdev. `rig image build` now deletes the image it replaced, but instances are
   still nobody's job. `rig status` should show age and size, and there should
   be a way to list what is stale.
2. **`/work` lives inside the instance.** `rig rm` destroys the project with the
   VM, so the VM is the only copy until someone pushes a git remote. Either put
   `/work` on a separate volume that outlives the instance, or have `rig rm`
   refuse when the tree has uncommitted changes.
3. **One card, one project is enforced but not scheduled.** With several
   projects, "who had it last, and does something want it now" becomes guesswork
   at the point where it is most annoying to add.
4. **The ACL is a denylist.** Every new private range someone invents is a gap
   until noticed. The IPv6 plan already says default-deny plus an allowlist; the
   same argument applies to IPv4 once there is any appetite for the churn.
5. **It is NVIDIA-only, and one card.** The CLI is parameterised (`--flake`,
   `--attr`, `--alias`, `--profile`, `RIG_*`) and nothing names a particular
   host, but the vendor is everywhere: discovery greps PCI vendor `10de`,
   `base/gpu-dev.nix` is `hardware.nvidia`, `internal/hostgpu` names the NVIDIA
   modules, and `project-template` is CUDA. Another vendor is a second driver
   in the image and a second reset path on the host, not a flag.
6. **The integration suite is the only thing that tests Incus itself, and it
   is easy to stop running.** `internal/incus` is now unit-tested against a
   fake daemon, but a fake only pins what rig sends; whether Incus honours it
   is the tagged suite's question, and that suite needs the daemon and the
   card. It once stopped compiling for two weeks behind a signature change,
   because a build tag hides a package from `go vet ./...` — `make test` now
   vets it explicitly, but nothing runs it but a human with the machine idle.
7. **The credential is in the guest.** `/run/rig/env` is tmpfs and dies with
   the VM, but while the VM runs the key is in the agent's environment and in
   the environment of everything it spawns. Nothing keeps it out of the agent's
   context; an agent that prints its environment has printed it. The design
   that would fix this is sketched under "A guest can reach this host over
   vsock": a host-side proxy that holds the key and injects it per request,
   with the guest holding only a proxy address, so an agent that strips its
   proxy settings gets a 401 rather than a bypass. `rig forward --to-guest` is
   the transport it would run over. It is not implemented, and until it is,
   scoping the key is the only control.
8. **`rig agent` is Claude Code-shaped.** "The agent is a project decision"
   means which build of Claude Code, and whether it comes from the nix profile
   or the guest flake — not which agent. The runner and the host side lean on
   Claude specifics in nine places: the binary name and its flags (`-p`,
   `--dangerously-skip-permissions`, `--verbose --output-format stream-json`,
   `--model`); `--session-id` and `--resume`, which let rig choose the session
   UUID up front so a restart resumes rather than restarts; the transcript
   under `projects/<dir>/<uuid>.jsonl`, which is the evidence the runner uses
   to choose between those two; the credential shapes (`CLAUDE_CODE_OAUTH_TOKEN`,
   `ANTHROPIC_API_KEY`, `CLAUDE_CREDENTIALS_B64`) and `CLAUDE_CONFIG_DIR`;
   `IS_SANDBOX=1` to run unattended as root; the stream-json event schema that
   `agent log` and `agent status` parse for speech and for the last error; the
   OAuth error text `ExplainError` recognises; the `command -v claude`
   preflight; and the `claude-code` package `agent install` names. Another
   agent would need six things from its side — a non-interactive run, a
   permission bypass, credentials by environment variable, a model flag, a
   parseable transcript, and a way to resume a conversation by an ID rig
   picked — and the last decides how well it works: without it a restart can
   only re-read the brief with a "check git log" note, which is the harness
   rig replaced. The refactor is an agent profile: one file per agent holding
   those nine facts, chosen by `rig agent start --agent` and recorded on the
   instance, with the runner taking its invocation from the environment rather
   than spelling it out. Roughly a day for an agent with resumable sessions.
   Not done, because there has been one agent.
