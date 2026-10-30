# Design notes

Decisions and why, what was learned proving them, and what is still weak.
Written 2026-08-23; last updated 2026-10-30. `README.md` says what this is;
`docs/DEV_NOTES.md` has the facts the code depends on.

## 2026-10-30: the lead, and only the lead

The hook that delivers mid-turn fires for every tool call in the session,
the lead's and its subagents' alike, and until now `rig hook` read its
input and ignored it: whichever process made the next tool call got the
message. In one mission two instructions meant for the lead landed in a
subagent, which wrote "I'll pass it on to the lead" and did not; the
record said `path: hook`, as if delivered. The agent rig talks to is the
lead, the process that was given the brief; a subagent is a tool it runs.

The hook's input is the only evidence of which process it ran in: on
Claude Code 2.1.314 a subagent's carries `agent_id`, the lead's does not,
and the session and transcript are the lead's in both (DEV_NOTES). So the
hook delivers only when its input proves the lead, positively: the
session rig chose, and no agent identity. A subagent's call takes nothing
and the message waits for the lead's next tool call. An input rig cannot
place, a hook shape from some other version, proves nothing either; the
message then waits for the next launch, which is the lead's by
construction, and `stderr.log` says so once. The record says whom a
delivery reached: `to: lead` for the hook and the launch prompt, `to:
caller` for a verb, because the Bash environment is the same in the lead
and a subagent and nothing in the guest can tell who typed `rig wait`.
The system prompt tells the lead that only it receives operator messages
and that subagents must not run the verbs.

An acknowledgement protocol was considered for the fallback: a message
counts as delivered only when the lead's own process says so. There is
nothing the lead could say that a subagent could not say too, for the same
reason `rig wait` cannot tell them apart, so it would prove nothing. What
replaces it is the launch listing: each launch prompt lists every item
that reached the agent mid-turn since the previous launch, by id, path,
recipient and first line, so a lead whose context was compacted, or whose
run was killed by the watchdog, is reminded of what a hook added. The
harness says how it delivers (`Delivery()`), so `rig agent send` reports
the path and its latency from the harness rather than from a sentence
written for one.

## 2026-10-30: the manifest says how the agent runs, and what is held for it

The overseer of a mission needed facts that were flags to `rig agent
start` or lines in a brief: the prompt file, the workdir, the model, the
timeout, and the ports to hold forwards for. The forward died with the VM
when the image was rebuilt, and someone had to remember to restart it.
Now `guest.agent` carries the run's settings (its short form, the harness
name alone, still works), budgets, which are hard limits, and watch
thresholds, which are soft ones; and `guest.forwards` carries the ports.

**Forwards in the manifest.** A forward is a host process by nature: the
vsock listener is on the host side, and nothing in Incus or the guest can
hold a port on this host's loopback. So it is a grant like the others,
declared in the file and recorded on the instance, and `rig start` leaves
a holder, `rig-forward-<vm>`, a transient unit in the operator's user
manager, the way it leaves the handback watcher and the input forwarder
as root units. User scope because vsock needs no root and a preview is the
operator's own; `rig proxy setup`'s lingering keeps it past a logout, as
it does the proxies. It follows the VM, ends with it, and is left again at
the next start, so a VM recreated from a new image comes back with its
forwards. It was not put in the proxy: that is witmproxy's process, exists
only for a proxied VM, and carries HTTP, not a raw port. The schema, the
README and `rig status` say how `forwards` differs from `ports`: a
published port goes through the network and the ACL and can be bound to
an address other machines reach; a forward is vsock, loopback only, and
leaves `rig verify` PROVEN. For an operator's own preview, `forwards` is
the default and `ports` the exception; `rig doctor` notes a port named
under both.

**Defaults and records.** `rig agent start` resolves each option as the
flag given now, else what the last start recorded, else the file, else
the flag's default. The record stands in for the file once it exists,
since it was resolved against it, and `rig apply -f` clears the record
when the file's agent settings change, so a bare start then reads the
file. The alternative, the file always winning, would make `--model` a
one-start override that a bare restart silently undoes, which is the
forgotten-flag failure the record exists to prevent.

## 2026-10-16: a channel to the agent that is timely and signed

