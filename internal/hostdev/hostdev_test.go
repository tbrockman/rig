package hostdev

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDevice builds a sysfs-shaped tree for one PCI address and points the
// package at it. Returns the device directory so a test can add or remove the
// nodes it cares about.
func fakeDevice(t *testing.T, pci string) string {
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

const pci = "0000:2b:00.0"

// The reset is the whole point: a card handed back from a guest keeps that
// guest's state, and on Ada the GSP will not boot on top of it.
func TestResetDeviceWritesTheResetNode(t *testing.T) {
	dev := fakeDevice(t, pci)
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

// A device with no reset node cannot be fixed without a reboot. Saying so is
// more useful than proceeding to a bind that will look fine and do nothing.
func TestResetDeviceExplainsAMissingResetNode(t *testing.T) {
	fakeDevice(t, pci)
	err := resetDevice(pci)
	if err == nil {
		t.Fatal("a device with no reset node was accepted")
	}
	for _, want := range []string{"reset", "reboot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// The regression this check exists for. sysfs said "driver: nvidia" and
// /dev/nvidia0 existed while RmInitAdapter had failed, so every check `rig host`
// made passed and the DisplayPort stayed dark. A DRM node is the signal that
// cannot be true unless the adapter really came up.
func TestAliveIgnoresBindingAndLooksForThePattern(t *testing.T) {
	dev := fakeDevice(t, pci)

	// Bound, with a device node, but no adapter — the exact failing state.
	if err := os.Symlink("/fake/drivers/nvidia", filepath.Join(dev, "driver")); err != nil {
		t.Fatal(err)
	}
	if alive(pci, "drm/card*") {
		t.Fatal("reported alive with no DRM node; that is the bug this replaces")
	}

	if err := os.MkdirAll(filepath.Join(dev, "drm", "card0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !alive(pci, "drm/card*") {
		t.Fatal("a DRM node exists but the device was reported not alive")
	}
}

// The same check with a USB controller's pattern: a bus directory appears
// under the controller once xhci_hcd has it up.
func TestAliveWorksForAUSBController(t *testing.T) {
	const usb = "0000:3c:00.3"
	dev := fakeDevice(t, usb)
	if alive(usb, "usb*") {
		t.Fatal("reported alive with no bus")
	}
	if err := os.MkdirAll(filepath.Join(dev, "usb3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !alive(usb, "usb*") {
		t.Fatal("a bus exists but the controller was reported not alive")
	}
}

// An empty drm/ directory is not an adapter. Globbing loosely would call it one.
func TestAliveRejectsAnEmptyDrmDirectory(t *testing.T) {
	dev := fakeDevice(t, pci)
	if err := os.MkdirAll(filepath.Join(dev, "drm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if alive(pci, "drm/card*") {
		t.Fatal("an empty drm/ directory was treated as a working adapter")
	}
}

func TestWaitAliveGivesUpRatherThanHanging(t *testing.T) {
	fakeDevice(t, pci)
	start := time.Now()
	if waitAlive(pci, "drm/card*", 400*time.Millisecond) {
		t.Fatal("reported alive for a device that never came up")
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Fatalf("returned after %v, before its own deadline", elapsed)
	}
}

// The node appears a moment after the module loads, not with it, so the poll
// has to tolerate the gap rather than checking once.
func TestWaitAliveSucceedsWhenTheNodeArrivesLate(t *testing.T) {
	dev := fakeDevice(t, pci)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.MkdirAll(filepath.Join(dev, "drm", "card0"), 0o755)
	}()
	if !waitAlive(pci, "drm/card*", 5*time.Second) {
		t.Fatal("did not notice the DRM node appearing during the wait")
	}
}

// A card's audio function shares its IOMMU group and left for the VM with it,
// so it has to come back with it. It used to be hard-coded as function 1;
// now it is whatever sysfs says the group holds, minus the bridge above.
func TestGroupIsReadFromSysfsWithoutTheBridge(t *testing.T) {
	dev := fakeDevice(t, pci)
	root := filepath.Dir(dev)
	group := filepath.Join(root, "group13", "devices")
	for addr, class := range map[string]string{
		"0000:2a:00.0": "0x060400",
		"0000:2b:00.0": "0x030000",
		"0000:2b:00.1": "0x040300",
	} {
		if err := os.MkdirAll(filepath.Join(group, addr), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(group, addr, "class"), []byte(class+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "group13"), filepath.Join(dev, "iommu_group")); err != nil {
		t.Fatal(err)
	}

	got := Spec{PCI: pci}.group()
	if strings.Join(got, ",") != "0000:2b:00.0,0000:2b:00.1" {
		t.Fatalf("group = %v, want the card first then its audio function, no bridge", got)
	}
}

// With no group information at all — a fake tree, or a host with the IOMMU
// off — the device alone is the honest answer, not an error.
func TestGroupFallsBackToTheDeviceAlone(t *testing.T) {
	fakeDevice(t, pci)
	if got := (Spec{PCI: pci}).group(); len(got) != 1 || got[0] != pci {
		t.Fatalf("group = %v", got)
	}
}

// Which nodes count as "holding the device" is per device: its DRM nodes by
// PCI path, and the /dev/nvidia* family only when the recipe is NVIDIA's.
func TestDevNodesFollowByPathAndTheNvidiaFamily(t *testing.T) {
	root := t.TempDir()
	old := devRoot
	devRoot = root
	t.Cleanup(func() { devRoot = old })

	dri := filepath.Join(root, "dri")
	if err := os.MkdirAll(filepath.Join(dri, "by-path"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dri, "card1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../card1", filepath.Join(dri, "by-path", "pci-"+pci+"-card")); err != nil {
		t.Fatal(err)
	}

	plain := Spec{PCI: pci}.DevNodes()
	if len(plain) != 1 || plain[0] != filepath.Join(dri, "card1") {
		t.Errorf("nodes without nvidia = %v", plain)
	}
	nv := Spec{PCI: pci, Modules: []string{"nvidia_drm", "nvidia"}}.DevNodes()
	if len(nv) != 2 || nv[1] != filepath.Join(root, "nvidia") {
		t.Errorf("nodes with nvidia = %v", nv)
	}
	if (Spec{PCI: "0000:3c:00.3"}).DevNodes() != nil {
		t.Error("a device with no DRM nodes and no nvidia module has nothing to hold")
	}
}

// Presence is read from the USB bus in sysfs by ids, the same way the guest
// probe looks, so the two answers are comparable.
func TestUSBPresentReadsTheBusByIDs(t *testing.T) {
	root := t.TempDir()
	old := usbRoot
	usbRoot = root
	t.Cleanup(func() { usbRoot = old })
	dev := filepath.Join(root, "3-2.2.4.1")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dev, "idVendor"), []byte("3151\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "idProduct"), []byte("4010\n"), 0o644)
	if !USBPresent("3151:4010") {
		t.Error("a device on the bus was reported absent")
	}
	if USBPresent("1234:5678") {
		t.Error("a device not on the bus was reported present")
	}
}

// Back is what spares rig stop a sudo prompt for a device Incus already
// rebound. It must not call a device back that is still on vfio-pci, nor one
// whose host driver bound without the recipe's sign of life: that is a dead
// adapter, and the full return with its reset is what fixes it.
func TestBackNeedsAHostDriverAndTheSignOfLife(t *testing.T) {
	const usb = "0000:12:00.0"
	dev := fakeDevice(t, usb)
	drivers := t.TempDir()
	bind := func(name string) {
		t.Helper()
		_ = os.Remove(filepath.Join(dev, "driver"))
		if err := os.Symlink(filepath.Join(drivers, name), filepath.Join(dev, "driver")); err != nil {
			t.Fatal(err)
		}
	}

	if _, ok := Back(usb, "usb*"); ok {
		t.Fatal("no driver bound, yet reported back")
	}
	bind("vfio-pci")
	if _, ok := Back(usb, "usb*"); ok {
		t.Fatal("still on vfio-pci, yet reported back")
	}
	bind("xhci_hcd")
	if _, ok := Back(usb, "usb*"); ok {
		t.Fatal("host driver bound but no bus: that is not back")
	}
	if err := os.MkdirAll(filepath.Join(dev, "usb1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if drv, ok := Back(usb, "usb*"); !ok || drv != "xhci_hcd" {
		t.Fatalf("bound with a bus: got %q, %v; want xhci_hcd, true", drv, ok)
	}
	if _, ok := Back(usb, ""); !ok {
		t.Fatal("with no sign of life in the recipe, a host driver is enough")
	}
}
