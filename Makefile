# Binaries land in the repo root so ./rig keeps working from the docs.
BINS := rig

# The guest build of rig (cmd/rig/guest), which `rig agent start` installs in
# the guest. rig embeds it from internal/agent/guestbin/bin, so it is built
# first. Static, for any guest; one per architecture Incus runs. The flags are
# the ones TestEmbeddedGuestBuildIsCurrent rebuilds with to catch a stale one.
GUEST_DIR   := internal/agent/guestbin/bin
GUEST_BUILD := CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags='-s -w'

all: $(BINS)

guest: FORCE
	GOARCH=amd64 $(GUEST_BUILD) -o $(GUEST_DIR)/rig-linux-amd64 ./cmd/rig/guest
	GOARCH=arm64 $(GUEST_BUILD) -o $(GUEST_DIR)/rig-linux-arm64 ./cmd/rig/guest

$(BINS): %: guest FORCE
	go build -o $@ ./cmd/$@

# The integration suite is build-tagged out of ./..., so vet it explicitly or
# it rots unnoticed: it once stopped compiling for weeks behind a signature
# change nothing else noticed.
test: guest FORCE
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

# rig's witmproxy plugin. Its signed component is committed under
# internal/proxy/plugin, so this is only for changing the plugin.
plugin: FORCE
	$(MAKE) -C plugin

# Onto PATH, for use from any directory: no verb assumes it runs from this
# checkout, and the only thing tying a command to it is a path you type.
install: guest FORCE
	go install ./cmd/rig

clean: FORCE
	rm -f $(BINS) $(GUEST_DIR)/rig-linux-*

FORCE:
.PHONY: all guest test generate test-integration plugin install clean FORCE
