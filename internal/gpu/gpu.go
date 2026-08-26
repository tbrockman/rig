// Package gpu enforces one invariant: at most one instance has the GPU device
// configured, and it never moves away from a running instance.
//
// No state is stored. Incus is the sole source of truth; everything here is
// derived from the instance list. The only persistent artefact is a lock file,
// because "check then act" is a race and flock is the smallest thing that fixes
// it.
package gpu

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"rig/internal/incus"
	"rig/internal/policy"
)

const (
	DefaultDevice = "gpu0"
	DefaultACL    = "vm-isolate"
	DefaultLock   = "/var/lock/rig.lock"
)

type Config struct {
	Device string // device name on the instance
	PCI    string // e.g. 0000:04:00.0; empty means discover
	ACL    string // required isolation ACL
	Lock   string
}

func ConfigFromEnv() Config {
	return Config{
		Device: envOr("RIG_DEVICE", DefaultDevice),
		PCI:    os.Getenv("RIG_PCI"),
		ACL:    envOr("RIG_ACL", DefaultACL),
		Lock:   envOr("RIG_LOCK", DefaultLock),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type Holder struct {
	Instance string `json:"instance"`
	Device   string `json:"device"`
	Status   string `json:"status"`
	PCI      string `json:"pci"`
}

func Holders(instances []incus.Instance) []Holder {
	var out []Holder
	for _, inst := range instances {
		for name, dev := range inst.GPUDevices() {
			pci := dev["pci"]
			if pci == "" {
				pci = "?"
			}
			out = append(out, Holder{Instance: inst.Name, Device: name, Status: inst.Status, PCI: pci})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Instance < out[b].Instance })
	return out
}

// CheckProfiles refuses to operate while any profile defines a GPU device.
//
// Checked against the profiles themselves, not instances' expanded devices: an
// instance-level device of the same name (gpu0, which is exactly what this tool
// creates) masks the profile's copy, so scanning inherited devices reports
// all-clear while the profile stays primed to damage the next instance created
// — and misses a poisoned profile that no instance uses yet entirely.
func CheckProfiles(c *incus.Client, instances []incus.Instance) error {
	profiles, err := c.Profiles()
	if err != nil {
		return err
	}
	for _, p := range profiles {
		var gpus []string
		for name, dev := range p.Devices {
			if dev.Type() == "gpu" {
				gpus = append(gpus, name)
			}
		}
		if len(gpus) == 0 {
			continue
		}
		sort.Strings(gpus)
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
		return fmt.Errorf("profile %q defines GPU device(s) %s.%s\n"+
			"Every instance using this profile is configured to grab the same card.\n"+
			"Remove it:  incus profile device remove %s %s",
			p.Name, strings.Join(gpus, ", "), used, p.Name, gpus[0])
	}
	return nil
}

// DiscoverPCI resolves the card's address: an explicit setting, else whoever
// holds it, else the hardware. The last step matters because the first two are
// empty on a fresh host and straight after a release — which is exactly when a
// new project makes its first claim.
func DiscoverPCI(c *incus.Client, cfg Config) (string, error) {
	if cfg.PCI != "" {
		return cfg.PCI, nil
	}
	if instances, err := c.Instances(); err == nil {
		if h := Holders(instances); len(h) == 1 && h[0].PCI != "?" {
			return h[0].PCI, nil
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
		return "", fmt.Errorf("no NVIDIA display controller on the PCI bus.\n" +
			"  Set it explicitly:  export RIG_PCI=0000:04:00.0")
	default:
		return "", fmt.Errorf("found %d NVIDIA display controllers (%s); this tool assumes one.\n"+
			"  Set it explicitly:  export RIG_PCI=%s",
			len(found), strings.Join(found, ", "), found[0])
	}
}

// displayControllers lists PCI addresses matching a vendor and class 0x0300
// (VGA-compatible display controller), read straight from sysfs.
func displayControllers(vendor string) ([]string, error) {
	entries, err := filepath.Glob("/sys/bus/pci/devices/*")
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

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

// --- operations ----------------------------------------------------------

// Claim moves the GPU to name. Refuses to take it from a running instance, and
// refuses to add it to a running one: GPU devices are not hotpluggable for VMs.
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

	holders := Holders(instances)
	if len(holders) > 1 {
		return fmt.Errorf("GPU is configured on multiple instances; run `rig release` first")
	}
	if len(holders) == 1 && holders[0].Instance == name {
		fmt.Printf("%s already holds the GPU.\n", name)
		return nil
	}
	if len(holders) == 1 && holders[0].Status == "Running" {
		return fmt.Errorf("GPU is held by %s, which is RUNNING.\n"+
			"Detaching it would hot-unplug the card from a live workload.\n"+
			"Stop it first:  rig stop %s", holders[0].Instance, holders[0].Instance)
	}
	if target.Running() {
		return fmt.Errorf("%s is running; GPU devices cannot be hotplugged into a VM.\n"+
			"Stop it first:  rig stop %s", name, name)
	}

	pci, err := DiscoverPCI(c, cfg)
	if err != nil {
		return err
	}

	if len(holders) == 1 {
		prev := holders[0]
		inst, _, err := c.Instance(prev.Instance)
		if err != nil {
			return err
		}
		devices := cloneDevices(inst.Devices)
		delete(devices, prev.Device)
		if err := c.SetDevices(prev.Instance, devices); err != nil {
			return err
		}
		fmt.Printf("detached GPU from %s\n", prev.Instance)
	}

	devices := cloneDevices(target.Devices)
	devices[cfg.Device] = incus.Device{"type": "gpu", "gputype": "physical", "pci": pci}
	if err := c.SetDevices(name, devices); err != nil {
		return err
	}
	fmt.Printf("attached GPU (%s) to %s\n", pci, name)
	return nil
}

func Release(c *incus.Client, cfg Config, force bool) error {
	unlock, err := FileLock(cfg.Lock)
	if err != nil {
		return err
	}
	defer unlock()

	instances, err := c.Instances()
	if err != nil {
		return err
	}
	holders := Holders(instances)
	if len(holders) == 0 {
		fmt.Println("GPU is already unassigned.")
		return nil
	}
	for _, h := range holders {
		if h.Status == "Running" && !force {
			return fmt.Errorf("%s is RUNNING. Stop it first, or pass --force to "+
				"hot-unplug (this WILL break its workload).", h.Instance)
		}
	}
	for _, h := range holders {
		inst, _, err := c.Instance(h.Instance)
		if err != nil {
			return err
		}
		devices := cloneDevices(inst.Devices)
		delete(devices, h.Device)
		if err := c.SetDevices(h.Instance, devices); err != nil {
			return err
		}
		fmt.Printf("detached GPU from %s\n", h.Instance)
	}
	return nil
}

// Start claims the card and starts the instance. The isolation check runs
// before the claim so a refusal does not leave the card moved.
func Start(c *incus.Client, cfg Config, name string, allowUnisolated, withGPU bool, timeoutSec int) error {
	inst, _, err := c.Instance(name)
	if err != nil {
		return err
	}
	unisolated, noEgress := policy.Report(inst, cfg.ACL)
	if len(unisolated) > 0 && !allowUnisolated {
		return fmt.Errorf("%s has no network isolation: NIC %s does not carry the %q ACL.\n"+
			"An unisolated guest reaches this host's sshd on every address the host holds, "+
			"plus the LAN and the tailnet.\n"+
			"Fix it for every instance:  rig apply\n"+
			"To start anyway:  rig start --allow-unisolated %s",
			name, strings.Join(unisolated, ", "), cfg.ACL, name)
	}
	for _, nic := range noEgress {
		fmt.Fprintf(os.Stderr, "warning: NIC %s has %s attached but its egress default is not "+
			"'allow'. Attaching an ACL makes Incus default to reject, so this guest will have "+
			"no internet at all.\n", nic, cfg.ACL)
	}

	// A VM that does not want the card must not take it. Without this every
	// start claims the GPU, which on a host whose desktop is driving it means
	// starting any project VM kills the display — an expensive surprise for a
	// project that never needed the card in the first place.
	if withGPU {
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

func Stop(c *incus.Client, cfg Config, name string, timeoutSec int) error {
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
	if err := c.SetState(name, "stop", timeoutSec); err != nil {
		return err
	}
	fmt.Printf("stopped %s (GPU still attached to it)\n", name)
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
