package volumes

import (
	"testing"

	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
)

// Mount what is declared, unmount what rig mounted and is no longer declared,
// and leave every other device alone, a disk someone else added included.
func TestPlanMountsExactlyTheDeclaredVolumes(t *testing.T) {
	inst := &incus.Instance{
		Name: "daw",
		Devices: map[string]incus.Device{
			"rig-vol-old":  {"type": "disk", "pool": "fast", "source": "old", "path": "/srv/old"},
			"someone-else": {"type": "disk", "pool": "fast", "source": "theirs", "path": "/srv/theirs"},
		},
		ExpandedDevices: map[string]incus.Device{"root": {"type": "disk", "pool": "fast", "path": "/"}},
	}
	if got := Pool(inst); got != "fast" {
		t.Fatalf("pool %q, want the root disk's", got)
	}
	vols := []manifest.Volume{{Name: "daw-documents", Path: "/home/me/Documents"}}
	devices, changes := Plan(inst, vols, "fast")
	if _, ok := devices["rig-vol-old"]; ok {
		t.Error("an undeclared rig volume must be unmounted")
	}
	if devices["someone-else"] == nil {
		t.Error("a disk rig did not add is not rig's to remove")
	}
	if d := devices["rig-vol-daw-documents"]; d["source"] != "daw-documents" || d["path"] != "/home/me/Documents" || d["pool"] != "fast" {
		t.Errorf("declared volume not mounted as declared: %v", d)
	}
	if len(changes) != 2 {
		t.Errorf("want one unmount and one mount, got %v", changes)
	}

	inst.Devices = devices
	if _, again := Plan(inst, vols, "fast"); len(again) != 0 {
		t.Errorf("already reconciled: want no changes, got %v", again)
	}
	if held := Held(inst); len(held) != 1 || held[0] != "daw-documents" {
		t.Errorf("Held = %v", held)
	}
}
