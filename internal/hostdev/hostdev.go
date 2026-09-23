// Package hostdev moves a device between this host and VM use.
//
// It is the only code in the project that touches the host itself, needs
// root, and can take the display out from under the person running it. The
// privileged half does sysfs and systemctl and nothing else, so the code
// running as root is as small as it can be; discovery, and the guard that
// refuses while a VM still holds the device, happen unprivileged in the caller.
//
// It was written for one card and one display manager. What was NVIDIA-specific
// turned out to be three facts — which modules to cycle, whether to reset,
// what proves the device came up — and those are fields of a Spec now, so a
// USB controller returns through the same routine with different data.
//
// Assumes that when the device is a display adapter the host has another one —
// an iGPU, typically — driving the console, so the card can leave for a VM and
// come back without taking the only display with it.
package hostdev

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Spec is one device and how the host takes it back.
type Spec struct {
	PCI string // e.g. 0000:2b:00.0
	// Modules to unload before the unbind and reload, in reverse, after the
	// reset. The NVIDIA driver needs this: reloaded while the card is still
	// on vfio-pci it grabs the device mid-transition and mismatches.
	Modules []string
	// Reset issues a function-level reset with no driver bound. On Ada the
	// GSP firmware will not boot on top of what a guest left in the card.
	Reset bool
	// Alive is a glob under the device's sysfs directory that only exists once
	// the driver has actually brought the device up. Empty means trust the bind.
	Alive string
	// Unit is a host systemd unit that uses the device, started after a
	// return and stopped before a VM claims it.
	Unit string
}

// Status prints where the device is. Reads only; needs no privilege.
func (s Spec) Status() {
	for _, addr := range s.group() {
		fmt.Printf("%-14s driver: %-10s override: %s\n", addr, Driver(addr), readFile(sysfs(addr, "driver_override")))
	}
	if s.Unit != "" {
		fmt.Printf("%s: %s\n", s.Unit, run("systemctl", "is-active", s.Unit))
	}
}

// Return rebinds the device to its host driver and starts its unit.
//
// The caller must have established that no instance still holds the device:
// the rebind would otherwise race Incus and both sides lose.
func (s Spec) Return() error {
	if Driver(s.PCI) == "vfio-pci" {
		fmt.Printf("returning %s to the host...\n", s.PCI)
		// Order matters: unload stale modules while the device is still on
		// vfio-pci, or the reloaded module grabs it mid-transition.
		if len(s.Modules) > 0 {
			run("modprobe", append([]string{"-r"}, s.Modules...)...)
		}
		// Everything in the IOMMU group left for the VM together, so it all
		// comes back together: a card's audio function, most often.
		members := s.group()
		for _, addr := range members {
			if Driver(addr) != "vfio-pci" {
				continue
			}
			writeSysfs("/sys/bus/pci/drivers/vfio-pci/unbind", addr)
			writeSysfs(sysfs(addr, "driver_override"), "\n")
		}

		// Reset before the driver loads, not after. With no driver bound this
		// is the only moment the device can be cleared of what the guest left
		// in it.
		if s.Reset {
			fmt.Println("resetting...")
			if err := resetDevice(s.PCI); err != nil {
				return fmt.Errorf("%w\n"+
					"Without a reset the driver may bind to a device that will not "+
					"initialise, which looks like a working device that does nothing.\n"+
					"A reboot clears it.", err)
			}
		}

		for i := len(s.Modules) - 1; i >= 0; i-- {
			if out := run("modprobe", s.Modules[i]); out != "" {
				fmt.Println(out)
			}
		}
		for _, addr := range members {
			writeSysfs("/sys/bus/pci/drivers_probe", addr)
		}

		// Binding is not working. Check the thing the device is for.
		if s.Alive != "" && !waitAlive(s.PCI, s.Alive, 15*time.Second) {
			msg := fmt.Sprintf("%s bound to %s but did not come up: nothing matches %s under its sysfs "+
				"directory, so the host has a driver on a device that is not working.",
				Driver(s.PCI), s.PCI, s.Alive)
			if why := kernelComplaint(); why != "" {
				msg += "\n\nThe kernel said:\n" + why
			}
			msg += "\n\nA reboot clears a device that a reset could not."
			return fmt.Errorf("%s", msg)
		}
	} else {
		fmt.Printf("%s is already on %s.\n", s.PCI, Driver(s.PCI))
	}

	if s.Unit != "" {
		fmt.Printf("starting %s...\n", s.Unit)
		if err := exec.Command("systemctl", "start", s.Unit).Run(); err != nil {
			return fmt.Errorf("starting %s: %w", s.Unit, err)
		}
		fmt.Println("\nIf your monitor is on this device, switch it to that input.")
	}
	s.Status()
	return nil
}

