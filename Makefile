# Binaries land in the repo root so ./rig keeps working from the docs.
BINS := rig

all: $(BINS)

$(BINS): %: FORCE
	go build -o $@ ./cmd/$@

# The integration suite is build-tagged out of ./..., so vet it explicitly or
# it rots unnoticed: it once stopped compiling for weeks behind a signature
# change nothing else noticed.
test: FORCE
	go vet ./...
	go vet -tags integration ./integration/
	go test ./...

# schema/rig.schema.json and README.md's field table, from internal/manifest's
# types and doc comments. `make test` fails when they are stale.
generate: FORCE
	go test ./internal/manifest -run TestGeneratedFilesAreCurrent -update

# Against real Incus and the real card. Destructive: creates and deletes
# instances, moves the GPU, and starts a VM with no isolation on purpose.
# Refuses to run while anything else is up.
test-integration: rig FORCE
	go test -tags integration ./integration/ -v -timeout 40m

# Onto PATH, for use from any directory: no verb assumes it runs from this
# checkout, and the only thing tying a command to it is a path you type.
install: FORCE
	go install ./cmd/rig

clean: FORCE
	rm -f $(BINS)

FORCE:
.PHONY: all test generate test-integration install clean FORCE
