# Project template

`rig init <dir>` writes this into a new project, from the copy embedded in the
binary; in a checkout, copying the directory does the same. It gives you a CUDA
toolchain and the agent, both pinned by the project rather than the base image,
plus one test worth keeping.

```bash
./rig init proj
./rig new myproj --env secrets/myproj.env --start
./rig push myproj proj
./rig exec --dir /work/proj myproj nix develop "path:." -c make run
```

## Why the toolchain is here

The base image carries the NVIDIA **driver**, which has to match the kernel.
Everything above it — nvcc, cudart, the agent — is a project decision, and two
projects should be able to disagree without either needing a new base image.

This works because CUDA drivers are backward compatible with older runtimes. In
use here: a **12.9 toolkit on a 13.2 driver**.

`libcuda.so.1` ships with the driver, not the toolkit, so it is in none of the
packages this flake installs. The `shellHook` puts `/run/opengl-driver/lib` on
`LD_LIBRARY_PATH`; without it you get a binary that compiles and links cleanly
and then dies with `cudaErrorInsufficientDriver`, which reads like a driver
problem and is not.

## `vectoradd.cu`

A vector add written as a test. The tutorial version passes even when the kernel
never runs, because a zeroed output buffer satisfies `0 + 0 == 0`. This one:

- **poisons** the device buffer and verifies the poison *before* launching, so
  "never ran" cannot pass, with a different pattern in the host buffer so a no-op
  D2H is distinguishable from a no-op kernel;
- computes expectations in **integer arithmetic from the generator**, never from
  the float arrays the GPU was handed, so a corrupted H2D fails too. Inputs are
  multiples of 1/8 below 2^18, so every sum is exactly representable and correct
  IEEE-754 addition is required to match;
- compares **all 4,194,304 elements** bitwise, no epsilon;
- ends with a **negative control** — one element corrupted by 1 ULP, which any
  tolerance-based check would wave through — so a vacuously-passing test fails;
- runs **two launch geometries**: one thread per element with a tail guard, then
  131072 threads so the grid-stride loop has to iterate.

Exit codes: `0` pass, `1` a check failed, `2` a CUDA call failed (including "no
CUDA device", which is what you get when the GPU is not attached).

It also prints H2D/D2H bandwidth — not a benchmark, but worth a glance: a card
in a chipset **x2** slot reports ~3 GB/s, and profiling conclusions drawn there
do not hold on an x16 machine. Passthrough itself costs nothing measurable.

## `run-agent`

Sources `/run/rig/env` — the credentials `rig start` injected — and execs the
agent with `--dangerously-skip-permissions`. That flag is the point of this VM,
not a shortcut around it: the agent is unattended, so there is nobody to answer a
prompt, and the containment is the VM boundary and the network ACL. Verify both
before trusting it: `./rig doctor <vm>` and `./rig verify <vm>`.

## Sizing

The CUDA toolchain plus the agent costs a few GiB of `/nix/store` on top of the
base system. `rig new` defaults to a 40 GiB root; Incus's own default is 10 GiB,
which fits but leaves no room to work.
