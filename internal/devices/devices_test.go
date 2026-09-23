package devices

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
)

func TestHoldersOnlyCountsLocalDevices(t *testing.T) {
	instances := []incus.Instance{
		{
			Name: "a", Status: "Stopped",
			Devices: map[string]incus.Device{
				"gpu0":     {"type": "gpu", "pci": "0000:2b:00.0"},
				"desk-usb": {"type": "pci", "address": "0000:3c:00.3"},
				"mouse":    {"type": "usb", "vendorid": "1234", "productid": "5678"},
				"root":     {"type": "disk"},
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
	if len(holders) != 3 {
		t.Fatalf("got %d holders, want 3: %+v", len(holders), holders)
	}
	want := map[string]string{"gpu0": "0000:2b:00.0", "desk-usb": "0000:3c:00.3", "mouse": "1234:5678"}
	for _, h := range holders {
		if h.Instance != "a" || want[h.Device] != h.PCI {
			t.Errorf("unexpected holder %+v", h)
		}
		if (h.Kind == "usb") == h.Exclusive() {
			t.Errorf("%s: exclusivity wrong for kind %s", h.Device, h.Kind)
		}
	}
}

func TestHoldersReportsUnknownAddress(t *testing.T) {
	instances := []incus.Instance{{
		Name: "a", Status: "Stopped",
		Devices: map[string]incus.Device{"gpu0": {"type": "gpu"}},
	}}
	if got := Holders(instances)[0].PCI; got != "?" {
		t.Fatalf("address = %q, want ?", got)
	}
}

// The three kinds and their Incus spellings are the whole vocabulary; a
// round trip through an Incus device must give the address back.
func TestIncusDeviceRoundTripsTheAddress(t *testing.T) {
	for _, d := range []Decl{
		{Name: "gpu", Kind: manifest.KindGPU, PCI: "0000:2b:00.0"},
		{Name: "usbc", Kind: manifest.KindPCI, PCI: "0000:3c:00.3"},
		{Name: "mouse", Kind: manifest.KindUSB, ID: "1234:5678"},
	} {
		dev := d.IncusDevice()
		if dev.Type() != d.Kind {
			t.Errorf("%s: incus type %q", d.Name, dev.Type())
		}
		if got := addressOf(dev); got != d.Address() {
			t.Errorf("%s: address read back as %q, want %q", d.Name, got, d.Address())
		}
	}
	if (Decl{Kind: "gpu", PCI: "0000:2b:00.0"}).IncusDevice()["gputype"] != "physical" {
		t.Error("a gpu device must be gputype=physical, or Incus makes it a mediated one")
	}
}

// A VM created before the record existed has no marker, and must keep
// claiming the card. Defaulting the other way would silently strip the GPU
// from every existing project.
func TestWantedDefaultsToTheCardForAnUnrecordedInstance(t *testing.T) {
	cfg := Config{Device: "gpu0"}
	for name, conf := range map[string]map[string]string{
		"no config at all": nil,
		"unrelated keys":   {"user.rig.env": "/x"},
		"explicit true":    {legacyGPU: "true"},
	} {
		got, err := Wanted(&incus.Instance{Config: conf}, cfg)
		if err != nil || len(got) != 1 || got[0].Kind != manifest.KindGPU || got[0].Name != "gpu0" {
			t.Errorf("%s: wanted = %+v, %v; should be the card", name, got, err)
		}
	}
}

// Only the exact legacy marker opts out, so a typo cannot quietly disable the
// GPU; and a recorded empty list opts out too, which is what --no-gpu writes.
func TestWantedOptsOutOnlyExplicitly(t *testing.T) {
	cfg := Config{Device: "gpu0"}
	if got, _ := Wanted(&incus.Instance{Config: map[string]string{legacyGPU: "false"}}, cfg); len(got) != 0 {
		t.Error("an explicit false must not claim the card")
	}
	if got, _ := Wanted(&incus.Instance{Config: map[string]string{DevicesKey: "[]"}}, cfg); len(got) != 0 {
		t.Error("a recorded empty list must want nothing")
	}
	for _, v := range []string{"False", "FALSE", "0", "no", ""} {
		if got, _ := Wanted(&incus.Instance{Config: map[string]string{legacyGPU: v}}, cfg); len(got) != 1 {
			t.Errorf("%q is not the marker; it must not disable the GPU", v)
		}
	}
}

func TestWantedReadsTheRecordAndItsRecipe(t *testing.T) {
	decls := []Decl{
		{Name: "gpu", Kind: "gpu", PCI: "0000:2b:00.0", Return: NvidiaReturn("display-manager")},
		{Name: "desk-usb", Kind: "pci", PCI: "0000:3c:00.3", Return: &Return{Reset: true, Alive: "usb*"}},
	}
	inst := &incus.Instance{Config: map[string]string{DevicesKey: Encode(decls)}}
	got, err := Wanted(inst, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Return.Unit != "display-manager" || got[1].Return.Alive != "usb*" || len(got[0].Return.Modules) != 4 {
		t.Fatalf("record did not round-trip: %+v", got)
	}
	_, err = Wanted(&incus.Instance{Name: "x", Config: map[string]string{DevicesKey: "not json"}}, Config{})
	if err == nil || !strings.Contains(err.Error(), DevicesKey) {
		t.Errorf("a corrupt record must be an error naming the key, got %v", err)
	}
}

func TestFromManifestCarriesTheRecipe(t *testing.T) {
	n := manifest.Named{Name: "gpu", Device: manifest.Device{
		Kind: "gpu", PCI: "0000:2b:00.0",
		Return: &manifest.Return{Modules: []string{"nvidia"}, Reset: true, Alive: "drm/card*"},
	}}
	d := FromManifest(n)
	if d.Name != "gpu" || d.PCI != "0000:2b:00.0" || d.Return == nil || d.Return.Alive != "drm/card*" || !d.Return.Reset {
		t.Fatalf("got %+v", d)
	}
	if FromManifest(manifest.Named{Name: "m", Device: manifest.Device{Kind: "usb", ID: "1:2"}}).Return != nil {
		t.Error("a device without a recipe must record none")
	}
}

func TestConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("RIG_DEVICE", "")
	t.Setenv("RIG_ACL", "")
	cfg := ConfigFromEnv()
	if cfg.Device != DefaultDevice || cfg.ACL != DefaultACL {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestConfigFromEnvOverrides(t *testing.T) {
	t.Setenv("RIG_ACL", "custom-acl")
	if cfg := ConfigFromEnv(); cfg.ACL != "custom-acl" {
		t.Fatalf("ACL = %q, want custom-acl", cfg.ACL)
	}
}

// fakePCI points the package at a sysfs-shaped tree holding the given
// devices: address -> {vendor:device, class}.
func fakePCI(t *testing.T, devs map[string][2]string) {
	t.Helper()
	root := t.TempDir()
	for addr, d := range devs {
		dir := filepath.Join(root, addr)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		vendor, device, _ := strings.Cut(d[0], ":")
		for name, v := range map[string]string{"vendor": "0x" + vendor, "device": "0x" + device, "class": d[1]} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	old := pciRoot
	pciRoot = root
	t.Cleanup(func() { pciRoot = old })
}

// The reference host after the Wi-Fi card was disabled in the BIOS: the
// chipset USB controller moved from 12:00.0 to 11:00.0 and the SATA
// controller took 12:00.0. A record naming 12:00.0 must be refused, and the
// refusal must say where the USB controller went.
func TestCheckIdentityRefusesARenumberedAddress(t *testing.T) {
	fakePCI(t, map[string][2]string{
		"0000:2b:00.0": {"10de:abcd", "0x030000"},
		"0000:11:00.0": {"1022:2222", "0x0c0330"},
		"0000:12:00.0": {"1022:3333", "0x010601"},
	})
	stale := Decl{Name: "audio-ctl", Kind: manifest.KindPCI, PCI: "0000:12:00.0", ID: "1022:2222"}
	err := CheckIdentity(stale)
	if err == nil {
		t.Fatal("the SATA controller was accepted as the USB controller")
	}
	for _, want := range []string{"1022:3333", "0000:11:00.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should name %s: %v", want, err)
		}
	}
	if err := CheckIdentity(Decl{Name: "audio-ctl", Kind: manifest.KindPCI, PCI: "0000:11:00.0", ID: "1022:2222"}); err != nil {
		t.Errorf("the right address was refused: %v", err)
	}
}

func TestCheckIdentityWithoutADeclaredID(t *testing.T) {
	fakePCI(t, map[string][2]string{
		"0000:2b:00.0": {"10de:abcd", "0x030000"},
		"0000:12:00.0": {"1022:3333", "0x010601"},
	})
	if err := CheckIdentity(Decl{Name: "gpu", Kind: manifest.KindGPU, PCI: "0000:2b:00.0"}); err != nil {
		t.Errorf("an NVIDIA display controller is what a card without an id must be: %v", err)
	}
	if err := CheckIdentity(Decl{Name: "gpu", Kind: manifest.KindGPU, PCI: "0000:12:00.0"}); err == nil {
		t.Error("a SATA controller was accepted as the card")
	}
	if err := CheckIdentity(Decl{Name: "ctl", Kind: manifest.KindPCI, PCI: "0000:12:00.0"}); err == nil {
		t.Error("a pci device with no identity recorded has nothing to be checked against and must be refused")
	}
	if err := CheckIdentity(Decl{Name: "gone", Kind: manifest.KindPCI, PCI: "0000:3d:00.0", ID: "1022:2222"}); err == nil {
		t.Error("an empty address was accepted")
	}
	if err := CheckIdentity(Decl{Name: "mouse", Kind: manifest.KindUSB, ID: "1234:5678"}); err != nil {
		t.Errorf("a usb device is found by identity already: %v", err)
	}
}
