# Build inside the devShell:  nix develop -c make run
NVCC      ?= nvcc

# Compile for the card that is actually present, as one real arch and not a
# PTX fallback: the binary then either matches the card or fails loudly, rather
# than silently JIT-compiling and hiding an arch mismatch. nvidia-smi reports
# compute capability as "8.9"; sm_89 is the same number without the dot.
COMPUTE_CAP := $(shell nvidia-smi --query-gpu=compute_cap --format=csv,noheader 2>/dev/null | head -n1 | tr -d '. ')
ifeq ($(COMPUTE_CAP),)
  $(warning nvidia-smi sees no GPU; set ARCH=sm_XX by hand, e.g. ARCH=sm_89 for Ada)
  ARCH    ?= sm_89
else
  ARCH    ?= sm_$(COMPUTE_CAP)
endif

NVCCFLAGS ?= -O2 -arch=$(ARCH) -lineinfo -Xcompiler -Wall -Xcompiler -Wextra

all: vectoradd

vectoradd: vectoradd.cu
	$(NVCC) $(NVCCFLAGS) -o $@ $<

run: vectoradd
	./vectoradd

clean:
	rm -f vectoradd

.PHONY: all run clean
