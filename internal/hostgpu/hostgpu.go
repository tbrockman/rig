// Package hostgpu moves the card between host desktop use and VM use.
//
// It was a separate binary once. The split encoded something real — this is the
// only code in the project that touches the host itself, needs root, and can
// take the display out from under the person running it — but it made the tool
// look optional, when reclaiming the card is a necessary step in the ordinary
// rig lifecycle. It is now `rig host`, which re-execs itself under sudo for the
// two verbs that write to sysfs.
//
// Everything here is deliberately incus-free: discovery, and the guard that
// refuses while a VM still holds the card, happen unprivileged in the caller.
// The privileged half does sysfs and systemctl and nothing else, so the code
// running as root is as small as it can be.
//
// Assumes the host has another GPU — an iGPU, typically — that can drive the
// console, so this card can leave for a VM and come back without taking the
// only display with it.
package hostgpu

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Conf is one card and the display manager that competes with it for the card.
type Conf struct {
	GPUPCI   string // e.g. 0000:04:00.0
	AudioPCI string // the same device at function 1; shares an IOMMU group
	DM       string // display manager unit, e.g. gdm
}

// AudioFunction returns the audio function that must move with a GPU. They
// share an IOMMU group, so passing one without the other does not work.
func AudioFunction(gpuPCI string) string {
	return strings.TrimSuffix(gpuPCI, ".0") + ".1"
}

// Status prints where the card is. Reads only; needs no privilege.
func (c Conf) Status() {
	fmt.Printf("GPU   %s   driver: %s\n", c.GPUPCI, Driver(c.GPUPCI))
	fmt.Printf("audio %s   driver: %s\n", c.AudioPCI, Driver(c.AudioPCI))
	fmt.Printf("override: %s\n", readFile(sysfs(c.GPUPCI, "driver_override")))
	fmt.Printf("%s: %s\n", c.DM, run("systemctl", "is-active", c.DM))
}

// Desktop rebinds the card to the host driver and starts the desktop.
//
// The caller must have established that no instance still holds the card: the
// rebind would otherwise race Incus and both sides lose.
func (c Conf) Desktop() error {
	if Driver(c.GPUPCI) == "vfio-pci" {
		fmt.Println("rebinding to nvidia...")
		// Order matters: unload stale modules while the card is still on
		// vfio-pci, or the reloaded module grabs it mid-transition and mismatches.
		run("modprobe", "-r", "nvidia_drm", "nvidia_modeset", "nvidia_uvm", "nvidia")
		writeSysfs("/sys/bus/pci/drivers/vfio-pci/unbind", c.GPUPCI)
		writeSysfs("/sys/bus/pci/drivers/vfio-pci/unbind", c.AudioPCI)
		for _, addr := range []string{c.GPUPCI, c.AudioPCI} {
			writeSysfs(sysfs(addr, "driver_override"), "\n")
		}

		// Reset before the driver loads, not after. With no driver bound this
		// is the only moment the card can be cleared of what the guest left in
		// it, and on Ada the GSP firmware will not boot without it.
		fmt.Println("resetting the card...")
		if err := resetDevice(c.GPUPCI); err != nil {
			return fmt.Errorf("%w\n"+
				"Without a reset the driver will bind but the adapter will not "+
				"initialise, which looks like a working card with no display.\n"+
				"A reboot clears it.", err)
		}

		if out := run("modprobe", "nvidia"); out != "" {
			fmt.Println(out)
		}
		writeSysfs("/sys/bus/pci/drivers_probe", c.AudioPCI)

		// Binding is not working. Check the thing a display actually needs.
		if !waitAdapter(c.GPUPCI, 15*time.Second) {
			msg := fmt.Sprintf("the nvidia driver bound to %s but the adapter did "+
				"not come up: no DRM node, so nothing will appear on DisplayPort.",
				c.GPUPCI)
			if why := nvrmComplaint(); why != "" {
				msg += "\n\nThe kernel said:\n" + why
			}
			msg += "\n\nA reboot clears a GPU that FLR could not."
			return fmt.Errorf("%s", msg)
		}
	}

	fmt.Println("starting desktop...")
	if err := exec.Command("systemctl", "start", c.DM).Run(); err != nil {
		return fmt.Errorf("starting %s: %w", c.DM, err)
	}
	fmt.Println("\nIf your monitor is on this card, switch it to that input.")
	fmt.Println("(The console on the other GPU stays available on tty1.)")
	c.Status()
	return nil
}

