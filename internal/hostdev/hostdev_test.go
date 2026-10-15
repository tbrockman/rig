package hostdev

import (
	"os"
	"os/exec"
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

// holding starts a process with node open as its stdin, the way a program
// holds a card, and reaps it when it exits so an ended one leaves no zombie
// holding a PID. script runs under sh when set, else the process is a sleep.
func holding(t *testing.T, node, script string) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if script != "" {
		cmd = exec.Command("sh", "-c", script)
	}
	if node != "" {
		f, err := os.Open(node)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		cmd.Stdin = f
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, done
}

// execed waits until pid is running name with its command line in place, so
// what a test reads or signals is that program and not the fork before it.
func execed(t *testing.T, pid int, name string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if p := Describe(pid); p.Name == name && p.Args != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never became %s", pid, name)
		}
	}
}

func exited(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("still running")
		return nil
	}
}

// Only a listed process that holds the node is ended. The list comes from
// Incus a moment earlier, and anything on it that has let go since, or is a
// PID reused by something else, must be left alone.
func TestEndTermsWhatHoldsTheNodeAndSparesTheRest(t *testing.T) {
	holder, holderDone := holding(t, "/dev/full", "")
	bystander, bystanderDone := holding(t, "", "")
	execed(t, holder.Process.Pid, "sleep")
	execed(t, bystander.Process.Pid, "sleep")

	if err := End("/dev/full", []int{holder.Process.Pid, bystander.Process.Pid}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := exited(t, holderDone); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Errorf("the holder should have ended on SIGTERM, got %v", err)
	}
	select {
	case err := <-bystanderDone:
		t.Errorf("a process not holding the node was ended: %v", err)
	default:
	}
}

// A holder that ignores SIGTERM gets SIGKILL once the grace period is out,
// rather than End reporting success or waiting forever.
func TestEndKillsWhatIgnoresTerm(t *testing.T) {
	stubborn, done := holding(t, "/dev/full", `trap "" TERM; exec sleep 60`)
	execed(t, stubborn.Process.Pid, "sleep") // so the trap is set before the signal arrives
	if err := End("/dev/full", []int{stubborn.Process.Pid}, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := exited(t, done); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Errorf("should have taken SIGKILL, got %v", err)
	}
}

// The root verb ends processes for holding a device and nothing else, so a
// path that is not a device node under /dev is refused before any signal.
func TestEndRefusesWhatIsNotADeviceNode(t *testing.T) {
	for _, node := range []string{"/etc/hostname", "/dev/../etc/hostname", "dev/full", "/dev"} {
		if err := End(node, []int{os.Getpid()}, time.Second); err == nil {
			t.Errorf("%s accepted", node)
		}
	}
}

// Incus names holders by PID alone; the message has to say what they are.
func TestDescribeNamesAProcessAndAGoneOne(t *testing.T) {
	cmd, done := holding(t, "", "")
	execed(t, cmd.Process.Pid, "sleep")
	p := Describe(cmd.Process.Pid)
	if p.Name != "sleep" || p.Args != "sleep 60" || p.User == "" {
		t.Errorf("got %+v", p)
	}
	if s := p.String(); !strings.HasPrefix(s, "sleep 60 (pid ") {
		t.Errorf("String() = %q", s)
	}
	_ = cmd.Process.Kill()
	exited(t, done)
	if s := Describe(cmd.Process.Pid).String(); !strings.HasSuffix(s, "(gone)") {
		t.Errorf("an exited process reads as %q", s)
	}
}

// boundTo points a fake device's driver link at a fake driver directory with
// an unbind node, the way sysfs shows a bound device.
func boundTo(t *testing.T, dev, driver string) string {
	t.Helper()
	drv := filepath.Join(filepath.Dir(dev), "..", "drivers", driver)
	if err := os.MkdirAll(drv, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(drv, "unbind"), filepath.Join(dev, "reset"), filepath.Join(dev, "driver_override")} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(drv, filepath.Join(dev, "driver")); err != nil {
		t.Fatal(err)
	}
	return drv
}

