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

- At every stop, however it comes, Incus rebinds a device to the driver it had
  when the VM started and leaves `driver_override` naming it (6.0.5's
  `postStop`). One that was parked on vfio-pci stays there; one taken from a
  host driver goes back to it unreset, before `rig stop` can act. A
  `pci`-type xHCI came back whole on `xhci_hcd` this way. A card taken from
  nvidia is the suspect in a hard host freeze (2026-09-28): the first stop
  after such a start froze the host before `rig stop` printed anything, and no
  log survived. So `rig start` parks a device whose recipe resets on vfio-pci
  first (`hostdev.Park`), and the return always resets it; `hostdev.Back`
  still spares sudo for one whose recipe does not.
- Returning a device **resets it** before loading the driver, then checks the
  recipe's sign of life (`alive:`) rather than trusting that the driver bound.
  Without the reset, an NVIDIA card comes back with no display; without the
  check, rig reports success anyway. A device on its host driver without the
  sign of life gets the whole return too.
- Incus reports a VM Stopped while it restarts it, whether the guest rebooted
  or `incus restart` did it. Only its lifecycle events tell a stop from a
  restart, and `incus restart` sends `instance-stopped` too, with an operation
  running on the instance until it is back. The handback watcher goes by both.
- `/run/lock` is sticky, and `fs.protected_regular` refuses an `O_CREAT` open
  of another user's file there even to root. The rig lock is opened without
  it, so the root handback watcher and the user share one.
- Under GNOME, `gnome-shell` renders on the discrete card even with the monitor
  on the iGPU, so freeing the card means ending the session, not just the
  display manager. `rig host headless` lists what holds the card first.
- Incus refuses to start a VM given an NVIDIA card while any host process has
  `/dev/nvidiaN` mapped, because unbinding a held card blocks. It reads every
  `/proc/<pid>/maps` as root and names PIDs only, in the error text, which
  `incus.DeviceInUse` parses. GDM's login screen (`gnome-shell --mode=gdm`) is
  such a process with nobody logged in (2026-09-23).
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
- ufw's forward policy drops a guest's egress while its stock rules still pass
  ICMP echo, so a guest that pings the internet but times out on every TCP and
  UDP port is being dropped by the host, not the ACL. The `ufw route allow`
  rule names the uplink interface, and a PCI renumbering renames that too.
- Don't run Tailscale inside a guest: it would be a tailnet node that can
  reach every other one, past the ACL. Publish on the host's tailnet address.
  A guest that must reach one tailnet node gets a vsock relay instead: a
  user unit on the host (`socat VSOCK-LISTEN:<port>,fork TCP:<tailnet
  addr>:22`) and a guest unit (`socat TCP-LISTEN:<port>,bind=127.0.0.1,fork
  VSOCK-CONNECT:2:<port>`), the pattern the CI pools use for host services.
  The connection leaves with the host's tailnet identity, so the tailnet
  policy for the host is what the guest gets, to that one address and port;
  any VM on the host can dial the vsock port.
- `tailscale status` (text) marks some offline peers only with `-` in the
  last column; `--json` has an `Online` field. `verify` picks its tailnet
  peer from the JSON and pings it first, else the check reads "the host
  cannot reach it either" on every run and verify is never conclusive.
- Incus evaluates an ACL's `drop` rules, then `reject`, then `allow`, across
  every ACL on the NIC, so an allow for one address inside a rejected range
  (a proxy port on the bridge address) is shadowed by the reject. A hole in
  the isolation for one port cannot be an ACL rule on that NIC; it is vsock
  (`rig forward`) or a second NIC with an ACL of its own (`rig proxy`).
- ACLs are per NIC, so a NIC under `rig-proxy` (one allow, default reject
  both ways) is isolated by its own rules and the first NIC's are untouched.
  `security.ipv4_filtering` on the NIC drops anything the guest sends from
  another address, which is what lets the proxy take an address as an
  identity.
- Anything a guest sends to the host's own address on a bridge is INPUT on
  the host, where ufw's default policy drops it: DHCP to the bridge's
  dnsmasq and TCP to the proxy alike (`UFW BLOCK ... IN=rigproxy0` in the
  kernel log). `rig proxy setup` adds the one rule for the proxies' range
  under sudo, printed first; ufw's status is root's to read, so rig cannot
  tell whether the rule is there and relies on ufw skipping one it has.
  DHCP was not worth a rule, so rig sets the proxy NIC's address itself at
  each start, from the address the host pinned to the NIC.
