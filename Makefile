# Binaries land in the repo root so ./rig keeps working from the docs.
BINS := rig

all: $(BINS)

$(BINS): %: FORCE
	go build -o $@ ./cmd/$@

test: FORCE
	go vet ./...
	go test ./...

# Against real Incus and the real card. Destructive: creates and deletes
# instances, moves the GPU, and starts a VM with no isolation on purpose.
# Refuses to run while anything else is up.
test-integration: rig FORCE
	go test -tags integration ./integration/ -v -timeout 40m

clean: FORCE
	rm -f $(BINS)

FORCE:
.PHONY: all test test-integration clean FORCE
