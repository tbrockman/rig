// vectoradd.cu — CUDA correctness test for the passed-through GPU.
//
// This exists to answer one question: does a kernel on this VM's GPU produce
// bit-exact correct results for every element? It is deliberately not a
// benchmark, and deliberately not the usual tutorial vector-add, which passes
// even when the kernel never runs.
//
// What the tutorial version gets wrong, and what this does instead:
//
//   1. Zero-initialised output. A kernel that never launched leaves zeros, and
//      0 + 0 == 0 checks out. Here the device buffer is POISONED with a
//      distinctive bit pattern, and the poison is read back and verified
//      *before* the launch, so "the kernel wrote nothing" cannot pass. The host
//      receive buffer is poisoned with a different pattern, so "the D2H copy
//      did nothing" cannot pass either.
//
//   2. Expectations computed from the same floats the GPU was handed. Here the
//      inputs are integer multiples of 1/8, and the expectation is computed in
//      INTEGER arithmetic from the generator: a[i] = ka/8, b[i] = kb/8, so the
//      exact sum is (ka+kb)/8. Every value involved is exactly representable in
//      binary32 (ka, kb < 2^21, so ka+kb < 2^22 < 2^24, and scaling by 1/8 only
//      shifts the exponent), which means correct IEEE-754 addition is required
//      to produce *exactly* this — no epsilon, no tolerance. A corrupted H2D
//      transfer fails the check too, because the expectation never touched the
//      input arrays.
//
//   3. Spot-checking a few elements. All 4,194,304 are compared, bitwise on the
//      raw uint32, which also catches -0.0 vs +0.0 and any NaN payload.
//
//   4. A checker that cannot fail. A negative control at the end corrupts one
//      element by a single bit and asserts the comparator reports exactly one
//      mismatch, so a vacuously-passing test is itself detected.
//
// The kernel runs twice under different launch geometries: once with one thread
// per element, once with far fewer threads than elements so the grid-stride
// loop has to iterate. Both are checked in full.
//
// Exit codes: 0 all checks passed, 1 a check failed, 2 a CUDA call failed.

#include <cuda_runtime.h>

#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>

#define CUDA_CHECK(call)                                                       \
    do {                                                                       \
        cudaError_t err_ = (call);                                             \
        if (err_ != cudaSuccess) {                                             \
            std::fprintf(stderr, "CUDA error at %s:%d\n  %s\n  -> %s (%d)\n",  \
                         __FILE__, __LINE__, #call,                            \
                         cudaGetErrorString(err_), (int)err_);                 \
            std::exit(2);                                                      \
        }                                                                      \
    } while (0)

// 4,194,304 elements = 16 MiB per float array.
static const size_t N = 1u << 22;

// Poison patterns. Every legitimate result is a non-negative multiple of 1/8;
// both patterns are negative and neither is a multiple of 1/8, so a stale value
// can never be mistaken for a real one.
static const uint32_t POISON_DEVICE = 0xDEADBEEFu;  // ~ -6.26e18
static const uint32_t POISON_HOST   = 0xBAADF00Du;  // ~ -1.33e-3

// ---------------------------------------------------------------------------
// Input generation
// ---------------------------------------------------------------------------

// splitmix32. Any decent avalanche works; the point is that neighbouring
// indices produce unrelated values, so a kernel that reads the wrong element
// (off-by-one, wrong stride, a block that overwrites its neighbour) produces a
// wildly wrong result instead of a nearly-right one.
static uint32_t mix32(uint32_t x)
{
    x += 0x9e3779b9u;
    x ^= x >> 16;
    x *= 0x21f0aaadu;
    x ^= x >> 15;
    x *= 0x735a2d97u;
    x ^= x >> 15;
    return x;
}

// Both operands are integer multiples of 1/8 below 2^18, so their sum is
// exactly representable and the GPU has no rounding freedom at all.
static const uint32_t K_MASK = 0x1FFFFFu;  // 21 bits: ka, kb < 2^21
static const float    SCALE  = 0.125f;     // 1/8, an exact power of two

// ---------------------------------------------------------------------------

__global__ void vector_add(const float *__restrict__ a,
                           const float *__restrict__ b,
                           float *__restrict__ c, size_t n)
{
    size_t stride = (size_t)gridDim.x * blockDim.x;
    for (size_t i = (size_t)blockIdx.x * blockDim.x + threadIdx.x; i < n;
         i += stride) {
        c[i] = a[i] + b[i];
    }
}

// ---------------------------------------------------------------------------

static uint32_t bits(float f)
{
    uint32_t u;
    std::memcpy(&u, &f, sizeof u);
    return u;
}