- A forward proxy fronting a guest is a hole in the guest's isolation unless
  the proxy declines to dial what the guest may not: witmproxy's policy
  (`policy.toml` in the instance's root, written by rig) forbids the ACL's
  reject list to anyone and permits the guest, by its pinned address, HTTPS
  to the manifest's hosts only; judged on the address after resolution, so a
  name pointed at a LAN address is refused at the dial, and a CONNECT the
  policy forbids gets a 403 before any tunnel exists. `guest.proxy.egress:
  internet` adds a permit for everything from the guest, under the same
  forbid: the agent's tools inherit its `HTTPS_PROXY`, so cargo, git, nix
  and npm reach their registries through the proxy or not at all.
- Each guest's proxy is a witmproxy instance in the operator's user scope:
  `witm service install --user --instance rig-<vm>`, state under
  `~/.local/state/witmproxy/instances/rig-<vm>` (`$XDG_STATE_HOME`), unit
  `ez-witmproxy@rig-<vm>.service` in the user manager. Its ports follow from
  the guest's address (`10.207.0.<n>` → proxy `10.207.0.1:<3100+n>`, API
  `127.0.0.1:<3400+n>`), so nothing is recorded. rig's account's password
  goes in at install through the environment and is read back from the
  instance's `config.toml` (0600); the CA from `certs/ca.crt`. A user unit
  dies with the session unless the manager lingers; `rig proxy setup`
  enables that, which needs no root on systemd 256 and later (polkit's
  `set-self-linger` lets a user linger themself; `pkcheck --action-id
  org.freedesktop.login1.set-self-linger --process $$` says so) and asks an
  admin on older ones, where rig prints the sudo form. witmproxy reads
  `policy.toml` at start only, so rig restarts the instance when the policy
  changes.
- A plugin upload enables it for the instance, which is wanted here: the
  instance serves one guest. witmproxy's per-instance plugin configuration
  is `PUT /api/plugins/{ns}/{name}/config`; rig sets the rules there.
- The proxy bridge exists only once Incus is up. witmproxy's unit binds the
  bridge address at start and exits if it is not there, so it needs
  `After=incus.service` (docs/RUNBOOK.md) or it hits its start limit at boot.
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
- A prompt that begins with a dash is read by `claude` (2.1.308) as an
  option: `claude -p "--- NOTE: ..."` exits 1 with "unknown option" before
  anything runs. Every resume note starts with `---`, so each restart failed
  the same way until StartLimitBurst gave up and the agent sat idle for hours
  (2026-10-27, mono-agent). The runner passes the prompt after `--`.
- `claude -p --resume` takes `-p` as a new user turn. The runner used to pass
  the whole brief at every launch, and one 9.4 MB transcript held it 226
  times; it is now sent only when the session is created.
- Claude Code (2.1.296 in the guest) takes `--settings <file-or-json>` and
  `--append-system-prompt`. A PostToolUse hook set through `--settings` that
  prints `{"hookSpecificOutput":{"hookEventName":"PostToolUse",
  "additionalContext":"..."}}` puts that text before the model within the
  same `claude -p`, at the first tool completion after it arrives. Rechecked
  on 2.1.301 with rig's own settings (`"matcher":"*"`) and guest build.
- Hook `additionalContext` does not appear in the stream-json events, so a
  delivery is invisible in `events.jsonl`; rig logs each one to
  `deliveries.jsonl`.
- The hooks in `--settings` fire for a subagent's tool calls too, in the
  subagent's context: a message delivered there is read by the subagent,
  which says it will pass it on and does not (mono-agent, twice, 2026-10-28).
  On 2.1.314, checked with a PostToolUse hook that copied its stdin, the
  subagent's input carries `agent_id` and `agent_type`
  (`"agent_id":"aacedf6b44a9b07a6","agent_type":"general-purpose"`) and the
  lead's carries neither; `session_id` and `transcript_path` are the lead's
  in both, so neither tells them apart. The subagent's own transcript is
  `<session>/subagents/agent-<id>.jsonl`, named only by SubagentStop's
  `agent_transcript_path`. The Bash environment is identical in both
  (`CLAUDE_CODE_SESSION_ID`, `CLAUDE_PID` and the rest are the lead's), so a
  guest verb cannot tell who ran it; only the hook's input can. `rig hook
  post-tool-use` delivers only on an input that names the agent's session
  and no agent; anything else leaves the queue alone, and an input it cannot
  place is said once per launch in `stderr.log`.
- Claude Code's permission rules (2.1.314 docs): deny, then ask, then
  allow, first match wins, and an ask rule prompts even when a narrower
  allow matches, so `Bash(rig rm *)` in ask over `Bash(rig *)` in allow
  prompts as intended. A `*` may stand in the middle (`Bash(rig forward *
  --to-guest)`); a rule is matched per subcommand of a compound command,
  and ask rules apply inside subshells too. In `-p` mode a tool call that
  would prompt is denied, and `--permission-mode dontAsk` makes that
  explicit. The allow list is not a security boundary: `/usr/bin/rig` or
  `sh -c 'rig …'` matches no rule; it is friction removed, not trust.
- stream-json carries the account's rate limits: a `rate_limit_event` with
  `rate_limit_info.unifiedWindows.{five_hour,seven_day}.{utilization,resetsAt}`
  (seen once per run, at the first response), and each `result` event a
  `usage` for that run (`input_tokens`, `output_tokens`,
  `cache_creation_input_tokens`, `cache_read_input_tokens`) with
  `total_cost_usd` and a per-model `modelUsage` that is cumulative over the
  process. Each `assistant` event carries its API response's `usage`,
  repeated once per content block under the same `message.id`; a subagent's
  carry `parent_tool_use_id`. The OAuth usage endpoint
  (`GET https://api.anthropic.com/api/oauth/usage`, `Authorization: Bearer`
  the access token, `anthropic-beta: oauth-2025-04-20`) returns the same
  windows for the credential, with `utilization` in percent. Not for every credential: a token
  from `claude setup-token` is refused with 403 "OAuth token does not meet
  scope requirement user:profile" (checked 2026-10-30 against the
  mono-agent env), so only a full login's access token
  (`~/.claude/.credentials.json`, or a CLAUDE_CREDENTIALS_B64 snapshot of
  it) reads the quota. `rig agent usage` falls back to this host's own
  login and says so: it is the operator's account, not necessarily the
  agent's.