// Free stops the device's host unit so a VM can claim it.
//
// The check after the stop polls rather than asking once. `systemctl stop`
// returns when the unit is done, but the session's processes are still tearing
// down for a moment after that, so a single immediate check catches a
// gnome-shell that is already exiting and reports it as something that "still
// holds" the device. Only a holder that outlives the grace period is worth a
// warning.
func (s Spec) Free() error {
	if s.Unit == "" {
		fmt.Printf("%s has no host unit to stop.\n", s.PCI)
		return nil
	}
	if err := exec.Command("systemctl", "stop", s.Unit).Run(); err != nil {
		return fmt.Errorf("stopping %s: %w", s.Unit, err)
	}
	if held := s.waitReleased(10 * time.Second); len(held) > 0 {
		fmt.Fprintf(os.Stderr, "warning: these still hold %s after %s stopped:\n", s.PCI, s.Unit)
		for _, h := range held {
			fmt.Fprintf(os.Stderr, "  %s\n", h)
		}
		fmt.Fprintf(os.Stderr, "Starting a VM now would rebind the device out from under them.\n")
	}
	fmt.Printf("%s stopped; %s is free for VMs.\n", s.Unit, s.PCI)
	s.Status()
	return nil
}

// UnitActive reports whether the device's host unit is running, so a caller
// can decide whether freeing is needed before asking for a password.
func (s Spec) UnitActive() bool {
	return s.Unit != "" && run("systemctl", "is-active", s.Unit) == "active"
}

// waitReleased polls until nothing holds one of the device's nodes, returning
// whatever is left when the grace period runs out.
func (s Spec) waitReleased(within time.Duration) []string {
	deadline := time.Now().Add(within)
	for {
		held := s.Holders()
		if len(held) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return held
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Holders reports the processes with one of the device's nodes open, by
// reading /proc rather than shelling out to fuser.
//
// It exists so freeing can say what it is about to kill *before* it stops the
// unit, rather than warning afterwards. Stopping a display manager ends a live
// session; being told which session it was only once it is gone is not much
// of a warning.
//
// Best-effort by construction: without root, /proc/<pid>/fd is unreadable for
// other users' processes, so a short list is not proof that nothing holds the
// device. Callers say "at least these", never "only these".
func (s Spec) Holders() []string {
	nodes := s.DevNodes()
	if len(nodes) == 0 {
		return nil
	}
	var out []string
	procs, _ := filepath.Glob("/proc/[0-9]*")
	for _, p := range procs {
		fds, err := filepath.Glob(p + "/fd/*")
		if err != nil || len(fds) == 0 {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(fd)
			if err != nil || !matchesNode(target, nodes) {
				continue
			}
			name := strings.TrimSpace(readFile(p + "/comm"))
			if name == "" {
				name = filepath.Base(p)
			}
			out = append(out, fmt.Sprintf("%s (pid %s)", name, filepath.Base(p)))
			break
		}
	}
	return out
}

// DevNodes is what a process holding this device would have open: its DRM
// nodes, found through /dev/dri/by-path, and for the NVIDIA driver the
// /dev/nvidia* family, which is not per-device.
func (s Spec) DevNodes() []string {
	var nodes []string
	links, _ := filepath.Glob(devRoot + "/dri/by-path/pci-" + s.PCI + "-*")
	for _, l := range links {
		if target, err := filepath.EvalSymlinks(l); err == nil {
			nodes = append(nodes, target)
		}
	}
	for _, m := range s.Modules {
		if strings.HasPrefix(m, "nvidia") {
			nodes = append(nodes, devRoot+"/nvidia")
			break
		}
	}
	return nodes
}

func matchesNode(target string, nodes []string) bool {
	for _, n := range nodes {
		if target == n || strings.HasPrefix(target, n) {
			return true
		}
	}
	return false
}

// --- sysfs helpers -------------------------------------------------------

// sysfsRoot and devRoot are variables so tests can point the routine at a fake
// tree. Nothing but a test ever changes them.
var (
	sysfsRoot = "/sys/bus/pci/devices"
	usbRoot   = "/sys/bus/usb/devices"
	devRoot   = "/dev"
)

func sysfs(pci, leaf string) string { return filepath.Join(sysfsRoot, pci, leaf) }

// group lists the PCI functions that share the device's IOMMU group and are
// endpoints rather than bridges: what VFIO moved together, so what comes back
// together. Falls back to the device alone when sysfs has no group for it.
func (s Spec) group() []string {
	dir, err := filepath.EvalSymlinks(sysfs(s.PCI, "iommu_group"))
	if err != nil {
		return []string{s.PCI}
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "devices", "*"))
	var out []string
	for _, e := range entries {
		addr := filepath.Base(e)
		if strings.HasPrefix(readFile(filepath.Join(e, "class")), "0x0604") {
			continue // a bridge; never bound to vfio-pci
		}
		out = append(out, addr)
	}
	if len(out) == 0 {
		return []string{s.PCI}
	}
	// The device itself first, so a reset or a probe happens to it before
	// its companions.
	for i, addr := range out {
		if addr == s.PCI && i != 0 {
			out[0], out[i] = out[i], out[0]
		}
	}
	return out
}

// resetDevice issues a function-level reset.
//
// This is the step whose absence cost a working display: a card handed back
// from a guest still holds the state that guest's driver left in it, and on Ada
// the GSP firmware will not boot on top of that. The nvidia module binds
// anyway, so every surface-level check looks healthy — sysfs says
// "driver: nvidia", /dev/nvidia0 exists — while RmInitAdapter has failed and
// there is no usable adapter behind any of it.
//
// Must be called with no driver bound, or the reset races whatever is.
func resetDevice(pci string) error {
	node := sysfs(pci, "reset")
	if _, err := os.Stat(node); err != nil {
		return fmt.Errorf("%s has no reset node; this device cannot be reset "+
			"without a reboot: %w", pci, err)
	}
	if err := os.WriteFile(node, []byte("1"), 0o200); err != nil {
		return fmt.Errorf("resetting %s: %w", pci, err)
	}
	return nil
}

// alive reports whether the driver actually brought the device up, rather
// than merely bound to it: something matches the recipe's pattern under the
// device's sysfs directory. For a display adapter that is a DRM node, which
// is exactly what a display needs; for a USB controller, a bus.
func alive(pci, pattern string) bool {
	matches, _ := filepath.Glob(sysfs(pci, pattern))
	return len(matches) > 0
}

// waitAlive polls, because the node appears a moment after the module loads
// rather than with it.
func waitAlive(pci, pattern string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if alive(pci, pattern) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// kernelComplaint returns what the kernel said about the hardware in the last
// couple of minutes, so a failure to initialise explains itself instead of
// leaving the operator to find it.
func kernelComplaint() string {
	out := run("journalctl", "-k", "--no-pager", "--since", "-2 minutes")
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "NVRM:") || strings.Contains(line, "xhci_hcd") || strings.Contains(line, "vfio-pci") {
			keep = append(keep, "  "+strings.TrimSpace(line))
		}
	}
	if len(keep) > 4 {
		keep = keep[len(keep)-4:]
	}
	return strings.Join(keep, "\n")
}

