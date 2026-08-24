# Working in this repo

This host runs **one** GPU VM at a time. The VM is where an unattended agent
does its work; the isolation around it is the product, not a detail. Two things
can go wrong silently, and both are why the tooling looks the way it does:

- Starting a second VM with the same GPU **hot-unplugs the card from the running
  one**. `incus list` still shows the victim RUNNING with a healthy IP.
- A VM created without the network ACL can reach **this host's sshd** on every
  address the host holds, plus the LAN and the tailnet. Nothing warns you.

## Which tool for what

| Task | Use |
|---|---|
| Create, start, stop, delete a project VM | `./rig` |
| Work inside a guest | `incus exec`, `incus file` |
| Reconcile the ACL, release/force, start unisolated | `./gpuctl` |
| Change instance devices, profiles, or networks | **don't** — see below |

`./rig` is the surface for everything routine. `./gpuctl` sits under it and adds
the operations that can break an invariant (`release`, `apply`, `--force`,
`start --allow-unisolated`) — reach for it deliberately, not by habit.

Raw `incus init/start/stop/delete/copy`, `incus config device`, `incus profile`,
and `incus network` are denied in `.claude/settings.json`. Not because they are
forbidden knowledge — because every one of them has a `rig` or `gpuctl` verb
that does the same thing without dropping an invariant on the floor. If you find
yourself needing one, that is a gap in `rig`; say so rather than working around
it.

**That deny list is a guardrail, not a sandbox.** It is prefix-matched on the
command string, so `bash -c 'incus start x'` sidesteps it, and the test scripts
call `incus` internally without tripping anything. It makes the safe path the
easy path. The real boundary is the VM and the ACL.

## Starting a new project

```bash
# 1. credentials — scoping them is your call, rig only refuses a file
#    other users can read
printf 'ANTHROPIC_API_KEY=sk-ant-...\n' > secrets/myproj.env
chmod 600 secrets/myproj.env

# 2. the VM. Isolation is inherited from the default profile; rig checks that it
#    actually landed and warns if not.
./rig new myproj --env secrets/myproj.env

# 3. the project tree, including the CUDA toolchain and the agent launcher.
#    `push -r` names the guest directory after the SOURCE directory, so name
#    the host directory after the project and push it into /work.
mkdir -p /srv/projects/myproj && cp -r project-template/. /srv/projects/myproj/
./rig start myproj
incus file push -r /srv/projects/myproj myproj/work/     # -> /work/myproj/

# 4. prove the VM is what you think it is, before handing it to an agent
incus exec myproj -- gpu-check
./test-network-acl.sh myproj
incus exec myproj -- bash -lc 'cd /work/myproj && nix develop "path:." -c make run'

# 5. hand it over
incus exec myproj -- bash -lc 'cd /work/myproj && nix develop "path:." -c ./run-agent'
```

Step 4 is not ceremony. `gpu-check` proves the card is attached; `make run`
proves a kernel computes correct results; `test-network-acl.sh` proves the guest
cannot reach the host. Skipping them means an agent may spend hours drawing
conclusions from a machine that is quietly broken.

`rig rm myproj` deletes it — stopped only, and only VMs `rig` created.

## Credentials

`rig start` pushes the `--env` file to `/run/rig/env` (mode 0600, root) once the
guest agent is up. `/run` is tmpfs: the credentials never touch the instance's
disk or its Incus config, and they vanish when the VM stops. `run-agent` sources
that file.

To add or change credentials on an existing VM:
`incus config set <vm> user.rig.env=/abs/path/to.env`, then restart it.

`secrets/` is gitignored. Keep keys out of instance config, image builds, and
anything committed.

## Things that will bite you

- **`incus exec <vm> -- bash -c` gets a stub PATH.** Use `bash -lc`, or NixOS
  binaries are simply not found. This once made every network probe fail, which
  the test script read as "blocked" — false passes on the tests that mattered.
- **`incus file push -r` ignores a trailing `/.`** and always creates a
  directory named after the source. `push -r src/. vm/work/foo/` lands in
  `/work/foo/src/`, not `/work/foo/`. Name the host directory after the project
  and push it into `/work/`.
- **DHCP is not up when the guest agent is.** Poll for an address, not for the
  agent, before assuming the network is broken.
- **An ACL's rules cannot be edited while it is attached to anything.** Create
  `vm-isolate-v2` and repoint; do not edit in place.
- **Never put a GPU device in a profile.** Every instance would then be
  configured to grab the same card. `gpuctl` refuses to run if it finds one.
- **Fixed sleeps are wrong.** Poll with `timeout -s KILL`, and pass `</dev/null`
  so `incus exec` does not allocate a TTY.

## Where things are

`STATUS.md` — current state, decisions and why, verified Incus behaviours, open
work. Read it first. `RUNBOOK.md` — the ordered host setup procedure.
`project-template/README.md` — the CUDA toolchain and what `vectoradd.cu` proves.
