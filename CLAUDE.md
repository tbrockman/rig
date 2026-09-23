# Working in this repo

rig runs isolated NixOS VMs on Incus and grants each one only the host
resources it declares: devices, ports, volumes, pinned CPUs, input. `README.md`
is the user-facing overview, `docs/RUNBOOK.md` sets a host up,
`docs/DEV_NOTES.md` holds the facts and failure modes the code depends on, and
`docs/DESIGN.md` the decisions and their history. `rig <verb> --help` is the
reference for each verb; keep it accurate rather than repeating it here.

## Build and test

```bash
make                   # ./rig
make test              # go vet (including the integration tag) and unit tests
make test-integration  # real Incus and a real card; destructive, refuses if anything is up
```

Go 1.26, one module. Changes under `base/` reach a VM only through `rig image
build` and recreating the VM (`rig delete -f`, then `rig apply -f`; volumes
survive). Nix sees only files git knows about, so `git add` a new file under
`base/` before building or evaluating it.

## Rules

- **Nothing is granted by default.** A VM gets a device, a port, a volume or
  input only because a manifest or a flag asked. Keep new features opt-in.
- **Check before claiming, hand back on stop.** A grant that can be wrong
  (a renumbered PCI bus, a device held elsewhere, a missing ACL) is refused
  before anything moves, and the message names the fix.
- **Incus is the record.** Everything a manifest says is recorded on the
  instance, so later verbs work from Incus alone. Unknown manifest keys are
  errors.
- **Raw `incus` is a gap in rig.** If you reach for it while operating a VM,
  say so rather than working around it.
- **Configured is not proven.** `doctor` reads configuration; `verify` sends
  packets from inside the guest. Exit 2 from `verify` is not a pass.
- **Examples are fictional.** No real host's PCI addresses, device ids, IPs or
  paths in docs, tests or templates: GPU `0000:2b:00.0` / `10de:abcd`, tailnet
  `100.64.0.7`, `/home/me`. Credentials live outside any repository
  (`~/.config/rig/<vm>.env`).
- **Docs stay short.** A sentence of why beats a paragraph of history; history
  goes in DESIGN.md, hard-won facts in DEV_NOTES.md.

## Layout

| Path | |
|---|---|
| `cmd/rig` | the CLI |
| `internal/manifest` | `rig.yaml`: parsing and validation |
| `internal/devices` | who holds which device: the record, the claim, the lock, identity checks |
| `internal/hostdev` | handing a device back to the host: unbind, reset, probe |
| `internal/policy` | the isolation ACL and the `rig` profile |
| `internal/ports`, `volumes`, `cpuset`, `input` | the other grants |
| `internal/verify` | probing a live guest |
| `internal/agent`, `creds`, `pushguard` | the agent unit, credential injection, push's overwrite guard |
| `internal/incus`, `vsock` | the Incus REST client; AF_VSOCK |
| `base/` | the guest image flake: `base.nix`, and opt-in `nvidia`, `docker`, `desktop` modules |
| `project-template/` | what `rig init` writes: a `rig.yaml` and a guest flake |
| `examples/cuda-agent/` | a GPU VM for an unattended agent |
| `integration/` | build-tagged tests against real Incus |
