package cpuset

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHost is the reference host: 8 cores, SMT sibling of CPU n is n+8.
func fakeHost(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "online"), []byte("0-15\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 16; n++ {
		dir := filepath.Join(root, fmt.Sprintf("cpu%d", n), "topology")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		core := n % 8
		if err := os.WriteFile(filepath.Join(dir, "thread_siblings_list"), []byte(fmt.Sprintf("%d,%d\n", core, core+8)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := Root
	Root = root
	t.Cleanup(func() { Root = old })
}

func TestCheckAcceptsWholeCoresTheHostCanSpare(t *testing.T) {
	fakeHost(t)
	warnings, err := Check("4-7,12-15")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("four whole cores, four left to the host: %v %v", warnings, err)
	}
}

func TestCheckRefusesEveryCPUAndMissingOnes(t *testing.T) {
	fakeHost(t)
	if _, err := Check("0-15"); err == nil {
		t.Error("pinning to every host CPU must be refused")
	}
	// Seven whole cores and one thread of the eighth: the host has no whole core.
	if _, err := Check("0-14"); err == nil || !strings.Contains(err.Error(), "physical core") {
		t.Errorf("a host left one lone thread has no core of its own: %v", err)
	}
	if _, err := Check("12-19"); err == nil || !strings.Contains(err.Error(), "not online") {
		t.Errorf("CPUs 16-19 do not exist here: %v", err)
	}
}

func TestCheckWarnsAboutASplitCore(t *testing.T) {
	fakeHost(t)
	warnings, err := Check("4-7")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 4 {
		t.Errorf("four cores with one thread each pinned: want four warnings, got %v", warnings)
	}
}

func TestIsSetTellsASetFromACount(t *testing.T) {
	for limit, want := range map[string]bool{"8": false, "4-7": true, "4,12": true, "": false} {
		if IsSet(limit) != want {
			t.Errorf("IsSet(%q) = %v", limit, !want)
		}
	}
}