// Bitwise comparison of every element. Returns the number of mismatches and
// prints the first few with enough detail to tell a transfer bug from an
// arithmetic bug.
static size_t compare_all(const float *got, const float *expected, size_t n,
                          const char *what, bool verbose)
{
    size_t bad = 0;
    for (size_t i = 0; i < n; i++) {
        uint32_t g = bits(got[i]);
        uint32_t e = bits(expected[i]);
        if (g == e) continue;
        bad++;
        if (verbose && bad <= 8) {
            const char *note = "";
            if (g == POISON_DEVICE) note = "  <-- device buffer never written";
            if (g == POISON_HOST)   note = "  <-- host buffer never filled (D2H did nothing)";
            std::fprintf(stderr,
                         "  %s: element %zu got 0x%08x (%.6f) expected "
                         "0x%08x (%.6f)%s\n",
                         what, i, g, (double)got[i], e, (double)expected[i],
                         note);
        }
    }
    return bad;
}

struct LaunchConfig {
    const char *name;
    unsigned    blocks;
    unsigned    threads;
};

int main(void)
{
    int devices = 0;
    cudaError_t err = cudaGetDeviceCount(&devices);
    if (err != cudaSuccess || devices == 0) {
        std::fprintf(stderr,
                     "No CUDA device: %s\n"
                     "Either the GPU is not attached to this VM (check "
                     "`gpu-check`), or libcuda.so.1 is not on the library "
                     "path (it comes from the driver, not the toolkit).\n",
                     cudaGetErrorString(err));
        return 2;
    }

    cudaDeviceProp prop;
    CUDA_CHECK(cudaGetDeviceProperties(&prop, 0));

    int driver_version = 0, runtime_version = 0;
    CUDA_CHECK(cudaDriverGetVersion(&driver_version));
    CUDA_CHECK(cudaRuntimeGetVersion(&runtime_version));

    std::printf("device      : %s (compute %d.%d, %d SMs, %.0f MiB)\n",
                prop.name, prop.major, prop.minor, prop.multiProcessorCount,
                (double)prop.totalGlobalMem / (1024.0 * 1024.0));
    std::printf("driver/rt   : %d.%d / %d.%d\n", driver_version / 1000,
                (driver_version % 1000) / 10, runtime_version / 1000,
                (runtime_version % 1000) / 10);
    std::printf("elements    : %zu (%.0f MiB per array)\n", N,
                (double)(N * sizeof(float)) / (1024.0 * 1024.0));

    const size_t bytes = N * sizeof(float);

    float *a        = (float *)std::malloc(bytes);
    float *b        = (float *)std::malloc(bytes);
    float *expected = (float *)std::malloc(bytes);
    float *got      = (float *)std::malloc(bytes);
    float *poison   = (float *)std::malloc(bytes);
    if (!a || !b || !expected || !got || !poison) {
        std::fprintf(stderr, "host allocation failed\n");
        return 2;
    }

    // Inputs, and expectations derived from the integer generator rather than
    // from the float arrays.
    for (size_t i = 0; i < N; i++) {
        uint32_t ka = mix32((uint32_t)(2 * i)) & K_MASK;
        uint32_t kb = mix32((uint32_t)(2 * i + 1)) & K_MASK;
        a[i]        = (float)ka * SCALE;
        b[i]        = (float)kb * SCALE;
        expected[i] = (float)(ka + kb) * SCALE;  // exact, by construction
    }
    for (size_t i = 0; i < N; i++) {
        uint32_t p = POISON_DEVICE;
        std::memcpy(&poison[i], &p, sizeof p);
    }

    float *d_a = NULL, *d_b = NULL, *d_c = NULL;
    CUDA_CHECK(cudaMalloc(&d_a, bytes));
    CUDA_CHECK(cudaMalloc(&d_b, bytes));
    CUDA_CHECK(cudaMalloc(&d_c, bytes));

    // ---- H2D, timed. The link on this host is a chipset x2 slot; printing the
    // rate keeps that visible, because a profile taken here does not generalise
    // to an x16 machine.
    cudaEvent_t t0, t1;
    CUDA_CHECK(cudaEventCreate(&t0));
    CUDA_CHECK(cudaEventCreate(&t1));

    CUDA_CHECK(cudaEventRecord(t0));
    CUDA_CHECK(cudaMemcpy(d_a, a, bytes, cudaMemcpyHostToDevice));
    CUDA_CHECK(cudaMemcpy(d_b, b, bytes, cudaMemcpyHostToDevice));
    CUDA_CHECK(cudaEventRecord(t1));
    CUDA_CHECK(cudaEventSynchronize(t1));
    float h2d_ms = 0.0f;
    CUDA_CHECK(cudaEventElapsedTime(&h2d_ms, t0, t1));
    std::printf("H2D         : %.2f GB/s (%zu MiB in %.2f ms)\n",
                (double)(2 * bytes) / (h2d_ms * 1e-3) / 1e9,
                (2 * bytes) / (1024 * 1024), (double)h2d_ms);

    const LaunchConfig configs[] = {
        // One thread per element; the tail guard matters because 4194304 is not
        // a multiple of 384.
        {"one-thread-per-element", (unsigned)((N + 383) / 384), 384},
        // Far fewer threads than elements: every thread must iterate the
        // grid-stride loop 32 times. A kernel that assumes one element per
        // thread leaves 31/32 of the buffer poisoned.
        {"grid-stride", 512, 256},
    };

    int failures = 0;

    for (size_t ci = 0; ci < sizeof(configs) / sizeof(configs[0]); ci++) {
        const LaunchConfig &cfg = configs[ci];
        std::printf("\n--- run %zu: %s (%u blocks x %u threads)\n", ci + 1,
                    cfg.name, cfg.blocks, cfg.threads);

        // Poison the device buffer and prove the poison is actually there. If
        // this readback ever showed something else, the rest of the run would
        // be meaningless.
        CUDA_CHECK(cudaMemcpy(d_c, poison, bytes, cudaMemcpyHostToDevice));
        CUDA_CHECK(cudaMemcpy(got, d_c, bytes, cudaMemcpyDeviceToHost));
        size_t unpoisoned = 0;
        for (size_t i = 0; i < N; i++) {
            if (bits(got[i]) != POISON_DEVICE) unpoisoned++;
        }
        if (unpoisoned) {
            std::fprintf(stderr,
                         "  FAIL: device output buffer did not take the poison "
                         "(%zu of %zu elements)\n",
                         unpoisoned, N);
            failures++;
            continue;
        }
        std::printf("  poison      : verified on all %zu elements\n", N);

        // Poison the host receive buffer with a *different* pattern, so a D2H
        // copy that silently does nothing is distinguishable from a kernel that
        // silently does nothing.
        for (size_t i = 0; i < N; i++) {
            uint32_t p = POISON_HOST;
            std::memcpy(&got[i], &p, sizeof p);
        }

        CUDA_CHECK(cudaEventRecord(t0));
        vector_add<<<cfg.blocks, cfg.threads>>>(d_a, d_b, d_c, N);
        // Launch errors surface here; execution errors surface at the sync.
        CUDA_CHECK(cudaGetLastError());
        CUDA_CHECK(cudaEventRecord(t1));
        CUDA_CHECK(cudaDeviceSynchronize());
        float kernel_ms = 0.0f;
        CUDA_CHECK(cudaEventElapsedTime(&kernel_ms, t0, t1));

        CUDA_CHECK(cudaEventRecord(t0));
        CUDA_CHECK(cudaMemcpy(got, d_c, bytes, cudaMemcpyDeviceToHost));
        CUDA_CHECK(cudaEventRecord(t1));
        CUDA_CHECK(cudaEventSynchronize(t1));
        float d2h_ms = 0.0f;
        CUDA_CHECK(cudaEventElapsedTime(&d2h_ms, t0, t1));

        std::printf("  kernel      : %.3f ms\n", (double)kernel_ms);
        std::printf("  D2H         : %.2f GB/s (%.2f ms)\n",
                    (double)bytes / (d2h_ms * 1e-3) / 1e9, (double)d2h_ms);

        size_t bad = compare_all(got, expected, N, cfg.name, true);
        if (bad) {
            std::fprintf(stderr, "  FAIL: %zu of %zu elements wrong\n", bad, N);
            failures++;
        } else {
            std::printf("  result      : all %zu elements bit-exact\n", N);
        }
    }

    // ---- Negative control -------------------------------------------------
    // The comparator has just reported success 2/2 times. Prove it is capable
    // of reporting failure: perturb one element by the smallest possible
    // amount — a single mantissa bit, a change of 1 ULP that any tolerance-
    // based check would wave through — and require exactly one mismatch.
    // Built from `expected` rather than from the last run's output so that it
    // still tests the comparator when a run above has already failed.
    {
        const size_t victim = N / 3 + 7;
        std::memcpy(got, expected, bytes);
        uint32_t flipped = bits(got[victim]) ^ 1u;
        std::memcpy(&got[victim], &flipped, sizeof flipped);

        size_t bad = compare_all(got, expected, N, "negative-control", false);

        if (bad == 1) {
            std::printf(
                "\nnegative ctl: comparator caught a 1-ULP corruption at "
                "element %zu\n",
                victim);
        } else {
            std::fprintf(stderr,
                         "\nFAIL: negative control reported %zu mismatches, "
                         "expected exactly 1. The comparator is not working, "
                         "so the passes above mean nothing.\n",
                         bad);
            failures++;
        }
    }

    CUDA_CHECK(cudaEventDestroy(t0));
    CUDA_CHECK(cudaEventDestroy(t1));
    CUDA_CHECK(cudaFree(d_a));
    CUDA_CHECK(cudaFree(d_b));
    CUDA_CHECK(cudaFree(d_c));
    std::free(a);
    std::free(b);
    std::free(expected);
    std::free(got);
    std::free(poison);

    if (failures) {
        std::printf("\nFAILED (%d check%s)\n", failures,
                    failures == 1 ? "" : "s");
        return 1;
    }
    std::printf("\nPASS: %zu elements bit-exact under 2 launch geometries\n", N);
    return 0;
}