- Without a statement of where operator messages come from, the model
  discounts hook-delivered text ("can't verify it's from an operator"). With
  an `--append-system-prompt` line saying rig delivers them this way, it acts
  on a mid-turn instruction.
- The agent's Bash tool stops a command at ten minutes, which caps `rig ask
  --wait`; `claude -p` stops background tasks ten minutes after a turn ends,
  and its stderr names `CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS`.
- The agent unit caps its memory and sets `OOMPolicy=continue`. Without the
  cap the guest hits a global OOM and the kernel picks a victim; without the
  policy systemd stops the whole unit when a child is killed.
- The runner wraps `claude` in `timeout(1)`, and the watchdog ends a run by
  signalling timeout's pid: coreutils timeout runs the command in a process
  group of its own and, on a signal, passes it to the whole group, so a
  `bash` stand-in waiting on a `sleep` dies too (bash defers a SIGTERM
  until its foreground child exits, so a signal to bash alone would wait
  out the sleep). SIGKILL follows after thirty seconds regardless. A run
  the ceiling cut exits 124; one the watchdog cut exits 143.
- `RestartPreventExitStatus=` takes a space-separated list in a
  `systemd-run --property`; the runner's 77 (unverified) and 79 (budget)
  share it.
- Refreshing an OAuth session on the host rotates the refresh token, which
  invalidates a `CLAUDE_CREDENTIALS_B64` snapshot in a running guest. `rig
  creds` replaces it without a restart.
- A proxied credential is a stand-in in the guest's env file plus
  `HTTPS_PROXY` and the CA variables, so only what sources `/run/rig/env`
  uses the proxy. Node reads `NODE_EXTRA_CA_CERTS`, not the system store
  (the harness supplies that line); python-requests reads
  `REQUESTS_CA_BUNDLE`; curl, git, nix and cargo their own. The bundle at
  `/run/rig-ca/bundle.crt` is the image's plus the proxy's CA. `rig creds`
  reads the guest's stand-ins back and keeps them, so a swapped value
  reaches a running agent through the proxy with no restart.
- The proxy NIC is `proxy0` in the guest by a udev rule on rig's MAC prefix
  (`02:52:49:47:*`), and dhcpcd leaves it alone (`denyInterfaces` in
  `base.nix`); left to DHCP it would take a second default route to a bridge
  that routes nothing, or fall back to a link-local address when the host
  firewall drops the exchange.
- witmproxy's system scope keeps its state under `/var/lib/witmproxy`, 0700
  root, which an unprivileged rig cannot read; rig uses the user scope
  (`--user`), whose state is the operator's own, and reads the instance's
  `config.toml` (the admin password) and `certs/ca.crt` from there directly.
- Uploading a plugin to witmproxy enables it for the instance, and
  re-uploading re-enables it. That is wanted now: each instance serves one
  guest. In a shared instance it would not be, and the plugin would need
  disabling after every upload.
- Go for the plugin: witmproxy's plugin world is `async func` with
  `stream`/`future` types. wit-bindgen-go 0.7.0 emits no exports for those
  with a stock Go, and its async support needs a patched Go toolchain, so the
  plugin is Rust (`plugin/`, the toolchain `witm plugin new` targets) until
  that lands in a release.

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
