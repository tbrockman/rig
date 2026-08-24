# Build inside the devShell:  nix develop -c make run
NVCC      ?= nvcc

# RTX 4080 SUPER is AD103 => compute capability 8.9. Compiling for one real
# arch (not a PTX fallback) means the binary either matches the card or fails
# loudly, rather than silently JIT-compiling and hiding an arch mismatch.
ARCH      ?= sm_89

NVCCFLAGS ?= -O2 -arch=$(ARCH) -lineinfo -Xcompiler -Wall -Xcompiler -Wextra

all: vectoradd

vectoradd: vectoradd.cu
	$(NVCC) $(NVCCFLAGS) -o $@ $<

run: vectoradd
	./vectoradd

clean:
	rm -f vectoradd

.PHONY: all run clean