Four things were wrong between the operator and a running agent. A message
from `rig agent send` reached the agent only when the runner next launched
it: the runner read the inbox once, before `claude -p`, and a turn can run
for hours (`send`'s help said "if it is polling"; nothing polled). The agent
could not ask anything; its only signals were files the operator had to
think to read. `send` pulled the inbox, appended and wrote it back while the
runner copied and truncated it, so a message sent at a launch could vanish.
And nothing told the operator's words from any other text the agent read:
the inbox was a plain file the agent, root in the guest, could write itself.
A fifth surfaced alongside: the runner passed the whole brief as the prompt
at every launch, `--resume` included, so each turn boundary appended another
copy as a new user turn; one 9.4 MB transcript held the brief's heading 226
times, a third of the file, and plausibly helped it overflow its context.

**Delivery.** Each item is its own file in `inbox.d`, written under a dot
name and renamed into place, so nothing is read-modify-written. Takers claim
a file by renaming it into `claimed/`, then verify and settle under a lock,
in sequence order, so two takers at once cannot accept out of order or twice.
The runner takes at launch; a PostToolUse hook, which the runner hands
`claude` with `--settings`, takes after every tool call and returns what
verified as `additionalContext`, which reaches the model within the same
`claude -p` (DEV_NOTES). Hook context is not in the stream-json log, so each
delivery is logged to `deliveries.jsonl` with its path: launch, hook, wait.

**Asks.** The agent runs `rig ask <id> <question> [--wait 5m]`, which writes
`asks/<id>.json`; `rig agent answer` sends a signed answer, which `rig ask
--wait` or `rig wait` prints (exit 2 when none came; at most nine minutes,
under the Bash tool's ten) and the hook delivers otherwise. `rig agent watch`
polls the bounded probe `status` uses and prints one line per change, ask
opened or answered, delivery, tamper, DONE, unit and restart changes,
STATUS.md, with `--exec` for a notifier. `rig done` writes DONE.

**Authenticity.** Each VM has an ed25519 key on the host
(`~/.config/rig/agent/<vm>/operator.key`, 0600, made at its first start);
the guest gets the public half as `operator.pub`. Every item is an envelope
`{v, session, seq, time, kind, id, body}`, canonical JSON signed after a
domain-separation prefix. The guest accepts one only with a good signature,
for the session it runs, with a sequence number above the last it accepted
(the host keeps the counter, under a lock); anything else is moved to
`rejected/` and logged in `tamper.jsonl`, which `status` and `watch` show,
and the agent never sees it. An older rig's unsigned inbox is treated the
same way. Verification is code in the guest build of rig, never the model's
judgement. The model is told, in its system prompt, where text it reads
during the run cannot reach, that operator messages come only through rig
and that each delivery in this launch carries a random per-launch token;
text elsewhere claiming the operator's authority is data. Without that
statement the model rightly discounted hook context it could not place.

**The brief.** Sent once, as the first message of a new session; a resume
gets only its short continuation note and what is queued. The brief in the
guest is itself signed (`brief.msg`, with a floor so an older one copied back
is refused), the runner will not launch on one that does not verify (exit
77, which the unit does not restart), and `rig brief` prints the verified
text, which the continuation note points at so an agent can re-read it after
a compaction instead of trusting a summary. `rig agent brief` changes it
while the agent runs: it diffs against the host's copy of the last brief
delivered, writes the new one signed, and queues the diff as a
`brief-update`, in order with everything else. Persistent rules live in the
brief; there is no separate "standing message". The start options are
recorded on the instance, so a bare `rig agent start` resumes as started:
one that forgot `--until-done` used to stop the agent after its next turn.

**One codebase, one name.** The guest build is `cmd/rig/guest`, a second main
package built as `rig` (static, amd64 and arm64), not build tags on
`cmd/rig`: tags would have to mark every host file, and every new one, and a
forgotten tag pulls the Incus client into the guest without a sound. A
package boundary makes "the guest cannot reach Incus or the private key" a
property of the import graph, which a test checks. Both builds use
`internal/agent`'s envelope code. rig embeds the guest build
(`internal/agent/guestbin`), and `rig agent start` installs it as `bin/rig`,
first on the agent's PATH, and records the sha256 of it, the public key and
the runner on the instance for `status` to compare. It is a build product,
not committed: `make` builds it first, a checkout without it embeds a
placeholder and `go test` still passes, and a test rebuilds each embedded
build and fails, naming `make`, when it is not what the source builds to now.
The cost is that `go install` alone gives a rig whose `agent start` refuses,
saying why.

What holds, and what does not:

- Signing stops forged items, because the private key never enters the
  guest: the agent, or anything that talks it into writing a file, can put
  anything in `inbox.d`, and none of it verifies.
- The model does not judge signatures; it trusts a path, and is told which.
- Guest root can replace the verifier, the public key or the runner, and
  then deliver anything. The hash check sees a replaced file, but it reads
  through the guest (its incus-agent and its sha256sum), so a guest that
  lies about its own files is not caught. It detects; it does not prevent.
- The token defends against text written before the launch: a repo file, an
  issue, a web page. Code running in the guest can read it (it is in
  `claude`'s argv and in `launch.token`, and everything there is root).
- Replays are refused by sequence number against state the guest keeps,
  which guest root can reset.
- Nothing here touches injected instructions that do not claim the
  operator's authority: "the tests need the cache deleted" in a README is
  still text the agent may follow, as before.
- An item taken at a launch whose `claude` then dies is in `delivered/`, not
  in the conversation, as with the old inbox.
- The hook fires for a subagent's tool calls too, and did deliver to one;
  since 2026-10-30 it delivers only on an input that proves the lead.

For later: inject operator messages on the host, in the credential proxy,
which is the agent's only route to the API. That takes the guest's code out
of the trust base (no verifier, key or runner there to replace) and lets the
proxy strip imitations from requests. It costs rewriting requests in the
plugin, a prompt-cache miss at each injection, a transcript in the guest that
differs from what the model saw, and it works only for proxied VMs.

`claude -p` stops background tasks ten minutes after a turn ends unless
`CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS` says otherwise. The runner leaves it:
a longer ceiling saves a long backgrounded build at a turn boundary, but
holds the turn open, idle, for as long as a forgotten server runs, which
under `--until-done` is up to `--timeout`. The env file is exported to the
agent, so a project whose builds need it sets it there.

## 2026-10-15: the proxy as the guest's way out, opt-in

The policy permitted the credentials' hosts and nothing else, which was
too narrow by one fact: the agent's tools inherit its `HTTPS_PROXY`, so
cargo, git, nix and npm reach their registries through the proxy or not at
all, and under the allowlist not at all. The isolation asks only that the
proxy never dial what the guest's own NIC may not, the private floor; what
a guest may reach beyond that is its manifest's to say. So
`guest.proxy.egress: internet` adds a permit for everything from the
guest's address under the same forbid, and stays opt-in: nothing is granted
by default, and a guest whose proxied traffic is one API has no reason to
carry the rest. Interception is unchanged (the plugin's scope is every
connection), so every tool under the agent must trust the proxy's CA; rig
names the bundle for those that read a variable, cargo now among them, and
`rig doctor` proves from inside the guest that an unnamed host is refused,
or reached, as the manifest says.

## 2026-10-09: a proxy per guest

The multi-tenant shape below lasted a day. Two things argued against it.
Tenant dispatch in witmproxy is by source address with a five-second cache
that nothing invalidated when a mapping went away, and rig hands out the
lowest free address, so a guest made within five seconds of its
predecessor's deletion would have been its predecessor to the proxy and
been given its credentials; fixed in witmproxy, but a process boundary
cannot have that bug. And one shared instance is one plugin set, one CA and
one policy default for the operator's own traffic and the guests' alike: a
plugin uploaded by hand re-enables globally, anything the operator enables
for themself handles guest traffic, and the default has to be permit for one
and forbid for the other.

So each guest has a witmproxy of its own: an *instance* (named,
`rig-<vm>`) in the operator's *user scope* (`--user`, which witmproxy gained
for this: state in the user's home, a `systemctl --user` unit, no root),
made by rig at the first start that proxies credentials with witmproxy's
own installer, stopped with the VM, removed with it. Its process, database,
CA and policy are that guest's; the plugin is enabled for the whole
instance with that guest's rules as its configuration, which needed one
endpoint witmproxy lacked (`PUT /api/plugins/{ns}/{name}/config`). The
ports follow from the guest's address, so nothing about the proxy is
recorded on the instance; the NIC's ACL allows that one port, so a guest
cannot reach another guest's proxy, and the policy says what that proxy
dials: nothing by default, never the isolation ACL's reject list, HTTPS to
the manifest's hosts from the guest's pinned address. Tenants and IP
mappings left rig entirely.

The cost is lifetime: a user unit ends with the session unless the
operator's manager lingers. That turned out not to be root's: systemd 256
lets a user linger themself, so `rig proxy setup` does it (2026-10-13).
The one thing left to root, the ufw rule for the proxies' range, setup runs
under sudo with the rule printed first, as the host verbs do, rather than
only printing it as the published-ports rules are: setup is run once, by
hand, where a password prompt is fine. The alternatives were a system template
unit with a polkit rule (state still root's) or a root supervisor in
witmproxy (orchestration it need not do). On macOS the equivalent, a
LaunchDaemon with `UserName`, is noted in witmproxy and not written; a
LaunchAgent lives with the login session there.

Policy became data in witmproxy rather than a plugin capability. The sketch
had plugins contribute rules under a CEL bound (`dst.port() == 443`) on what
they might permit; a bound evaluated over a *pattern* is ill-defined, and
the thing that knows both the credential and the client is rig, so rig
writes `policy.toml` and the plugin never sees policy. The algebra is
Cedar's, the shape Tailscale's, evaluated in the dialer per resolved
address; witmproxy's `--upstream-deny` is now shorthand for one forbid.

## 2026-10-08: credentials the guest never holds

The README listed it under what does not hold: a guest can send out anything
it has, and the one thing it has worth sending is its API key. Scoping the key
was the whole answer. Now a manifest can say `guest.proxy.credentials:
{ANTHROPIC_API_KEY: [POST api.anthropic.com/v1/messages]}`, and the key stays
on the host: witmproxy runs there with rig's plugin (`plugin/`, `@joinezco/rig`,
embedded in rig as a signed component), which puts the real value into HTTPS
requests to the endpoints named, in place of the stand-in the guest was given.
An endpoint is a host, and optionally the methods and the path below which
the value may go, so a key can be good for one call and nothing else. The
stand-in is random per start (and kept across `rig creds`, so a swapped value
needs no agent restart), works through this proxy alone, and only there;
`rig creds` rotates it with the key.

witmproxy is the operator's service, not rig's child. Its own installer puts
it in a system unit; it has tenants, per-tenant plugin enablement and
configuration, and a management API; rig is a client of that API and a plugin
in it. Each guest is a tenant, known by its address: a second NIC on a bridge
of rig's own, `rigproxy0`, with no NAT, no routing, no DNS, and an ACL on the
NIC that allows one thing, TCP to the proxy's port on the bridge address.
The address is pinned to the NIC by Incus (`ipv4_filtering` drops anything
the guest sends from another) and set in the guest by rig at each start, so
an address is an identity the proxy can trust. Incus would lease it by DHCP
too, and the first attempt relied on that, until the host's firewall turned
out to drop the exchange: a guest talking to the host's own address on a
bridge is ordinary INPUT traffic to the host, and ufw's default policy
drops it. The proxy port needs one ufw rule for the same reason; DHCP did
not need to. The plugin is installed disabled and
enabled per tenant with that tenant's rules, so a client the proxy cannot
name is never intercepted on rig's account. The agent gets `HTTPS_PROXY`
through the env file it already sources, with the proxy's CA named for every
tool that keeps its own trust store; the harness adds the line its runtime
needs (`NODE_EXTRA_CA_CERTS` for Claude Code). Its traffic to the rest of
the world keeps going out of its first NIC, under the isolation ACL,
carrying stand-ins.

A first cut (2026-10-06) read the Incus ACL ordering (rejects before allows,
across every ACL on a NIC) as ruling out any IP path to a proxy on this
host, and went over vsock: socat on the guest's loopback dialing the host,
and a transient user unit of rig's own supervising witmproxy with a
plugin-directory sidecar for its rules. It worked and was verified, and was
replaced two days later: it duplicated what witmproxy already is, tied every
guest's credentials to the operator's login session (a user unit ends with
it unless lingering is on), and could not tell one guest from another. The
ordering fact rules out a hole in the first NIC's ACL, not a second NIC: ACLs
are per NIC, and a NIC of its own has an ACL with no reject in it to shadow
anything. Running witmproxy inside the guest was never an option; the agent
is root there.

witmproxy gained what the shape needed. From the first cut: `--upstream-deny`
(the ACL's reject list, judged on the address about to be dialed, after
resolution, since a forward proxy reaches whatever its host reaches), a 421
for a request inside an intercepted connection that names another host, a
plugin-directory sidecar for configuration as files, and dialing the
kernel's record of the original destination in transparent mode. For this
cut: tenant resolution on the explicit listener, which its own code had left
as "Phase 4", threaded through CONNECT and the MITM into every plugin
dispatch, with the per-tenant effective plugin set and per-tenant
configuration that existed but had no callers; a tenant-create endpoint;
and `witm tenant create` using it rather than the registration placeholder.

On the plugin's language: witmproxy's plugin world is `async func` with
`stream` and `future` types. wit-bindgen-go 0.7.0 with a stock Go emits no
exports for those, and its async support needs a patched Go toolchain, so
the plugin is Rust, which is the toolchain `witm plugin new` scaffolds for.
It lives in rig, with the WIT package vendored, and its signed build is
committed, so building rig needs no Rust.

The agent tooling became generic over a harness at the same time
(`internal/agent.Harness`, implemented by `internal/agent/claudecode`, chosen
by `guest.agent`): the binary, how it is installed and launched, how its
event log is read, which credentials it authenticates with and where they
go, and how it trusts an extra CA. The unit, the inbox, the session id and
the restart policy are shared. Claude Code is the only implementation.

What is weak: the key is usable from the guest at the endpoints it is meant
for, which is its purpose, so an agent can still spend it there; only its
leaving is prevented. Every HTTPS connection a proxied guest sends through
the proxy is terminated by it, not only those to credential hosts, because a
tenant can enable a plugin but not narrow its scope; a client that pins its
server's certificate fails through the proxy. A response that echoes a
credential back would echo the real one; none of the APIs this was written
for do. A credential inside a request body is not rewritten, nor one inside
Basic auth's base64. Only what sources `/run/rig/env` uses the proxy; the
rest of the guest's traffic goes direct, carrying nothing. A VM that stops
without `rig stop` leaves its tenant in the proxy until its next start, and
`rig proxy status` names it. Uploading the plugin enables it globally, so
`rig proxy setup` disables it right after; a `witm plugin add` by hand would
not.

## 2026-09-23: from agent sandbox to grants

rig began as one NVIDIA card and a coding agent, and its defaults said so:
`rig new` claimed the card unless told `--no-gpu`, an instance with no
device record wanted the card, the base image carried the NVIDIA driver and
docker, `rig init` wrote a CUDA project, and host setup put the isolation on
Incus's `default` profile. Running a DAW with no network and a USB controller
showed the model was really "a VM gets only what it is granted". So the
defaults moved to nothing: a VM gets no devices unless a manifest or `--gpu`
asks, the base image is minimal with `nvidia`, `docker` and `desktop` as
modules, `rig init` writes a manifest and a guest flake, and rig's VMs use a
`rig` profile of their own, seeded from `default`'s NIC and root disk, so
installing rig changes nothing about instances it did not create. The CUDA
agent project lives on as `examples/cuda-agent`.

The verbs followed. `rig new -f` created and `rig apply -f` updated, while plain
`rig apply` set up the host: three meanings on two verbs. Now `rig apply -f` is
create-or-update in kubectl's sense, `rig delete -f` removes, `rig new` is the
imperative quick create, and host setup is `rig setup`. Unlike kubectl, apply
never recreates a VM (that wipes its disk) and never starts one (that claims
devices); it reports what needs either. The manifest's `build:` became
`flake:`, a flake reference with nixos-rebuild's `#name`: Compose's `build:` is
a Dockerfile's directory, and a field that looks like Compose's but means
something else is worse than a different name.

## Goal

An isolated VM with real GPU access for an unsupervised coding agent:

    monitored claude code on host  ->  Incus VM (GPU passed through)
                                        ->  unsupervised agent on a project

One VM at a time, one per project, stopped when unused. Remote paid models power
the agent; the GPU is for the software being built.

## Reference host

Everything here was developed and verified on one machine:

- AMD Ryzen 7 7800X3D, 60 GiB RAM, Ubuntu 26.04, kernel 7.0
- RTX 4080 SUPER (Ada) in an IOMMU group of its own plus the bridge above
  it — no ACS override needed
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
  cover `network_acls` — hence `rig setup`.
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
  Every build rewrites the copy first (2026-10-15): a `make` at the same
  commit keeps the cache key, so a project's image was built against the
  base an older binary had left there; the lock pinning the old copy fails
  in nix's words, with rig's hint to remove it.
  The cost is that the template a binary writes is the template it was built
  with, which is the point: it is the one that binary's image build agrees
  with.
- **socat is in the base image.** It was a project's job to ship it, so
  `rig forward` failed on any fresh guest until someone remembered — the
  provisioning-by-memory the image exists to end. It is rig's dependency, and
  rig's image carries it.
- **Host devices are declared data; the allocator is generic.** The GPU
  package enforced one invariant for one card. The invariant was never about
  the card: any PCI function passed through VFIO hot-unplugs silently when a
  second VM names its address, and the host takes each one back the same way
  — unbind, reset, probe, wait for a sign of life. So a device is a record
  (`internal/devices.Decl`: kind, address, return recipe), the allocator
  iterates over records, and what made the card special is four fields of
  the recipe rather than a code path. Adding a USB controller was a manifest
  entry, not a package. See "Host devices are declared, not coded".
- **The manifest is intent; Incus stays the record.** `rig.yaml` reads as a
  Compose file because that is the shape people and models already know, and
  `rig apply -f` records everything it reads on the instance, so later verbs
  need no file. The `host:` block repeats per project, deliberately: one file
  then describes everything a VM needs, and a checkout on another machine can
  see the addresses it was written for. Unknown keys are errors.
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
- **`rig pull` is `push` turned around.** Both take `<vm> <from> [<to>]`, copy
  a directory's contents to exactly the destination, and refuse to overwrite a
  file they did not write. Re-pulling to see an agent's progress is the normal
  workflow, so pull marks each host file with the hash it wrote, in a `user.`
  extended attribute that moves and dies with the file, and refuses only a file
  edited since or one it never wrote that differs. Pull once read a single file
  and printed a directory's JSON listing as if it were content, with exit 0.
- **`rig exec` takes its command after `--`.** Before, everything after the VM
  was the guest's, so rig's flags there reached bash, and words were joined
  with spaces, losing their quoting. Now rig's flags go anywhere before `--`;
  after it, several words arrive exactly as given, as with `docker exec`, and
  one is a shell script. stdin stays opt-in (`-i`), or a script's loop loses
  its input to the guest, as with ssh.
- **`rm` and `delete` read `-f` the way their namesakes do.** `rig rm -f myvm`
  forces, as `docker rm -f` does; `rig delete -f rig.yaml` names a manifest, as
  `kubectl delete -f` does. One verb with both meanings of `-f` read a VM name
  as a missing file. Both ask before deleting, since the disk holds `/work`.
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

`base/flake.nix` exposes `lib.mkGuest` and the optional modules (`nvidia`,
`docker`, `desktop`), so a project can ship a guest flake adding its own. `rig image
build` already took `--flake`/`--attr`/`--alias` and `rig new` already took
`--image`, so no new verbs were needed; the missing piece was only that the base
was not importable. `project-template/guest/` is what `rig init` writes.

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
incidental. `examples/cuda-agent` uses `path:.` and gets away with it only
because that tree never accumulates build output.

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

## Host devices are declared, not coded (2026-09-20)

What changed: `internal/gpu` became `internal/devices`, generic over declared
devices, and `internal/hostgpu` became `internal/hostdev`, a return routine
driven by a recipe. `rig stop` now detaches a VM's devices and returns the
ones with a recipe; `rig start` stops a declared host unit first. `rig.yaml`
carries all of it, and `ports:` publishes guest ports to the LAN.

Verified on the reference host while building it:

- **Which controller the monitor is on.** The kernel log shows the monitor's
  hub, the keyboard, the mouse, an audio interface and a phone all appearing
  on USB bus 3 together and leaving together when the monitor's KVM switched
  input. Bus 3 is `0000:3c:00.3`, a CPU-side xHCI controller alone in IOMMU
  group 27 with nothing else plugged into it, so passing it costs the host
  nothing. Its only reset method is `pm`, not FLR; whether it comes back to
  `xhci_hcd` cleanly after a VM is the one thing not yet proven, and needs
  a sudo prompt to try.
- **NAT-mode proxies refuse a wildcard listen address.** "Cannot listen on
  wildcard address 0.0.0.0 when in nat mode". They also need the instance's
  NIC to carry a static `ipv4.address`, and take a wildcard *connect* address
  as "the guest". So `ports:` publishes on the host's default-route address,
  discovered, or one the manifest names in Compose's `ip:host:guest` form,
  and pins a guest address from the top of the bridge's range.
- **The per-instance ingress ACL is load-bearing.** With `rig-fwd-<name>`
  beside `vm-isolate` on the NIC, a TCP and a UDP listener in the guest
  answered on the host's LAN address. With it removed from the NIC and
  nothing else changed, the same TCP port hung. `rig verify` still reports
  PROVEN with ports published, which is right: every egress reject range is
  still enforced; only named ingress ports are admitted.
- **The host's own firewall is the fourth layer, and a test from the host
  cannot see it.** A Moonlight client on the LAN got "Unable to connect"
  while the same port answered from the host: ufw's forward chain dropped
  the mapped packets (`UFW BLOCK IN=enp6s0 OUT=incusbr0 DST=<guest>`), and
  locally generated traffic never traverses that chain. `ufw route allow`
  scoped to the LAN and the guest's pinned address fixed it. `rig doctor`
  now reads the kernel log for those drops and prints the rules.
- **The guest's own firewall is the third layer.** The published ports
  reached the guest's NIC and went no further until `nixos-fw` admitted them.
  NixOS enables its firewall by default and nothing on the host can open it,
  so the desktop module sets `openFirewall` for Sunshine and the docs say the
  same thing twice on purpose.
- **X11, not Wayland, for the desktop — and X needs to be told the bus
  address.** Inside QEMU the virtual screen Incus gives every VM is the boot
  VGA. A Wayland compositor picks it and renders in software. X is subtler:
  a Device section with no BusID is offered only the boot VGA device, so the
  nvidia driver reported "No devices detected" with a working card one slot
  over. The card's guest address depends on how Incus orders the VM's
  devices (here `07:00.0`, after the USB controller at `06:00.0`), so
  `desktop.nix` finds it in sysfs at boot and starts X with a layout naming
  that device alone. The virtual screen stays, as the
  `incus console --type=vga` fallback that opens no network path.
- **The card drives the monitor from inside the VM.** With the monitor
  switched to another machine's input, the guest kernel still reported the
  card's DP-1 `connected`, and X brought the screen up at 2560x1440 on it
  with XFCE logged in and Sunshine listening. Moonlight's `/serverinfo`
  answered on the host's LAN address through the published port.
- **A development build's guest lock goes stale.** The template's guest
  flake pins `path:~/.cache/rig/base-<build>` by NAR hash in its
  `flake.lock`; rebuilding rig with a changed base rewrites that copy, and
  the next image build fails with "NAR hash mismatch". Deleting the guest's
  `flake.lock` fixes it, and `rig image build` now recognises the failure
  and says so, naming the lock file.

- **The monitor shows the session; the peripherals reach the guest.** With
  the monitor switched to the card's input, the XFCE desktop was on screen
  and the whole hub tree — keyboard, mouse, audio interface, MIDI
  controller — appeared on the guest's bus 3. X ignored all of them: the
  first version of the boot-time card layout replaced X's config directory
  with `-configdir`, which also dropped NixOS's libinput InputClass rules,
  so every device was "No input driver specified". The generated file now
  goes into that directory by symlink and X only gets `-layout`.
- **A desktop session swallows the power button.** `rig stop` asks the
  guest to shut down over ACPI; xfce4-power-manager holds the handle-power-key
  inhibitor and does nothing, so the guest logged "Power key pressed short"
  and stayed up through two timeouts. The desktop module now has logind act
  on the key regardless of inhibitors, and `rig stop` pulls the plug when
  the clean shutdown times out rather than leaving a VM running with the
  host's devices inside it.
- **The host hung hard with the USB controller passed through
  (2026-09-20 18:34).** The journal stops mid-stream on routine lines, no
  panic, no oops, no IOMMU fault, nothing in pstore; inbound SSH and
  Tailscale died together, and the machine needed a reset. The VM had held
  the card and `0000:3c:00.3` for about an hour, the guest was answering
  commands minutes before, and nothing rig-side was acting at the moment.
  Suspect, not proven: the controller. It is one function of the same PCI
  device as the iGPU that drives the host's console, and AMD's integrated
  xHCI controllers have a record of hanging hosts on VFIO resets and
  teardown; VFIO had reset this one twice that day. The desk project now
  passes the keyboard and mouse by identity (`kind: usb`) and leaves the
  controller with the host. The controller kind stays in rig — it is the
  right thing for a chipset controller or a discrete card — with this in
  the runbook as the reason to prefer a controller that shares nothing with
  the host.
- **One USB cable serves both consoles.** The monitor's host-side USB
  cable is the host console's keyboard as well as the VM's. While a VM holds
  the controller, or has the devices redirected by identity, switching the
  monitor to the host's own input shows the host console with no keyboard
  and no mouse. During the hang that read as a second failure; it was the
  design. A host driven over SSH from another machine does not mind; one
  that needs its console does, and should keep a keyboard on a controller
  no VM is given.

Not yet proven: that the keyboard and mouse work in the session with the
input fix (the devices were in the guest; X now has a driver for them), and
that a passed-through controller returns to `xhci_hcd` without a reboot on
`rig stop` — moot for the desk project now, and not worth another hang to
find out.

## PCI addresses are not names (2026-09-22)

Disabling the Wi-Fi card in the BIOS removed a function from the chipset's
PCIe switch, and the buses behind it were renumbered: the chipset USB
controller went from `12:00.0` to `11:00.0`, and the SATA controller took
`12:00.0`. A manifest naming `0000:12:00.0` then passed the host's SATA
controller to a VM running untrusted software. Every check rig had passed:
the address was exclusive, alone in its IOMMU group, isolated. None of them
asked what the device was. It came to nothing only because no disk hangs off
that controller on this host.

The fix is identity, not vigilance: `gpu` and `pci` devices declare
`id: vendor:device`, recorded on the instance, and `devices.CheckIdentity`
compares it with sysfs before any claim, refusing with the address the declared
device has moved to. `doctor` runs the same check on a stopped VM. A card with
no id recorded (instances from before this) must at least be an NVIDIA display
controller; a `pci` device with none is refused outright, since there is
nothing else to check it against. Vendor:device does not tell two identical
controllers apart; on this host nothing that rig passes has a twin.

## Known Incus behaviours (verified here)

Behaviours the code now handles are documented at their handling site, not here.
What remains:

1. **A passed-through device is not released on VM stop.** Incus sets
   `driver_override=vfio-pci` and never clears it. `rig stop` returns devices
   that have a recipe; `rig host desktop` does it for the card by hand.
   Not universal: a `pci`-type xHCI (`12:00.0`) came back on `xhci_hcd` by
   itself when detached from a stopped instance (2026-09-22), with
   `driver_override` set to `xhci_hcd`. `rig stop` now checks for a bound host
   driver and the recipe's sign of life before asking for root.
   Read against the source (6.0.5, 2026-09-23): at every stop Incus rebinds
   a device to the driver it had when the VM started, so the card stayed on
   vfio-pci because it was parked there then. Taken from a host driver, it
   goes back unreset, the dead adapter of 6. below. So `rig host return`
   redoes a device that is bound without its sign of life, and `rig start`
   leaves a watcher, `rig-handback-<vm>`, for a stop `rig stop` did not make.
   That redo comes too late if the rebind itself does the damage: the first
   stop after a start that took the card from nvidia, with the host's desktop
   on it, froze the host hard before `rig stop` could act (2026-09-28). No log
   survived, so the rebind is the likeliest cause rather than a proven one.
   Since then `rig start` parks a device whose recipe resets on vfio-pci after
   the claim has checked its identity, so the driver Incus restores is
   vfio-pci and the reset always happens.
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
   nftables chain it never created and fails. `rig setup` detaches, rewrites
   and reattaches, and refuses while a consumer is running. See
   `policy.rewriteACL`.

7. **A NAT-mode proxy on a VM needs one concrete listen address and a static
   guest address.** Wildcard listen is refused outright; a wildcard connect is
   filled in with the NIC's `ipv4.address`. See `internal/ports`.

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
- `rig image build` builds and imports the guest image, stamping it with the
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
the NIC. Declared in `internal/policy`, reconciled by `rig setup`.

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
image is stamped and `rig doctor` reports drift, and `rig verify` fails when a
target in a declared reject range stops yielding a proof rather than skipping
quietly. (A range with nothing of the host's in it is a different case: it is
reported as moot, because there is nothing there for the guest to reach, and
treating it as a gap had made verify inconclusive on every host without
Tailscale and docker.)

1. **VMs are not garbage-collected.** Stopped project VMs accumulate at ~6 GiB
   each on a 500 GiB loop file, and a full root means write errors on a ZFS
   vdev. `rig image build` now deletes the image it replaced, but instances are
   still nobody's job. `rig status` should show age and size, and there should
   be a way to list what is stale.
2. **`/work` lives inside the instance.** `rig rm` destroys the project with the
   VM, so the VM is the only copy until someone pushes a git remote or pulls it
   out. `rig rm` now says so and asks first; a separate volume for `/work`
   that outlives the instance is still the fix.
3. **One card, one project is enforced but not scheduled.** With several
   projects, "who had it last, and does something want it now" becomes guesswork
   at the point where it is most annoying to add.
4. **The ACL is a denylist.** Every new private range someone invents is a gap
   until noticed. The IPv6 plan already says default-deny plus an allowlist; the
   same argument applies to IPv4 once there is any appetite for the churn.
5. **It is NVIDIA-only, and one card.** The CLI is parameterised (`--flake`,
   `--attr`, `--alias`, `--profile`, `RIG_*`) and nothing names a particular
   host, but for a GPU the vendor is fixed: discovery greps PCI vendor `10de`,
   the only driver module is `base/nvidia.nix`, and the default return recipe
   names the NVIDIA modules. The host side is now
   data — a manifest can name another driver's modules — but another vendor is
   still a second driver in the image, not a flag. Two cards would need the
   manifest's `gpu` kind to stop discovering and start being told.
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
