package gpu

import (
	"testing"

	"rig/internal/incus"
)

func TestHoldersOnlyCountsLocalDevices(t *testing.T) {
	instances := []incus.Instance{
		{
			Name: "a", Status: "Stopped",
			Devices: map[string]incus.Device{
				"gpu0": {"type": "gpu", "pci": "0000:04:00.0"},
				"root": {"type": "disk"},
			},
		},
		{
			Name: "b", Status: "Running",
			Devices: map[string]incus.Device{"root": {"type": "disk"}},
			// Inherited from a profile: still a misconfiguration, but not this
			// instance's device. CheckProfiles is what catches it.
			ExpandedDevices: map[string]incus.Device{"gpu0": {"type": "gpu"}},
		},
	}
	holders := Holders(instances)
	if len(holders) != 1 {
		t.Fatalf("got %d holders, want 1: %+v", len(holders), holders)
	}
	if holders[0].Instance != "a" || holders[0].PCI != "0000:04:00.0" {
		t.Fatalf("unexpected holder %+v", holders[0])
	}
}

func TestHoldersReportsUnknownPCI(t *testing.T) {
	instances := []incus.Instance{{
		Name: "a", Status: "Stopped",
		Devices: map[string]incus.Device{"gpu0": {"type": "gpu"}},
	}}
	if got := Holders(instances)[0].PCI; got != "?" {
		t.Fatalf("PCI = %q, want ?", got)
	}
}

func TestConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("GPUCTL_DEVICE", "")
	t.Setenv("GPUCTL_ACL", "")
	cfg := ConfigFromEnv()
	if cfg.Device != DefaultDevice || cfg.ACL != DefaultACL {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestConfigFromEnvOverrides(t *testing.T) {
	t.Setenv("GPUCTL_ACL", "custom-acl")
	if cfg := ConfigFromEnv(); cfg.ACL != "custom-acl" {
		t.Fatalf("ACL = %q, want custom-acl", cfg.ACL)
	}
}
