# Binaries land in the repo root so ./rig keeps working from the docs.
BINS := rig hostgpu

all: $(BINS)

$(BINS): %: FORCE
	go build -o $@ ./cmd/$@

test: FORCE
	go vet ./...
	go test ./...

# Against real Incus. Needs the base image.
test-invariants: rig FORCE
	./test-invariants.sh nixos-gpu-base

clean: FORCE
	rm -f $(BINS)

FORCE:
.PHONY: all test test-invariants clean FORCE
