// Package hostdev moves a device between this host and VM use.
//
// It is the only code in the project that touches the host itself, needs
// root, and can take the display out from under the person running it. The
// privileged half does sysfs, systemctl, and signals to the processes holding
// a device, and nothing else, so the code running as root is as small as it
// can be; discovery, and the guard that refuses while a VM still holds the
// device, happen unprivileged in the caller.
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
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
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
	// A device that resets is parked before its VM starts (see Park), or
	// Incus's own rebind at stop would skip the reset.
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
// A device already on a host driver is left there only when the recipe's sign
// of life is present. Incus rebinds a device to its old driver by itself when
// a VM stops, with no reset and no module reload, so a card can come back on
// nvidia and dead; that one goes through the whole return like any other.
//
// The caller must have established that no instance still holds the device:
// the rebind would otherwise race Incus and both sides lose.
func (s Spec) Return() error {
	driver, back := Back(s.PCI, s.Alive)
	switch {
	case back:
		fmt.Printf("%s is already on %s.\n", s.PCI, driver)
	case driver == "vfio-pci" || driver == "(none)":
		fmt.Printf("returning %s to the host...\n", s.PCI)
	default:
		fmt.Printf("%s is on %s but did not come up (nothing matches %s); returning it again...\n",
			s.PCI, driver, s.Alive)
		// Unbinding a driver from a device something holds blocks until it
		// lets go. Incus checks the same before it takes a card.
		if held := s.Holders(); len(held) > 0 {
			return fmt.Errorf("%s is held by %s, and unbinding it would block until they let go.\n"+
				"End them, then run this again.", s.PCI, strings.Join(held, ", "))
		}
	}
	if !back {
		if err := s.rebind(); err != nil {
			return err
		}
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

// rebind takes the device off whatever driver it is on, resets it, and binds
// the host driver, then checks the device came up.
func (s Spec) rebind() error {
	// Order matters: unload stale modules while the device is still off the
	// host driver, or the reloaded module grabs it mid-transition.
	if len(s.Modules) > 0 {
		run("modprobe", append([]string{"-r"}, s.Modules...)...)
	}
	// Everything in the IOMMU group left for the VM together, so it all
	// comes back together: a card's audio function, most often. A companion
	// Incus already rebound is left where it is.
	members := s.group()
	for _, addr := range members {
		if drv := Driver(addr); drv == "(none)" || (drv != "vfio-pci" && addr != s.PCI) {
			continue
		}
		writeSysfs(sysfs(addr, "driver/unbind"), addr)
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
		writeSysfs(filepath.Join(filepath.Dir(sysfsRoot), "drivers_probe"), addr)
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

// Park binds the device, and everything in its IOMMU group, to vfio-pci
// before a VM claims it.
//
// Incus hands a device back at stop to whatever driver it had when the VM
// started, unreset, and before rig stop can do anything: a card taken from
// nvidia goes back to nvidia with the guest's state still in it. Parked, the
// driver Incus restores is vfio-pci, and the device waits there for Return to
// reset it before the host driver loads.
//
// Unbinding a driver from a device something holds blocks until it lets go,
// so the holders are checked first, as Incus checks before it takes a card;
// kill ends them instead of refusing.
func (s Spec) Park(kill bool) error {
	if s.Parked() {
		fmt.Printf("%s is already on vfio-pci.\n", s.PCI)
		return nil
	}
	if held := s.blockers(); len(held) > 0 {
		nodes := make([]string, 0, len(held))
		for node := range held {
			nodes = append(nodes, node)
		}
		sort.Strings(nodes)
		if !kill {
			var lines []string
			for _, node := range nodes {
				for _, pid := range held[node] {
					lines = append(lines, fmt.Sprintf("  %s: %s", node, Describe(pid)))
				}
			}
			return fmt.Errorf("these hold %s, and unbinding its driver would block until they let go:\n%s\n"+
				"End them, or start with --kill-holders to have rig end them.", s.PCI, strings.Join(lines, "\n"))
		}
		for _, node := range nodes {
			if err := End(node, held[node], 10*time.Second); err != nil {
				return err
			}
		}
	}

	bus := filepath.Dir(sysfsRoot)
	if _, err := os.Stat(filepath.Join(bus, "drivers", "vfio-pci")); err != nil {
		run("modprobe", "vfio-pci")
	}
	members := s.group()
	for _, addr := range members {
		writeSysfs(sysfs(addr, "driver_override"), "vfio-pci")
		if drv := Driver(addr); drv != "(none)" && drv != "vfio-pci" {
			writeSysfs(sysfs(addr, "driver/unbind"), addr)
		}
		writeSysfs(filepath.Join(bus, "drivers_probe"), addr)
	}
	for _, addr := range members {
		if !waitDriver(addr, "vfio-pci", 10*time.Second) {
			return fmt.Errorf("%s did not bind to vfio-pci; it is on %s.", addr, Driver(addr))
		}
	}
	fmt.Printf("parked %s on vfio-pci.\n", strings.Join(members, ", "))
	return nil
}

// Parked reports whether the device and its group are all on vfio-pci, where
// Park puts them and a VM's stop then leaves them. Reads sysfs only.
func (s Spec) Parked() bool {
	for _, addr := range s.group() {
		if Driver(addr) != "vfio-pci" {
			return false
		}
	}
	return true
}

// blockers maps each node whose holders would make unbinding the device's
// driver block to the processes that have it open or mapped: the device's
// own DRM nodes and, on nvidia, its own /dev/nvidiaN, the node Incus checks
// before it takes a card. Narrower than DevNodes on purpose. The rest of the
// /dev/nvidia* family is shared by every card, a daemon such as
// nvidia-persistenced can hold it without holding this one, and Incus takes
// the card regardless, so refusing over it would refuse starts that work.
// Sees other users' processes only as root.
func (s Spec) blockers() map[string][]int {
	var nodes []string
	links, _ := filepath.Glob(devRoot + "/dri/by-path/pci-" + s.PCI + "-*")
	for _, l := range links {
		if target, err := filepath.EvalSymlinks(l); err == nil {
			nodes = append(nodes, target)
		}
	}
	if minor := nvidiaMinor(s.PCI); minor != "" {
		nodes = append(nodes, filepath.Join(devRoot, "nvidia"+minor))
	}
	held := map[string][]int{}
	if len(nodes) == 0 {
		return held
	}
	procs, _ := filepath.Glob("/proc/[0-9]*")
	for _, p := range procs {
		pid, err := strconv.Atoi(filepath.Base(p))
		if err != nil || pid == os.Getpid() {
			continue
		}
		for _, node := range nodes {
			if ok, _ := holdsNode(pid, node); ok {
				held[node] = append(held[node], pid)
			}
		}
	}
	return held
}

// nvidiaMinor is the N of the card's /dev/nvidiaN, from the driver's own
// description of the card, or "" when nvidia does not have it.
func nvidiaMinor(pci string) string {
	for _, line := range strings.Split(readFile(filepath.Join(nvidiaProcRoot, pci, "information")), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Device Minor" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// waitDriver polls until addr is bound to driver, rather than trusting that a
// probe bound it.
func waitDriver(addr, driver string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if Driver(addr) == driver {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
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

// --- ending what holds a device -------------------------------------------

// Process is a host process, described from the parts of /proc anyone may
// read, so a message can say what it is even when what it holds is not ours
// to look at.
type Process struct {
	PID  int
	User string
	Name string // comm
	Args string // the command line, shortened
	Unit string // the systemd unit or scope its cgroup is in
}

// Describe reads what can be known about a process without privilege. One
// that has exited is its PID alone.
func Describe(pid int) Process {
	dir := fmt.Sprintf("/proc/%d", pid)
	p := Process{PID: pid, Name: readFile(dir + "/comm")}
	p.Args = strings.TrimSpace(strings.ReplaceAll(readFile(dir+"/cmdline"), "\x00", " "))
	if len(p.Args) > 60 {
		p.Args = p.Args[:57] + "..."
	}
	for _, line := range strings.Split(readFile(dir+"/status"), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == "Uid:" {
			p.User = "uid " + f[1]
			if u, err := user.LookupId(f[1]); err == nil {
				p.User = u.Username
			}
		}
	}
	// cgroup v2: one line, 0::/user.slice/.../org.gnome.Shell@gdm.service
	_, path, _ := strings.Cut(readFile(dir+"/cgroup"), "::")
	parts := strings.Split(path, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.HasSuffix(parts[i], ".service") || strings.HasSuffix(parts[i], ".scope") {
			p.Unit = parts[i]
			break
		}
	}
	return p
}

func (p Process) String() string {
	if p.Name == "" {
		return fmt.Sprintf("pid %d (gone)", p.PID)
	}
	what := p.Args
	if what == "" {
		what = p.Name
	}
	s := fmt.Sprintf("%s (pid %d", what, p.PID)
	for _, extra := range []string{p.User, p.Unit} {
		if extra != "" {
			s += ", " + extra
		}
	}
	return s + ")"
}

// Visible reports whether this process can see what pid has open. Short of
// root, that is also whether pid is ours to signal, so the caller knows
// before trying whether ending it needs sudo.
func Visible(pid int) bool {
	_, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	return err == nil || errors.Is(err, fs.ErrNotExist)
}

// End ends the processes among pids that have node open or mapped: SIGTERM,
// then SIGKILL for any still holding it after grace. A PID that no longer
// holds node is left alone, so a list gone stale, or a PID reused since it
// was made, costs nothing.
//
// node must be a character device under /dev. The verb that runs this as
// root ends processes for holding a device and for nothing else.
func End(node string, pids []int, grace time.Duration) error {
	if fi, err := os.Stat(node); err != nil || !strings.HasPrefix(node, "/dev/") ||
		filepath.Clean(node) != node || fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("%s is not a device node under /dev", node)
	}
	var targets []int
	for _, pid := range pids {
		if pid <= 1 || pid == os.Getpid() {
			continue
		}
		held, err := holdsNode(pid, node)
		if err != nil {
			return err
		}
		if held {
			targets = append(targets, pid)
		}
	}
	if len(targets) == 0 {
		fmt.Printf("nothing listed holds %s any more.\n", node)
		return nil
	}
	signal := func(pids []int, sig syscall.Signal, verb string) error {
		for _, pid := range pids {
			fmt.Printf("%s %s\n", verb, Describe(pid))
			if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("signalling pid %d: %w", pid, err)
			}
		}
		return nil
	}
	if err := signal(targets, syscall.SIGTERM, "ending"); err != nil {
		return err
	}
	left := waitUnheld(node, targets, grace)
	if len(left) == 0 {
		return nil
	}
	fmt.Printf("still holding %s after %s:\n", node, grace)
	if err := signal(left, syscall.SIGKILL, "  killing"); err != nil {
		return err
	}
	if left = waitUnheld(node, left, 5*time.Second); len(left) > 0 {
		var names []string
		for _, pid := range left {
			names = append(names, Describe(pid).String())
		}
		return fmt.Errorf("still holding %s after SIGKILL: %s", node, strings.Join(names, ", "))
	}
	return nil
}

// holdsNode reports whether a process has node open or mapped. Incus checks
// the maps alone, so a process that closed the node but kept a mapping still
// blocks it. A process that has exited, or been reaped, holds nothing.
func holdsNode(pid int, node string) (bool, error) {
	dir := fmt.Sprintf("/proc/%d", pid)
	fds, err := os.ReadDir(dir + "/fd")
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot see what pid %d has open: %w", pid, err)
	}
	for _, fd := range fds {
		if target, err := os.Readlink(dir + "/fd/" + fd.Name()); err == nil && target == node {
			return true, nil
		}
	}
	maps, err := os.ReadFile(dir + "/maps")
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot see what pid %d has mapped: %w", pid, err)
	}
	for _, line := range strings.Split(string(maps), "\n") {
		if f := strings.Fields(line); len(f) >= 6 && f[5] == node {
			return true, nil
		}
	}
	return false, nil
}

// waitUnheld polls until none of pids holds node, returning those still
// holding it when time runs out. One that can no longer be read counts as
// released: it was readable when End began, so it has exited and its PID may
// belong to someone else now, which is the last thing to send SIGKILL to.
func waitUnheld(node string, pids []int, within time.Duration) []int {
	deadline := time.Now().Add(within)
	for {
		var left []int
		for _, pid := range pids {
			if held, err := holdsNode(pid, node); err == nil && held {
				left = append(left, pid)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// --- sysfs helpers -------------------------------------------------------

// sysfsRoot and devRoot are variables so tests can point the routine at a fake
// tree. Nothing but a test ever changes them.
var (
	sysfsRoot      = "/sys/bus/pci/devices"
	usbRoot        = "/sys/bus/usb/devices"
	devRoot        = "/dev"
	nvidiaProcRoot = "/proc/driver/nvidia/gpus"
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