// Back reports whether a device is already on a host driver and, when the
// recipe names a sign of life, shows it. It reads sysfs only, so it needs no
// privilege: the caller uses it to avoid asking for root to do nothing. Incus
// rebinds some devices itself when they leave a stopped instance (a pci-type
// xHCI came back on xhci_hcd unaided, 2026-09-22). A driver that bound without
// the sign of life is not back: that is the dead-adapter case Return exists for.
func Back(pci, alivePattern string) (driver string, ok bool) {
	driver = Driver(pci)
	if driver == "vfio-pci" || driver == "(none)" {
		return driver, false
	}
	return driver, alivePattern == "" || alive(pci, alivePattern)
}

// Driver names the driver currently bound to a PCI address.
func Driver(pci string) string {
	target, err := os.Readlink(sysfs(pci, "driver"))
	if err != nil {
		return "(none)"
	}
	return filepath.Base(target)
}

// USBPresent reports whether a USB device with this vendor:product id is on
// the host's bus right now. A device redirected into a VM by identity is
// only there to redirect while it is plugged in here — a monitor's KVM on
// another input takes it away from both machines — so "absent from the
// guest" means nothing until this says it is present.
func USBPresent(id string) bool {
	vendor, product, ok := strings.Cut(id, ":")
	if !ok {
		return false
	}
	entries, _ := filepath.Glob(usbRoot + "/*")
	for _, e := range entries {
		if readFile(filepath.Join(e, "idVendor")) == vendor && readFile(filepath.Join(e, "idProduct")) == product {
			return true
		}
	}
	return false
}

// IDs reads a device's vendor and device ids, as "vvvv:dddd", for finding the
// same hardware from inside a guest where the address differs.
func IDs(pci string) string {
	vendor := strings.TrimPrefix(readFile(sysfs(pci, "vendor")), "0x")
	device := strings.TrimPrefix(readFile(sysfs(pci, "device")), "0x")
	if vendor == "" || device == "" {
		return ""
	}
	return vendor + ":" + device
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeSysfs(path, value string) {
	if err := os.WriteFile(path, []byte(value), 0o200); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "warning: writing %s: %v\n", path, err)
	}
}

func run(name string, args ...string) string {
	out, _ := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out))
}
