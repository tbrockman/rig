package hostgpu

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCard builds a sysfs-shaped tree for one PCI address and points the
// package at it. Returns the device directory so a test can add or remove the
// nodes it cares about.
func fakeCard(t *testing.T, pci string) string {
	t.Helper()
	root := t.TempDir()
	dev := filepath.Join(root, pci)
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	old := sysfsRoot
	sysfsRoot = root
	t.Cleanup(func() { sysfsRoot = old })
	return dev
}

const pci = "0000:04:00.0"

// The reset is the whole point: a card handed back from a guest keeps that
// guest's state, and on Ada the GSP will not boot on top of it.
func TestResetDeviceWritesTheResetNode(t *testing.T) {
	dev := fakeCard(t, pci)
	node := filepath.Join(dev, "reset")
	if err := os.WriteFile(node, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := resetDevice(pci); err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	got, err := os.ReadFile(node)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "1" {
		t.Fatalf("wrote %q to the reset node, want \"1\"", got)
	}
}

// A card with no reset node cannot be fixed without a reboot. Saying so is more
// useful than proceeding to a bind that will look fine and produce no display.
func TestResetDeviceExplainsAMissingResetNode(t *testing.T) {
	fakeCard(t, pci)
	err := resetDevice(pci)
	if err == nil {
		t.Fatal("a card with no reset node was accepted")
	}
	for _, want := range []string{"reset", "reboot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// The regression this whole change exists for. sysfs said "driver: nvidia" and
// /dev/nvidia0 existed while RmInitAdapter had failed, so every check `rig host`
// made passed and the DisplayPort stayed dark. A DRM node is the signal that
// cannot be true unless the adapter really came up.
func TestAdapterReadyIgnoresBindingAndLooksForADrmNode(t *testing.T) {
	dev := fakeCard(t, pci)

	// Bound, with a device node, but no adapter — the exact failing state.
	if err := os.Symlink("/fake/drivers/nvidia", filepath.Join(dev, "driver")); err != nil {
		t.Fatal(err)
	}
	if adapterReady(pci) {
		t.Fatal("reported ready with no DRM node; that is the bug this replaces")
	}

	if err := os.MkdirAll(filepath.Join(dev, "drm", "card0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !adapterReady(pci) {
		t.Fatal("a DRM node exists but the adapter was reported not ready")
	}
}

// An empty drm/ directory is not an adapter. Globbing loosely would call it one.
func TestAdapterReadyRejectsAnEmptyDrmDirectory(t *testing.T) {
	dev := fakeCard(t, pci)
	if err := os.MkdirAll(filepath.Join(dev, "drm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if adapterReady(pci) {
		t.Fatal("an empty drm/ directory was treated as a working adapter")
	}
}

func TestWaitAdapterGivesUpRatherThanHanging(t *testing.T) {
	fakeCard(t, pci)
	start := time.Now()
	if waitAdapter(pci, 400*time.Millisecond) {
		t.Fatal("reported ready for a card that never came up")
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Fatalf("returned after %v, before its own deadline", elapsed)
	}
}

// The node appears a moment after the module loads, not with it, so the poll
// has to tolerate the gap rather than checking once.
func TestWaitAdapterSucceedsWhenTheNodeArrivesLate(t *testing.T) {
	dev := fakeCard(t, pci)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.MkdirAll(filepath.Join(dev, "drm", "card0"), 0o755)
	}()
	if !waitAdapter(pci, 5*time.Second) {
		t.Fatal("did not notice the DRM node appearing during the wait")
	}
}
