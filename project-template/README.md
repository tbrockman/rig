# Project template

Copy this directory into a new project. It gives you a CUDA toolchain pinned by
the project (not by the base image) and one test worth keeping: a GPU
correctness check that fails when the GPU is broken, missing, or lying.

```bash
incus copy nixos-gpu-base proj-foo --vm -d root,size=40GiB
./gpuctl start proj-foo
incus file push -r project-template/. proj-foo/work/foo/
incus exec proj-foo -- bash -lc 'cd /work/foo && nix develop "path:." -c make run'
```

## Why the toolchain is here and not in the image

The base image carries the NVIDIA **driver** — that has to match the kernel, so
it belongs to the image. Everything above it (nvcc, cudart, cuDNN, whatever) is
a project decision. Two projects can want different CUDA versions; neither
should need a new base image to get one.

This works because CUDA drivers are backward compatible with older runtimes. The
combination in use here is a **12.9 toolkit on a 13.2 driver**, verified working.

## The one thing that always bites

`libcuda.so.1` ships with the **driver**, not the toolkit, so it is in none of
the packages this flake installs. On NixOS it lives in `/run/opengl-driver/lib`,
and the `shellHook` puts that on `LD_LIBRARY_PATH`. Without it you get a binary
that compiles and links cleanly and then dies at runtime with
`cudaErrorInsufficientDriver` — which reads like a driver problem and is not.

## `vectoradd.cu`

A vector add that is actually a test. Deliberately not the tutorial version,
which passes even when the kernel never runs.

- **Poisoned buffers.** The device output buffer is filled with `0xDEADBEEF` and
  the poison is read back and verified *before* the launch, so "the kernel never
  ran" cannot pass. The host receive buffer is poisoned with a different pattern
  (`0xBAADF00D`), so "the D2H copy did nothing" is distinguishable from it.
- **Expectations computed independently.** Inputs are integer multiples of 1/8
  below 2^18, so every sum is exactly representable in binary32 and the expected
  value is computed in *integer* arithmetic from the generator — never from the
  float arrays the GPU was handed. A corrupted H2D transfer therefore fails too.
- **Bit-exact, all 4,194,304 elements.** Compared on the raw `uint32`, no
  epsilon. There is no tolerance to hide behind, because a correct IEEE-754 add
  has no freedom here.
- **Negative control.** At the end it corrupts one element by a single mantissa
  bit — 1 ULP, which any tolerance-based check would wave through — and requires
  the comparator to report exactly one mismatch. A vacuously-passing test fails.
- **Two launch geometries.** One thread per element (with a tail guard, since
  4194304 is not a multiple of 384), then 131072 threads for 4.2M elements so
  the grid-stride loop has to iterate 32 times. Both checked in full.

Exit codes: `0` pass, `1` a check failed, `2` a CUDA call failed (including
"no CUDA device", which is what you get when the GPU is not attached).

It also prints H2D/D2H bandwidth. That is not a benchmark — it is there because
this host has the card in a **chipset x2 slot** (~3.2 GB/s), and any profiling
conclusion about transfer costs drawn here will not hold on an x16 machine.

Expected output on a healthy VM:

```
device      : NVIDIA GeForce RTX 4080 SUPER (compute 8.9, 80 SMs, 15946 MiB)
driver/rt   : 13.2 / 12.9
H2D         : 3.11 GB/s (32 MiB in 10.78 ms)
...
PASS: 4194304 elements bit-exact under 2 launch geometries
```

## Sizing

The CUDA toolchain costs about 2.7 GiB of `/nix/store` on top of the base
system (6.2 GiB total). Incus's default volume is 10 GiB, which fits but leaves
little room — create project instances with `-d root,size=40GiB`.
