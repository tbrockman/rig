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
starts or stops the desktop.

`rig` covers the whole lifecycle, so raw `incus` should not be needed. If you
reach for it, that is a gap in `rig` — say so rather than working around it.

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