// Headless stops the desktop and frees the card for VMs.
//
// The check after the stop polls rather than asking once. `systemctl stop`
// returns when the unit is done, but the session's processes are still tearing
// down for a moment after that, so a single immediate check catches a
// gnome-shell that is already exiting and reports it as something that "still
// holds" the card. That reads as a refusal to release and sends the operator
// looking for something to kill, when waiting two seconds would have answered
// it. Only a holder that outlives the grace period is worth a warning.
func (c Conf) Headless() error {
	if err := exec.Command("systemctl", "stop", c.DM).Run(); err != nil {
		return fmt.Errorf("stopping %s: %w", c.DM, err)
	}
	if held := waitCardReleased(10 * time.Second); len(held) > 0 {
		fmt.Fprintf(os.Stderr, "warning: these still hold /dev/nvidia* after %s stopped:\n", c.DM)
		for _, h := range held {
			fmt.Fprintf(os.Stderr, "  %s\n", h)
		}
		fmt.Fprintf(os.Stderr, "Starting a VM now would rebind the card out from under them.\n")
	}
	fmt.Println("desktop stopped; card is free for VMs.")
	c.Status()
	return nil
}

// waitCardReleased polls until nothing holds a /dev/nvidia* node, returning
// whatever is left when the grace period runs out.
func waitCardReleased(within time.Duration) []string {
	deadline := time.Now().Add(within)
	for {
		held := Holders()
		if len(held) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return held
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Holders reports the processes with a /dev/nvidia* node open, by reading
// /proc rather than shelling out to fuser or nvidia-smi.
//
// It exists so `rig host headless` can say what it is about to kill *before* it
// stops the display manager, rather than warning afterwards. Stopping gdm ends
// a live session; being told which session it was only once it is gone is not
// much of a warning.
//
// Best-effort by construction: without root, /proc/<pid>/fd is unreadable for
// other users' processes, so a short list is not proof that nothing holds the
// card. Callers say "at least these", never "only these".
func Holders() []string {
	var out []string
	procs, _ := filepath.Glob("/proc/[0-9]*")
	for _, p := range procs {
		fds, err := filepath.Glob(p + "/fd/*")
		if err != nil || len(fds) == 0 {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(fd)
			if err != nil || !strings.HasPrefix(target, "/dev/nvidia") {
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

// --- sysfs helpers -------------------------------------------------------

// sysfsRoot is a variable so tests can point the reset and readiness checks at
// a fake tree. Nothing but a test ever changes it.
var sysfsRoot = "/sys/bus/pci/devices"

func sysfs(pci, leaf string) string { return filepath.Join(sysfsRoot, pci, leaf) }

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
		return fmt.Errorf("%s has no reset node; this card cannot be reset "+
			"without a reboot: %w", pci, err)
	}
	if err := os.WriteFile(node, []byte("1"), 0o200); err != nil {
		return fmt.Errorf("resetting %s: %w", pci, err)
	}
	return nil
}

// adapterReady reports whether the driver actually brought the GPU up, rather
// than merely bound to it. A DRM node is the right signal because it is exactly
// what a display needs: no node, no DisplayPort output, whatever lspci says.
func adapterReady(pci string) bool {
	cards, _ := filepath.Glob(sysfs(pci, "drm/card*"))
	return len(cards) > 0
}

// waitAdapter polls, because the DRM node appears a moment after the module
// loads rather than with it.
func waitAdapter(pci string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if adapterReady(pci) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// nvrmComplaint returns what the kernel said about the GPU, so a failure to
// initialise explains itself instead of leaving the operator to find it.
func nvrmComplaint() string {
	out := run("journalctl", "-k", "--no-pager", "--since", "-2 minutes")
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "NVRM:") {
			keep = append(keep, "  "+strings.TrimSpace(line))
		}
	}
	if len(keep) > 4 {
		keep = keep[len(keep)-4:]
	}
	return strings.Join(keep, "\n")
}

// Driver names the driver currently bound to a PCI address.
func Driver(pci string) string {
	target, err := os.Readlink(sysfs(pci, "driver"))
	if err != nil {
		return "(none)"
	}
	return filepath.Base(target)
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
