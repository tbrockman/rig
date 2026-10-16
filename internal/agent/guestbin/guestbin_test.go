package guestbin

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The guest build is a build product embedded in rig, so it can lag the
// source it came from: edit cmd/rig/guest or internal/agent, run `go build
// ./cmd/rig` instead of make, and rig installs yesterday's verifier. So each
// embedded build is rebuilt here, the way make builds it, and must match byte
// for byte. A fresh checkout embeds none and has nothing to check.
func TestEmbeddedGuestBuildIsCurrent(t *testing.T) {
	ents, err := bins.ReadDir("bin")
	if err != nil {
		t.Fatal(err)
	}
	readGuestSources(t)
	checked := 0
	for _, e := range ents {
		arch, ok := strings.CutPrefix(e.Name(), "rig-linux-")
		if !ok {
			continue
		}
		embedded, _ := bins.ReadFile("bin/" + e.Name())
		out := filepath.Join(t.TempDir(), e.Name())
		// Keep in step with the Makefile's guest target. If they drift, this
		// fails on every run, which is how the drift gets noticed.
		cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w",
			"-o", out, "github.com/tbrockman/rig/cmd/rig/guest")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if msg, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building the guest for %s: %v\n%s", arch, err, msg)
		}
		fresh, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(fresh, embedded) {
			t.Errorf("bin/%s is stale: it is not what cmd/rig/guest builds to now. Run make.", e.Name())
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no guest build embedded (a fresh checkout); make builds one, and this test then checks it")
	}
}

func TestForNamesWhatIsMissing(t *testing.T) {
	if _, err := For("riscv64"); err == nil || !strings.Contains(err.Error(), "riscv64") {
		t.Errorf("an unknown architecture must be named, got %v", err)
	}
	if b, err := For("x86_64"); err != nil && !strings.Contains(err.Error(), "make") {
		t.Errorf("a rig built without its guest half must say how to get it, got %v", err)
	} else if err == nil && len(b) < 1<<20 {
		t.Errorf("an embedded guest build of %d bytes is not a build", len(b))
	}
}

// readGuestSources reads every file the guest build is made from. go test
// caches a pass by what the test itself read, and the build above is another
// process: without this, an edit to the guest's source replays a cached pass,
// which is exactly the staleness this test is for.
func readGuestSources(t *testing.T) {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps",
		"-f", "{{if .Module}}{{if .Module.Main}}{{.Dir}}{{end}}{{end}}",
		"github.com/tbrockman/rig/cmd/rig/guest").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	mod, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	root := strings.TrimSpace(string(mod))
	files := []string{filepath.Join(root, "go.mod"), filepath.Join(root, "go.sum")}
	for _, dir := range strings.Fields(string(out)) {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		files = append(files, matches...)
	}
	for _, f := range files {
		if _, err := os.ReadFile(f); err != nil {
			t.Fatal(err)
		}
	}
}