// Incus rebinds a card to nvidia by itself when its VM stops, with no reset.
// The driver is bound and the card is dead, and Return used to call that
// "already on nvidia". It has to go through the whole return instead.
func TestReturnRedoesADeviceBoundButNotAlive(t *testing.T) {
	dev := fakeDevice(t, pci)
	drv := boundTo(t, dev, "nvidia")
	// The card comes up once it has been reset, and not before.
	go func() {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if b, _ := os.ReadFile(filepath.Join(dev, "reset")); string(b) == "1" {
				os.MkdirAll(filepath.Join(dev, "drm", "card1"), 0o755)
				return
			}
		}
	}()
	if err := (Spec{PCI: pci, Reset: true, Alive: "drm/card*"}).Return(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(drv, "unbind")); string(b) != pci {
		t.Errorf("not unbound from the dead driver first: unbind got %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dev, "driver_override")); string(b) != "\n" {
		t.Errorf("driver_override left as %q", b)
	}
}

// A device that is on its driver and alive is left alone: resetting a card
// the host is using would take the display with it.
func TestReturnLeavesAWorkingDeviceAlone(t *testing.T) {
	dev := fakeDevice(t, pci)
	boundTo(t, dev, "nvidia")
	if err := os.MkdirAll(filepath.Join(dev, "drm", "card1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (Spec{PCI: pci, Reset: true, Alive: "drm/card*"}).Return(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dev, "reset")); len(b) != 0 {
		t.Errorf("a working device was reset")
	}
}

// fakeDevNodes points the package at an empty /dev and an nvidia driver
// that has the card as minor, when minor is set, and returns the /dev.
func fakeDevNodes(t *testing.T, minor string) string {
	t.Helper()
	dev, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gpus := t.TempDir()
	oldDev, oldGPUs := devRoot, nvidiaProcRoot
	devRoot, nvidiaProcRoot = dev, gpus
	t.Cleanup(func() { devRoot, nvidiaProcRoot = oldDev, oldGPUs })
	if minor != "" {
		if err := os.MkdirAll(filepath.Join(gpus, pci), 0o755); err != nil {
			t.Fatal(err)
		}
		info := "Model: \t\t Fake Card\nDevice Minor: \t " + minor + "\nGPU Excluded:\t No\n"
		if err := os.WriteFile(filepath.Join(gpus, pci, "information"), []byte(info), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dev
}

// kernelBindsVfio stands in for the kernel: once a device has been unbound
// with vfio-pci named as its override, its driver link moves to vfio-pci.
func kernelBindsVfio(t *testing.T, devs ...string) {
	t.Helper()
	vfio := filepath.Join(filepath.Dir(filepath.Dir(devs[0])), "drivers", "vfio-pci")
	if err := os.MkdirAll(vfio, 0o755); err != nil {
		t.Fatal(err)
	}
	go func() {
		bound := map[string]bool{}
		for deadline := time.Now().Add(10 * time.Second); len(bound) < len(devs) && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			for _, dev := range devs {
				override, _ := os.ReadFile(filepath.Join(dev, "driver_override"))
				unbind, _ := os.ReadFile(filepath.Join(dev, "driver", "unbind"))
				if bound[dev] || string(override) != "vfio-pci" || string(unbind) != filepath.Base(dev) {
					continue
				}
				os.Remove(filepath.Join(dev, "driver"))
				os.Symlink(vfio, filepath.Join(dev, "driver"))
				bound[dev] = true
			}
		}
	}()
}

// A card and its audio function are taken off their host drivers together,
// as VFIO needs the whole group, and each is checked to be on vfio-pci
// rather than trusted to be.
func TestParkMovesTheGroupOntoVfio(t *testing.T) {
	card := fakeDevice(t, pci)
	root := filepath.Dir(card)
	audio := filepath.Join(root, "0000:2b:00.1")
	if err := os.MkdirAll(audio, 0o755); err != nil {
		t.Fatal(err)
	}
	group := filepath.Join(root, "group13", "devices")
	for addr, class := range map[string]string{pci: "0x030000", "0000:2b:00.1": "0x040300"} {
		if err := os.MkdirAll(filepath.Join(group, addr), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(group, addr, "class"), []byte(class+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, dev := range []string{card, audio} {
		if err := os.Symlink(filepath.Join(root, "group13"), filepath.Join(dev, "iommu_group")); err != nil {
			t.Fatal(err)
		}
	}
	boundTo(t, card, "nvidia")
	boundTo(t, audio, "snd_hda_intel")
	fakeDevNodes(t, "0")
	kernelBindsVfio(t, card, audio)

	s := Spec{PCI: pci}
	if err := s.Park(false); err != nil {
		t.Fatal(err)
	}
	for _, dev := range []string{card, audio} {
		if b, _ := os.ReadFile(filepath.Join(dev, "driver_override")); string(b) != "vfio-pci" {
			t.Errorf("%s: driver_override = %q, want vfio-pci", filepath.Base(dev), b)
		}
	}
	if !s.Parked() {
		t.Error("parked, yet not reported parked")
	}
}

// A device already on vfio-pci, as rig restart leaves it, is not touched.
func TestParkLeavesAParkedDeviceAlone(t *testing.T) {
	dev := fakeDevice(t, pci)
	boundTo(t, dev, "vfio-pci")
	if err := (Spec{PCI: pci}).Park(false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dev, "driver_override")); len(b) != 0 {
		t.Errorf("driver_override written on a parked device: %q", b)
	}
}

// Unbinding a card something holds blocks until it lets go, so a held card
// is refused with its holder named, and nothing is unbound.
func TestParkRefusesWhileTheCardIsHeld(t *testing.T) {
	dev := fakeDevice(t, pci)
	drv := boundTo(t, dev, "nvidia")
	nodes := fakeDevNodes(t, "3")
	node := filepath.Join(nodes, "nvidia3")
	if err := os.WriteFile(node, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	holder, _ := holding(t, node, "")
	execed(t, holder.Process.Pid, "sleep")

	err := (Spec{PCI: pci}).Park(false)
	if err == nil {
		t.Fatal("parked a card that a process holds")
	}
	if !strings.Contains(err.Error(), node) || !strings.Contains(err.Error(), "--kill-holders") {
		t.Errorf("the refusal should name the node and the way past it: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(drv, "unbind")); len(b) != 0 {
		t.Errorf("unbound anyway: %q", b)
	}
}

// What blocks an unbind is the card's own nodes: its DRM nodes and its own
// /dev/nvidiaN. /dev/nvidiactl is shared by every card, and a daemon such as
// nvidia-persistenced keeping it open must not stop a start Incus would allow.
func TestBlockersAreTheCardsOwnNodes(t *testing.T) {
	fakeDevice(t, pci)
	nodes := fakeDevNodes(t, "3")
	if err := os.MkdirAll(filepath.Join(nodes, "dri", "by-path"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dri/card1", "nvidia3", "nvidiactl"} {
		if err := os.WriteFile(filepath.Join(nodes, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../card1", filepath.Join(nodes, "dri", "by-path", "pci-"+pci+"-card")); err != nil {
		t.Fatal(err)
	}
	pids := map[string]int{}
	for _, name := range []string{"dri/card1", "nvidia3", "nvidiactl"} {
		p, _ := holding(t, filepath.Join(nodes, name), "")
		execed(t, p.Process.Pid, "sleep")
		pids[name] = p.Process.Pid
	}

	held := Spec{PCI: pci}.blockers()
	if len(held) != 2 {
		t.Fatalf("blockers = %v, want the DRM node and nvidia3 only", held)
	}
	for _, name := range []string{"dri/card1", "nvidia3"} {
		got := held[filepath.Join(nodes, name)]
		if len(got) != 1 || got[0] != pids[name] {
			t.Errorf("%s held by %v, want [%d]", name, got, pids[name])
		}
	}
}
