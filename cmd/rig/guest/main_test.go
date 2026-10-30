package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tbrockman/rig/internal/agent"
)

// What the guest build can do is what it imports. It must not be able to
// reach Incus, and must not carry the code that holds the operator's private
// key: the guest is where the agent is root.
func TestTheGuestBuildImportsNoHostSide(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/tbrockman/rig/cmd/rig/guest").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	deps := "\n" + string(out)
	for _, forbidden := range []string{
		"github.com/tbrockman/rig/internal/incus",
		"github.com/tbrockman/rig/internal/agent/operator",
		"github.com/tbrockman/rig/internal/proxy",
		"github.com/tbrockman/rig/internal/creds",
		"github.com/coder/websocket",
		"crypto/x509",
	} {
		if strings.Contains(deps, "\n"+forbidden+"\n") {
			t.Errorf("the guest build imports %s", forbidden)
		}
	}
}

// The only verbs are the ones the guest can use.
func TestTheGuestBuildHasOnlyGuestVerbs(t *testing.T) {
	var names []string
	for _, c := range root().Commands() {
		names = append(names, c.Name())
	}
	if got := strings.Join(names, " "); got != "ask brief done hook inbox wait" {
		t.Errorf("verbs: %s", got)
	}
}

// `rig ask --wait` with no answer exits 2, which the agent can tell from an
// error; asking without --wait is a plain success.
func TestAskExitsTwoWhenNoAnswerCame(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RIG_AGENT_DIR", dir)
	t.Setenv("RIG_AGENT_SESSION", "sess")

	r := root()
	r.SetArgs([]string{"ask", "cuda", "Bump", "CUDA?"})
	if err := r.Execute(); err != nil {
		t.Fatalf("asking: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, agent.AsksName, "cuda.json")); err != nil {
		t.Fatal(err)
	}
	r = root()
	r.SetArgs([]string{"ask", "cuda", "Bump", "CUDA?", "--wait", "1s"})
	var code exitCode
	if err := r.Execute(); !errors.As(err, &code) || code != 2 {
		t.Errorf("want exit 2 for no answer yet, got %v", err)
	}
}
