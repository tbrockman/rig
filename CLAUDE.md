# Working in this repo

One GPU, one VM at a time. The VM is where an unattended agent works; the
isolation around it is the product. Two failures are silent, and shape all the
tooling:

- Starting a second VM with the same GPU **hot-unplugs the card from the running
  one**. `incus list` still shows the victim RUNNING with a healthy IP.
- A VM created without the network ACL can reach **this host's sshd** on every
  address the host holds, plus the LAN and the tailnet.

## Which tool for what

`./rig` does everything: the base image, VM lifecycle, working inside a guest,
and the card and policy verbs. `rig --help` groups them; the third group —
`release`, `apply`, `start --allow-unisolated` — is the one that can break an
invariant, so reach for it deliberately.

`./hostgpu` is separate because it is the only thing that touches the host
itself: it needs root, rebinds the card between `vfio-pci` and `nvidia`, and
starts or stops the desktop. Reclaiming the card **resets it** before the driver
loads, and then checks a DRM node appeared rather than trusting that the driver
bound — see STATUS.md, behaviour 6. Both matter: without the reset the desktop
comes back with no display, and without the check `hostgpu` says it succeeded.

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

`rig agent` runs an unattended agent in the guest and keeps a channel to it.
It is a systemd unit, so it outlives your shell; its session UUID is fixed and
stored on the instance, so a crash **resumes the conversation** rather than
restarting the brief and redoing finished work. `rig agent send` queues a
message as a file — delivered at the next restart, and at the next turn if the
brief tells the agent to poll it. A file, not a pipe: it survives a crash and
neither process can hang waiting for the other. `rig agent status` and
`rig agent log` are bounded reads, so checking often is cheap; `log` prints only
what the agent said, not the megabytes of tool calls around it.

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

`doctor` and `verify` answer different questions. `doctor` reads configuration:
is this VM set up right? `verify` sends real packets from inside the guest: is
that setup actually true? Configuration has been right here while the effect was
absent, so the second is not implied by the first.

Build with `make`. Go 1.26, two binaries from one module.

## Starting a new project

```bash
printf 'ANTHROPIC_API_KEY=sk-ant-...\n' > secrets/myproj.env
chmod 600 secrets/myproj.env

./rig new myproj --env secrets/myproj.env --start
./rig push myproj project-template            # -> /work/project-template
./rig doctor myproj                           # isolation, GPU, agent, address, creds
```

Then prove the two things `doctor` cannot: that a kernel returns correct results,
and that the isolation holds against real traffic.

```bash
./rig exec --dir /work/project-template myproj nix develop "path:." -c make run
./rig verify myproj    # 0 proven, 1 violated, 2 could not be proven
./rig exec --dir /work/project-template myproj nix develop "path:." -c ./run-agent
```

`verify` exit 2 is not a pass. It means some check could not tell a blocked
guest from a broken probe, so it says nothing either way.

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

## Notes

`.claude/settings.json` allowlists `./rig` so routine work does not prompt. It is
convenience, not containment — the boundary is the VM and the network ACL.

`STATUS.md` has current state, decisions and why, and open work. `RUNBOOK.md` is
host setup. `project-template/README.md` covers the CUDA toolchain.
