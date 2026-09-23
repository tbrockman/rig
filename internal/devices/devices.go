// Package devices enforces one invariant over every host device a VM can be
// given: an exclusive device is configured on at most one instance, and it
// never moves away from a running one.
//
// It began as the GPU package, for one card. The invariant was never about
// the card: any PCI function passed through VFIO has the same silent failure,
// where starting a second VM with the same address hot-unplugs it from the
// first and Incus reports nothing on the victim. So the allocator is generic
// over declared devices, and what makes a device a GPU or a USB controller is
// a few fields of data (see Decl), not a code path.
//
// No state is stored. Incus is the sole source of truth: what an instance
// wants is recorded on it, and who holds what is derived from the instance
// list. The only persistent artefact is a lock file, because "check then act"
// is a race and flock is the smallest thing that fixes it.
package devices

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
	"github.com/tbrockman/rig/internal/policy"
)

const (
	DefaultACL  = "vm-isolate"
	DefaultLock = "/var/lock/rig.lock"
)

type Config struct {
	PCI  string // the card `kind: gpu` without an address means; empty means discover
	ACL  string // required isolation ACL
	Lock string
}

func ConfigFromEnv() Config {
	return Config{
		PCI:  os.Getenv("RIG_PCI"),
		ACL:  envOr("RIG_ACL", DefaultACL),
		Lock: envOr("RIG_LOCK", DefaultLock),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// --- what an instance was given -------------------------------------------

// Decl is one device an instance wants, as recorded on the instance.
//
// It is the manifest's device with its name attached and the return recipe
// carried along, so every later verb — start, stop, doctor, the host-side
// return — has what it needs from Incus alone, without the file in hand.
type Decl struct {
	Name   string  `json:"name"`
	Kind   string  `json:"kind"`
	PCI    string  `json:"pci,omitempty"`
	ID     string  `json:"id,omitempty"`
	Return *Return `json:"return,omitempty"`
}

// Return is how the host takes the device back; see manifest.Return.
type Return struct {
	Modules []string `json:"modules,omitempty"`
	Reset   bool     `json:"reset,omitempty"`
	Alive   string   `json:"alive,omitempty"`
	Unit    string   `json:"unit,omitempty"`
}

// FromManifest turns a declared device into what gets recorded.
func FromManifest(n manifest.Named) Decl {
	d := Decl{Name: n.Name, Kind: n.Kind, PCI: n.PCI, ID: n.ID}
	if n.Return != nil {
		r := Return(*n.Return)
		d.Return = &r
	}
	return d
}

// NvidiaReturn is the recipe for handing an NVIDIA card back to a host that
// drives a display with it: the recipe `rig host desktop` always applied.
func NvidiaReturn(unit string) *Return {
	return &Return{
		Modules: []string{"nvidia_drm", "nvidia_modeset", "nvidia_uvm", "nvidia"},
		Reset:   true,
		Alive:   "drm/card*",
		Unit:    unit,
	}
}

func (d Decl) Exclusive() bool { return manifest.Exclusive(d.Kind) }

// Address is what identifies the device across instances: a PCI address, or
// a USB vendor:product.
func (d Decl) Address() string {
	if d.PCI != "" {
		return d.PCI
	}
	return d.ID
}

// IncusDevice is the device as Incus wants it declared. This map and the
// inverse in addressOf are the whole of what rig knows about each kind.
func (d Decl) IncusDevice() incus.Device {
	switch d.Kind {
	case manifest.KindGPU:
		return incus.Device{"type": "gpu", "gputype": "physical", "pci": d.PCI}
	case manifest.KindPCI:
		return incus.Device{"type": "pci", "address": d.PCI}
	case manifest.KindUSB:
		vendor, product, _ := strings.Cut(d.ID, ":")
		return incus.Device{"type": "usb", "vendorid": vendor, "productid": product}
	}
	return nil
}

// addressOf reads the identity back out of an Incus device, or "?" when it
// has none recorded.
func addressOf(dev incus.Device) string {
	switch dev.Type() {
	case "gpu":
		if dev["pci"] != "" {
			return dev["pci"]
		}
	case "pci":
		if dev["address"] != "" {
			return dev["address"]
		}
	case "usb":
		if dev["vendorid"] != "" {
			return dev["vendorid"] + ":" + dev["productid"]
		}
	}
	return "?"
}

func kindOf(dev incus.Device) string {
	switch dev.Type() {
	case "gpu", "pci", "usb":
		return dev.Type()
	}
	return ""
}

func isPassthrough(dev incus.Device) bool { return kindOf(dev) != "" }

// DevicesKey records what an instance wants, as a JSON list of Decl. No
// record means nothing: a device is only ever given because it was asked for.
const DevicesKey = "user.rig.devices"

// Wanted returns the devices an instance should be given when it starts.
func Wanted(inst *incus.Instance, cfg Config) ([]Decl, error) {
	if raw := inst.Config[DevicesKey]; raw != "" {
		var out []Decl
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return nil, fmt.Errorf("%s on %s is not a device list: %w", DevicesKey, inst.Name, err)
		}
		if out == nil {
			out = []Decl{}
		}
		return out, nil
	}
	return []Decl{}, nil
}

// Record writes the wanted list onto an instance.
func Record(c *incus.Client, name string, decls []Decl) error {
	if decls == nil {
		decls = []Decl{}
	}
	b, err := json.Marshal(decls)
	if err != nil {
		return err
	}
	return c.SetConfigKey(name, DevicesKey, string(b))
}

// Encode renders a wanted list the way Record stores it, for callers that
// build an instance's config up front.
func Encode(decls []Decl) string {
	if decls == nil {
		decls = []Decl{}
	}
	b, _ := json.Marshal(decls)
	return string(b)
}

// --- who holds what --------------------------------------------------------

type Holder struct {
	Instance string `json:"instance"`
	Device   string `json:"device"`
	Status   string `json:"status"`
	Kind     string `json:"kind"`
	PCI      string `json:"pci"` // the address; named pci for the card's sake, where it always is one
}

// Exclusive reports whether the held device is one at most one VM may have.
func (h Holder) Exclusive() bool { return manifest.Exclusive(h.Kind) }

// Holders lists every passthrough device configured on any instance itself,
// not inherited: an inherited one is a profile problem, which CheckProfiles
// reports on its own.
func Holders(instances []incus.Instance) []Holder {
	var out []Holder
	for _, inst := range instances {
		for name, dev := range inst.PassthroughDevices() {
			out = append(out, Holder{
				Instance: inst.Name, Device: name, Status: inst.Status,
				Kind: kindOf(dev), PCI: addressOf(dev),
			})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Instance != out[b].Instance {
			return out[a].Instance < out[b].Instance
		}
		return out[a].Device < out[b].Device
	})
	return out
}

// CheckProfiles refuses to operate while any profile defines an exclusive
// device.
//
// Checked against the profiles themselves, not instances' expanded devices: an
// instance-level device of the same name masks the profile's copy, so scanning
// inherited devices reports all-clear while the profile stays primed to damage
// the next instance created — and misses a poisoned profile that no instance
// uses yet entirely.
func CheckProfiles(c *incus.Client, instances []incus.Instance) error {
	profiles, err := c.Profiles()
	if err != nil {
		return err
	}
	for _, p := range profiles {
		var bad []string
		for name, dev := range p.Devices {
			if isPassthrough(dev) && manifest.Exclusive(kindOf(dev)) {
				bad = append(bad, name)
			}
		}
		if len(bad) == 0 {
			continue
		}
		sort.Strings(bad)
		var users []string
		for _, inst := range instances {
			for _, prof := range inst.Profiles {
				if prof == p.Name {
					users = append(users, inst.Name)
				}
			}
		}
		used := ""
		if len(users) > 0 {
			used = " Currently used by: " + strings.Join(users, ", ") + "."
		}
		return fmt.Errorf("profile %q defines passthrough device(s) %s.%s\n"+
			"Every instance using this profile is configured to grab the same hardware.\n"+
			"Remove it:  incus profile device remove %s %s",
			p.Name, strings.Join(bad, ", "), used, p.Name, bad[0])
	}
	return nil
}

// DiscoverPCI resolves the card's address: an explicit setting, else whoever
// holds a card, else the hardware. The last step matters because the first
// two are empty on a fresh host and straight after a release — which is
// exactly when a new project makes its first claim.
func DiscoverPCI(c *incus.Client, cfg Config) (string, error) {
	if cfg.PCI != "" {
		return cfg.PCI, nil
	}
	if instances, err := c.Instances(); err == nil {
		var cards []Holder
		for _, h := range Holders(instances) {
			if h.Kind == manifest.KindGPU && h.PCI != "?" {
				cards = append(cards, h)
			}
		}
		if len(cards) == 1 {
			return cards[0].PCI, nil
		}
	}
	found, err := displayControllers("0x10de")
	if err != nil {
		return "", err
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("no NVIDIA display controller on the PCI bus. rig drives NVIDIA cards\n" +
			"only — the guest image carries the NVIDIA driver. If the card is there under\n" +
			"another PCI class, name it:  export RIG_PCI=0000:01:00.0")
	default:
		return "", fmt.Errorf("found %d NVIDIA display controllers (%s); this tool assumes one.\n"+
			"  Set it explicitly:  export RIG_PCI=%s",
			len(found), strings.Join(found, ", "), found[0])
	}
}

// displayControllers lists PCI addresses matching a vendor and class 0x0300
// (VGA-compatible display controller), read straight from sysfs.
func displayControllers(vendor string) ([]string, error) {
	entries, err := filepath.Glob(filepath.Join(pciRoot, "*"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, dir := range entries {
		v, err := readTrimmed(filepath.Join(dir, "vendor"))
		if err != nil || v != vendor {
			continue
		}
		class, err := readTrimmed(filepath.Join(dir, "class"))
		if err != nil || !strings.HasPrefix(class, "0x0300") {
			continue
		}
		out = append(out, filepath.Base(dir))
	}
	sort.Strings(out)
	return out, nil
}

// pciRoot is where PCI devices are read from. A variable so tests can point
// it at a fake tree; nothing else changes it.
var pciRoot = "/sys/bus/pci/devices"

// Identify reads what is at a PCI address: its vendor:device, as lspci -nn
// prints it, and its class. Empty when nothing is there.
func Identify(pci string) (id, class string) {
	dir := filepath.Join(pciRoot, pci)
	vendor, err1 := readTrimmed(filepath.Join(dir, "vendor"))
	device, err2 := readTrimmed(filepath.Join(dir, "device"))
	if err1 != nil || err2 != nil {
		return "", ""
	}
	class, _ = readTrimmed(filepath.Join(dir, "class"))
	return strings.TrimPrefix(vendor, "0x") + ":" + strings.TrimPrefix(device, "0x"), class
}

// CheckIdentity refuses an exclusive device whose address holds something
// other than what was declared.
//
// PCI addresses are not names. Removing a device ahead of others on a bus
// renumbers everything behind it, and a BIOS setting is enough to do that:
// on the reference host, disabling the Wi-Fi card moved the chipset USB
// controller from 12:00.0 to 11:00.0 and put the SATA controller at 12:00.0,
// and the next start handed that to an untrusted VM. So the address is
// checked against the declared vendor:device before anything is claimed. A
// card declared without one must at least be an NVIDIA display controller,
// the only kind rig drives; any other device without one is refused.
func CheckIdentity(d Decl) error {
	if !d.Exclusive() || d.PCI == "" {
		return nil
	}
	got, class := Identify(d.PCI)
	if got == "" {
		return fmt.Errorf("%s: nothing is at %s. PCI addresses change when devices are added or removed "+
			"ahead of it, a BIOS setting included.\n  Find it:  lspci -nn -D", d.Name, d.PCI)
	}
	switch {
	case d.ID != "" && got == d.ID:
		return nil
	case d.ID == "" && d.Kind == manifest.KindGPU:
		if strings.HasPrefix(got, "10de:") && strings.HasPrefix(class, "0x03") {
			return nil
		}
		return fmt.Errorf("%s: %s is %s (class %s), not an NVIDIA display controller. "+
			"PCI addresses change when devices are added or removed ahead of it.\n"+
			"  Declare the card's id (lspci -nn) and its address in rig.yaml, then rig apply -f", d.Name, d.PCI, got, class)
	case d.ID == "":
		return fmt.Errorf("%s: %s is %s (class %s), and the record says nothing about what it should be. "+
			"A pci device needs id: vendor:device in rig.yaml; rig apply -f records it.", d.Name, d.PCI, got, class)
	}
	msg := fmt.Sprintf("%s: %s is %s (class %s), not the %s it was declared as. PCI addresses change "+
		"when devices are added or removed ahead of it, a BIOS setting included, and this one has.",
		d.Name, d.PCI, got, class, d.ID)
	if moved := addressesOf(d.ID); len(moved) > 0 {
		msg += fmt.Sprintf("\n  %s is at %s now. If that is the device you mean, update pci: in rig.yaml and rig apply -f.",
			d.ID, strings.Join(moved, ", "))
	} else {
		msg += fmt.Sprintf("\n  No %s is on the bus at all.", d.ID)
	}
	return fmt.Errorf("%s", msg)
}

// addressesOf lists every PCI address holding a vendor:device.
func addressesOf(id string) []string {
	entries, _ := filepath.Glob(filepath.Join(pciRoot, "*"))
	var out []string
	for _, dir := range entries {
		if got, _ := Identify(filepath.Base(dir)); got == id {
			out = append(out, filepath.Base(dir))
		}
	}
	sort.Strings(out)
	return out
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

// --- operations ----------------------------------------------------------

// Claim gives name every device it wants. Refuses to take an exclusive device
// from a running instance, and refuses to add one to a running instance: they
// are not hotpluggable in a VM. A USB device is, and may join a running guest.
func Claim(c *incus.Client, cfg Config, name string) error {
	unlock, err := FileLock(cfg.Lock)
	if err != nil {
		return err
	}
	defer unlock()
	return claimLocked(c, cfg, name)
}

func claimLocked(c *incus.Client, cfg Config, name string) error {
	instances, err := c.Instances()
	if err != nil {
		return err
	}
	if err := CheckProfiles(c, instances); err != nil {
		return err
	}

	var target *incus.Instance
	for i := range instances {
		if instances[i].Name == name {
			target = &instances[i]
		}
	}
	if target == nil {
		return fmt.Errorf("no such instance: %s", name)
	}
	wanted, err := Wanted(target, cfg)
	if err != nil {
		return err
	}
	holders := Holders(instances)

	// Resolve and check everything before claiming anything, so a device
	// that is not what it was declared as leaves nothing moved.
	resolved := make([]Decl, 0, len(wanted))
	for _, d := range wanted {
		if d.Kind == manifest.KindGPU && d.PCI == "" {
			pci, err := DiscoverPCI(c, cfg)
			if err != nil {
				return err
			}
			d.PCI = pci
		}
		if err := CheckIdentity(d); err != nil {
			return fmt.Errorf("%w\nNothing was claimed; %s was not started.", err, name)
		}
		resolved = append(resolved, d)
	}
	for _, d := range resolved {
		if err := claimOne(c, target, holders, d); err != nil {
			return err
		}
	}
	return nil
}

func claimOne(c *incus.Client, target *incus.Instance, holders []Holder, d Decl) error {
	var mine, others []Holder
	for _, h := range holders {
		if h.PCI != d.Address() {
			continue
		}
		if h.Instance == target.Name {
			mine = append(mine, h)
		} else {
			others = append(others, h)
		}
	}
	if len(mine) > 0 {
		fmt.Printf("%s already holds %s (%s).\n", target.Name, d.Name, d.Address())
		return nil
	}
	if d.Exclusive() {
		if len(others) > 1 {
			return fmt.Errorf("%s is configured on multiple instances; run `rig release` first", d.Address())
		}
		if len(others) == 1 && others[0].Status == "Running" {
			return fmt.Errorf("%s (%s) is held by %s, which is RUNNING.\n"+
				"Detaching it would hot-unplug the device from a live workload.\n"+
				"Stop it first:  rig stop %s", d.Name, d.Address(), others[0].Instance, others[0].Instance)
		}
		if target.Running() {
			return fmt.Errorf("%s is running; a %s device cannot be hotplugged into a VM.\n"+
				"Stop it first:  rig stop %s", target.Name, d.Kind, target.Name)
		}
		if len(others) == 1 {
			prev := others[0]
			inst, _, err := c.Instance(prev.Instance)
			if err != nil {
				return err
			}
			devices := cloneDevices(inst.Devices)
			delete(devices, prev.Device)
			if err := c.SetDevices(prev.Instance, devices); err != nil {
				return err
			}
			fmt.Printf("detached %s (%s) from %s\n", prev.Device, prev.PCI, prev.Instance)
		}
	}

	// Re-read: the target may have been rewritten by an earlier claim in this
	// same pass, and SetDevices replaces the whole map.
	inst, _, err := c.Instance(target.Name)
	if err != nil {
		return err
	}
	devices := cloneDevices(inst.Devices)
	devices[d.Name] = d.IncusDevice()
	if err := c.SetDevices(target.Name, devices); err != nil {
		return err
	}
	fmt.Printf("attached %s (%s %s) to %s\n", d.Name, d.Kind, d.Address(), target.Name)
	return nil
}

// Detached is a device taken off an instance, with the recipe for handing it
// back to the host when the instance's record has one.
type Detached struct {
	Name    string
	Kind    string
	Address string
	Return  *Return
}

// Detach removes every passthrough device from one instance. Refuses while it
// runs unless forced: that is a hot-unplug, and it breaks the workload.
func Detach(c *incus.Client, cfg Config, name string, force bool) ([]Detached, error) {
	unlock, err := FileLock(cfg.Lock)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return detachLocked(c, cfg, name, force)
}

func detachLocked(c *incus.Client, cfg Config, name string, force bool) ([]Detached, error) {
	inst, _, err := c.Instance(name)
	if err != nil {
		return nil, err
	}
	held := inst.PassthroughDevices()
	if len(held) == 0 {
		return nil, nil
	}
	if inst.Running() && !force {
		return nil, fmt.Errorf("%s is RUNNING. Stop it first, or pass --force to "+
			"hot-unplug (this WILL break its workload).", name)
	}
	wanted, _ := Wanted(inst, cfg)
	recipes := map[string]*Return{}
	for _, d := range wanted {
		recipes[d.Name] = d.Return
	}

	devices := cloneDevices(inst.Devices)
	var out []Detached
	for _, devName := range sortedKeys(held) {
		dev := held[devName]
		delete(devices, devName)
		out = append(out, Detached{Name: devName, Kind: kindOf(dev), Address: addressOf(dev), Return: recipes[devName]})
	}
	if err := c.SetDevices(name, devices); err != nil {
		return nil, err
	}
	for _, d := range out {
		fmt.Printf("detached %s (%s %s) from %s\n", d.Name, d.Kind, d.Address, name)
	}
	return out, nil
}

// Release detaches every exclusive device from every instance holding one.
func Release(c *incus.Client, cfg Config, force bool) ([]Detached, error) {
	unlock, err := FileLock(cfg.Lock)
	if err != nil {
		return nil, err
	}
	defer unlock()

	instances, err := c.Instances()
	if err != nil {
		return nil, err
	}
	holders := Holders(instances)
	var holding []string
	for _, h := range holders {
		if !h.Exclusive() {
			continue
		}
		if h.Status == "Running" && !force {
			return nil, fmt.Errorf("%s is RUNNING. Stop it first, or pass --force to "+
				"hot-unplug (this WILL break its workload).", h.Instance)
		}
		holding = append(holding, h.Instance)
	}
	if len(holding) == 0 {
		fmt.Println("no exclusive device is assigned.")
		return nil, nil
	}
	var out []Detached
	for _, name := range dedupe(holding) {
		d, err := detachLocked(c, cfg, name, force)
		if err != nil {
			return out, err
		}
		out = append(out, d...)
	}
	return out, nil
}

// Start claims what the instance wants and starts it. The isolation check
// runs before the claim so a refusal does not leave anything moved.
func Start(c *incus.Client, cfg Config, name string, allowUnisolated bool, timeoutSec int) error {
	inst, _, err := c.Instance(name)
	if err != nil {
		return err
	}
	unisolated, noEgress := policy.Report(inst, cfg.ACL)
	if len(unisolated) > 0 && !allowUnisolated {
		return fmt.Errorf("%s has no network isolation: NIC %s does not carry the %q ACL.\n"+
			"An unisolated guest reaches this host's sshd on every address the host holds, "+
			"plus the LAN and any overlay network (Tailscale, WireGuard) the host is on.\n"+
			"Fix it for every instance:  rig setup\n"+
			"To start anyway:  rig start --allow-unisolated %s",
			name, strings.Join(unisolated, ", "), cfg.ACL, name)
	}
	for _, nic := range noEgress {
		fmt.Fprintf(os.Stderr, "warning: NIC %s has %s attached but its egress default is not "+
			"'allow'. Attaching an ACL makes Incus default to reject, so this guest will have "+
			"no internet at all.\n", nic, cfg.ACL)
	}

	// A VM that wants nothing must take nothing. Without this rule every start
	// claimed the card, which on a host whose desktop is driving it meant
	// starting any project VM killed the display.
	wanted, err := Wanted(inst, cfg)
	if err != nil {
		return err
	}
	if len(wanted) > 0 {
		unlock, err := FileLock(cfg.Lock)
		if err != nil {
			return err
		}
		defer unlock()

		if err := claimLocked(c, cfg, name); err != nil {
			return err
		}
	}
	if inst, _, err = c.Instance(name); err != nil {
		return err
	}
	if inst.Running() {
		fmt.Printf("%s already running.\n", name)
		return nil
	}
	if err := c.SetState(name, "start", timeoutSec); err != nil {
		return err
	}
	fmt.Printf("started %s\n", name)
	return nil
}

// Stop stops the instance and nothing else. What it held is still configured
// on it afterwards; Detach is the caller's next step, or not.
func Stop(c *incus.Client, cfg Config, name string, timeoutSec int) error {
	return StopForce(c, cfg, name, timeoutSec, false)
}

// StopForce is Stop with the option of not asking the guest. A clean stop is
// an ACPI power-button press, and a guest running a desktop session answers
// that by asking the person at the screen, who is not there; after the
// timeout Incus reports "Failed shutting down instance" and the VM is still
// up. Force skips the request, at the cost of whatever the guest had not yet
// written.
func StopForce(c *incus.Client, cfg Config, name string, timeoutSec int, force bool) error {
	unlock, err := FileLock(cfg.Lock)
	if err != nil {
		return err
	}
	defer unlock()

	inst, _, err := c.Instance(name)
	if err != nil {
		return err
	}
	if inst.Stopped() {
		fmt.Printf("%s already stopped.\n", name)
		return nil
	}
	if force {
		if err := c.ForceStop(name, timeoutSec); err != nil {
			return err
		}
		fmt.Printf("stopped %s (forced)\n", name)
		return nil
	}
	gracefulErr := c.SetState(name, "stop", timeoutSec)
	if gracefulErr == nil {
		fmt.Printf("stopped %s\n", name)
		return nil
	}
	// The clean path failed. If the guest is simply still up, it ignored the
	// power button — a desktop session does that — and the alternative to
	// pulling the plug is a VM that stays running with the host's devices
	// inside it, which is worse than lost guest state. Anything else is a
	// real error and is reported as one.
	inst, _, err = c.Instance(name)
	if err != nil || !inst.Running() {
		return gracefulErr
	}
	fmt.Fprintf(os.Stderr, "warning: %s did not shut down within %ds (%v); pulling the plug\n",
		name, timeoutSec, gracefulErr)
	if err := c.ForceStop(name, timeoutSec); err != nil {
		return fmt.Errorf("forced stop after the clean one timed out: %w", err)
	}
	fmt.Printf("stopped %s (forced after the clean shutdown timed out)\n", name)
	return nil
}

// FileLock takes an exclusive advisory lock, waiting (loudly) if held.
func FileLock(path string) (func(), error) {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o664)
	if err != nil {
		return nil, fmt.Errorf("cannot open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(os.Stderr, "waiting for the rig lock...")
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, err
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func cloneDevices(in map[string]incus.Device) map[string]incus.Device {
	out := make(map[string]incus.Device, len(in))
	for name, dev := range in {
		copied := make(incus.Device, len(dev))
		for k, v := range dev {
			copied[k] = v
		}
		out[name] = copied
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
