# Binaries land in the repo root so ./rig and ./gpuctl keep working, and so the
# permission rules in .claude/settings.json keep matching.
BINS := rig gpuctl hostgpu

all: $(BINS)

$(BINS): %: FORCE
	go build -o $@ ./cmd/$@

test: FORCE
	go vet ./...
	go test ./...

# Against real Incus. test-invariants needs the base image; test-network-acl
# needs a running instance.
test-invariants: rig gpuctl FORCE
	./test-invariants.sh nixos-gpu-base

clean: FORCE
	rm -f $(BINS)

FORCE:
.PHONY: all test test-invariants clean FORCE
