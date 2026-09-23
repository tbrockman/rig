package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/tbrockman/rig/internal/cpuset"
	"github.com/tbrockman/rig/internal/creds"
	"github.com/tbrockman/rig/internal/devices"
	"github.com/tbrockman/rig/internal/hostdev"
	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
	"github.com/tbrockman/rig/internal/ports"
	"github.com/tbrockman/rig/internal/volumes"
)

// The manifest is declared intent; Incus is what is true. These helpers turn
// one into the other and report where they differ. Nothing here is the only
// copy of anything: what `rig new -f` reads from the file it records on the
// instance, so every later verb works from Incus alone and the file is
// consulted again only to reconcile or to report drift.

// manifestKey records the file an instance was made from, so `rig doctor` can
// find it without being told. portsKey records the published ports, as the
// manifest's list in JSON, so the network reconciliation has the intent to
// hand.
const (
	manifestKey = "user.rig.manifest"
	portsKey    = "user.rig.ports"
)

// desired is the instance config a manifest asks for: every key rig records,
// plus the limits Incus enforces. Diffing this against an instance's config is
// what both `apply -f` and `doctor` do.
func desired(m *manifest.Manifest) (map[string]string, error) {
	absEnv, err := absEnvFile(m.Resolve(m.Guest.EnvFile))
	if err != nil {
		return nil, err
	}
	var decls []devices.Decl
	for _, n := range m.Wanted() {
		decls = append(decls, devices.FromManifest(n))
	}
	ports, _ := json.Marshal(portStrings(m))
	cfg := map[string]string{
		managedKey:         "true",
		imageKey:           m.ImageAlias(),
		devices.DevicesKey: devices.Encode(decls),
		manifestKey:        m.Path,
		portsKey:           string(ports),
		// Always present, empty when unasked: removing the line from the file
		// must read as drift, not as nothing to say.
		inputKey: m.Guest.Input,
	}
	if absEnv != "" {
		cfg[creds.InstanceKey] = absEnv
	}
	if limit := m.Guest.CPUs.Limit(); limit != "" {
		cfg["limits.cpu"] = limit
	}
	if m.Guest.Memory != "" {
		cfg["limits.memory"] = m.Guest.Memory
	}
	return cfg, nil
}

func portStrings(m *manifest.Manifest) []string {
	out := make([]string, 0, len(m.Guest.Ports))
	for _, p := range m.Guest.Ports {
		out = append(out, p.String())
	}
	return out
}

// configDrift lists the keys where an instance differs from what its manifest
// asks for, as "key: have -> want" lines.
func configDrift(inst *incus.Instance, want map[string]string) []string {
	var out []string
	for _, key := range sortedKeys(want) {
		if key == manifestKey {
			continue // the file naming itself is not drift
		}
		have := inst.Config[key]
		if have == want[key] {
			continue
		}
		if have == "" {
			have = "<unset>"
		}
		out = append(out, fmt.Sprintf("%s: %s -> %s", key, have, firstNonEmpty(want[key], "<unset>")))
	}
	return out
}

// ensureImage makes the manifest's image exist: a `build:` is built when its
// alias is missing, an `image:` has to be there already.
func (a *app) ensureImage(m *manifest.Manifest) error {
	alias := m.ImageAlias()
	if a.c.ImageExists(alias) {
		return nil
	}
	if m.Guest.Build == "" {
		return fmt.Errorf("no such image: %s\n  Build it:  rig image build --alias %s", alias, alias)
	}
	dir := m.Resolve(m.Guest.Build)
	note("no image %s yet; building it from %s", alias, dir)
	return a.buildImage(dir, true, envOr("RIG_FLAKE_ATTR", "gpubase"), alias, false)
}

// newFromManifest creates the instance the file describes.
func (a *app) newFromManifest(m *manifest.Manifest, profile string) error {
	name := m.Guest.Name
	if a.c.Exists(name) {
		return fmt.Errorf("%s already exists.\n  Reconcile it to the file instead:  rig apply -f %s", name, m.Path)
	}
	if err := checkCPUs(m); err != nil {
		return err
	}
	if err := a.ensureImage(m); err != nil {
		return err
	}
	config, err := desired(m)
	if err != nil {
		return err
	}
	// CreateVM sets the limits from CreateOpts; the config map is for what
	// rig records. Passing both is harmless and keeps one source.
	if err := a.c.CreateVM(incus.CreateOpts{
		Name: name, Image: m.ImageAlias(), Profile: profile,
		CPUs: m.Guest.CPUs.Count, CPUSet: m.Guest.CPUs.Set, Memory: m.Guest.Memory, DiskSize: m.Guest.Disk, Config: config,
	}); err != nil {
		return err
	}
	if m.Guest.Network == manifest.NetworkNone {
		inst, _, err := a.c.Instance(name)
		if err != nil {
			return err
		}
		changes, err := a.reconcileNetwork(inst, true, false)
		for _, ch := range changes {
			note("%s", ch)
		}
		if err != nil {
			return fmt.Errorf("removing the network device: %w", err)
		}
	}
	if len(m.Guest.Volumes) > 0 {
		inst, _, err := a.c.Instance(name)
		if err != nil {
			return err
		}
		changes, err := volumes.Reconcile(a.c, inst, m.Guest.Volumes, false)
		for _, ch := range changes {
			note("%s", ch)
		}
		if err != nil {
			return fmt.Errorf("volumes: %w", err)
		}
	}
	if len(m.Guest.Ports) > 0 {
		inst, _, err := a.c.Instance(name)
		if err != nil {
			return err
		}
		changes, err := ports.Reconcile(a.c, a.cfg.ACL, inst, m.Guest.Ports, false)
		for _, ch := range changes {
			note("%s", ch)
		}
		if err != nil {
			return fmt.Errorf("publishing ports: %w", err)
		}
	}
	return nil
}

// reconcileManifest brings an existing instance's record to the file. Limits
// on a running VM are recorded but take effect at the next start, and the
// caller is told so.
func (a *app) reconcileManifest(m *manifest.Manifest) ([]string, error) {
	name := m.Guest.Name
	inst, _, err := a.c.Instance(name)
	if err != nil {
		return nil, fmt.Errorf("no instance %s yet.\n  Create it from the file:  rig new -f %s", name, m.Path)
	}
	if err := checkCPUs(m); err != nil {
		return nil, err
	}
	want, err := desired(m)
	if err != nil {
		return nil, err
	}
	var changes []string
	for _, key := range sortedKeys(want) {
		if inst.Config[key] == want[key] {
			continue
		}
		if inst.Running() && strings.HasPrefix(key, "limits.") {
			changes = append(changes, fmt.Sprintf("%s: %s -> %s (deferred: %s is running; takes effect at the next start)",
				key, firstNonEmpty(inst.Config[key], "<unset>"), want[key], name))
			continue
		}
		if err := a.c.SetConfigKey(name, key, want[key]); err != nil {
			return changes, err
		}
		changes = append(changes, key+": "+firstNonEmpty(inst.Config[key], "<unset>")+" -> "+firstNonEmpty(want[key], "<unset>"))
	}
	if inst.Running() && inst.Config[devices.DevicesKey] != want[devices.DevicesKey] {
		changes = append(changes, "note: a running VM keeps the devices it started with; rig restart "+name+" to hand it the new set")
	}
	if inst.Running() && inst.Config[inputKey] != want[inputKey] {
		if want[inputKey] == manifest.InputHost {
			changes = append(changes, "note: input is lent from the next start; for this run:  rig host input "+name+" --background")
		} else if inputActive(name) {
			changes = append(changes, "note: input stays lent until "+name+" stops; sooner:  rig host input "+name+" --stop")
		}
	}
	// The network device and the ports, in an order that matters. A mask is
	// lifted before ports are published, because they need the NIC back; it
	// is put on after ports are withdrawn, because withdrawing them removes
	// their NIC override and per-instance ACL, which masking first would leave
	// behind with nothing attached.
	none := m.Guest.Network == manifest.NetworkNone
	if !none {
		if inst, _, err = a.c.Instance(name); err != nil {
			return changes, err
		}
		netChanges, err := a.reconcileNetwork(inst, false, false)
		changes = append(changes, netChanges...)
		if err != nil {
			return changes, err
		}
	}
	// Ports are reconciled from the file directly, not from the record just
	// written: the record is for later verbs, and this is the verb with the
	// file in hand.
	inst, _, err = a.c.Instance(name)
	if err != nil {
		return changes, err
	}
	portChanges, err := ports.Reconcile(a.c, a.cfg.ACL, inst, m.Guest.Ports, false)
	changes = append(changes, portChanges...)
	if err != nil {
		return changes, fmt.Errorf("publishing ports: %w", err)
	}
	if none {
		if inst, _, err = a.c.Instance(name); err != nil {
			return changes, err
		}
		netChanges, err := a.reconcileNetwork(inst, true, false)
		changes = append(changes, netChanges...)
		if err != nil {
			return changes, err
		}
	}
	if inst, _, err = a.c.Instance(name); err != nil {
		return changes, err
	}
	volChanges, err := volumes.Reconcile(a.c, inst, m.Guest.Volumes, false)
	changes = append(changes, volChanges...)
	if err != nil {
		return changes, fmt.Errorf("volumes: %w", err)
	}
	if inst.Config[imageKey] != "" && inst.Config[imageKey] != want[imageKey] {
		changes = append(changes, "note: the image alias changed; an existing VM keeps its disk, so only a new VM is made from "+want[imageKey])
	}
	return changes, nil
}

// recordedPorts reads the ports an instance was given back out of its record.
func recordedPorts(inst *incus.Instance) ([]manifest.Port, error) {
	raw := inst.Config[portsKey]
	if raw == "" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("%s on %s is not a port list: %w", portsKey, inst.Name, err)
	}
	return ports.Parse(list)
}

// loadRecordedManifest returns the manifest an instance was made from, when it
// still exists where it was recorded. A missing file is reported, not fatal:
// the instance is complete without it.
func loadRecordedManifest(inst *incus.Instance) (*manifest.Manifest, string) {
	path := inst.Config[manifestKey]
	if path == "" {
		return nil, ""
	}
	if _, err := os.Stat(path); err != nil {
		return nil, path + " (missing)"
	}
	m, err := manifest.Load(path)
	if err != nil {
		return nil, path + " (" + err.Error() + ")"
	}
	return m, path
}

// --- handing devices back -------------------------------------------------

// spec turns a recorded recipe into the host-side routine's input.
func spec(pci string, r *devices.Return) hostdev.Spec {
	s := hostdev.Spec{PCI: pci}
	if r != nil {
		s.Modules, s.Reset, s.Alive, s.Unit = r.Modules, r.Reset, r.Alive, r.Unit
	}
	return s
}

// returnDetached hands each detached exclusive device back to the host by its
// recipe. Devices without one are left parked on vfio-pci, which is the right
// place for hardware the host never uses itself. Needs root for the sysfs
// writes, so each return re-execs under sudo unless already root; a failure
// is reported with the exact command to run, because the VM is stopped and
// the device detached either way.
func returnDetached(detached []devices.Detached) error {
	var failed []string
	for _, d := range detached {
		switch {
		case !manifest.Exclusive(d.Kind):
			continue
		case d.Return == nil:
			note("%s (%s) stays parked on vfio-pci: no return recipe recorded", d.Name, d.Address)
			continue
		}
		// Already back, by Incus's own rebind, and nothing else in the recipe
		// to do: asking for root here would only produce a prompt, or, without
		// a terminal, a failure report for a device that did return.
		if drv, ok := hostdev.Back(d.Address, d.Return.Alive); ok && d.Return.Unit == "" {
			note("%s (%s) is already back on %s", d.Name, d.Address, drv)
			continue
		}
		s := spec(d.Address, d.Return)
		var err error
		if os.Geteuid() == 0 {
			err = s.Return()
		} else {
			err = elevate(returnArgs(s))
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (%s): %v", d.Name, d.Address, err))
			continue
		}
		note("returned %s (%s) to the host", d.Name, d.Address)
	}
	if len(failed) > 0 {
		return fmt.Errorf("stopped and detached, but not every device made it back to the host:\n  %s\n"+
			"Run the printed sudo command by hand, or `rig host desktop` for the card.", strings.Join(failed, "\n  "))
	}
	return nil
}

// freeWanted stops the host unit of every device the instance is about to
// claim, when one is declared and running. The converse of returnDetached.
func freeWanted(wanted []devices.Decl) error {
	for _, d := range wanted {
		if d.Return == nil || d.Return.Unit == "" {
			continue
		}
		s := spec(d.PCI, d.Return)
		if !s.UnitActive() {
			continue
		}
		// As the invoking user we can read that user's own processes, which
		// is exactly whose session is about to end. After sudo it would be
		// too late to matter and no more accurate.
		if holders := s.Holders(); len(holders) > 0 {
			fmt.Fprintf(os.Stderr, "these hold %s and will lose it when %s stops:\n", d.PCI, d.Return.Unit)
			for _, h := range holders {
				fmt.Fprintf(os.Stderr, "  %s\n", h)
			}
		}
		var err error
		if os.Geteuid() == 0 {
			err = s.Free()
		} else {
			err = elevate(freeArgs(s))
		}
		if err != nil {
			return fmt.Errorf("freeing %s for %s: %w", d.PCI, d.Name, err)
		}
	}
	return nil
}

// --- the network device -----------------------------------------------------

// networkPlan works out how an instance's devices change for a manifest's
// `network:`. With none, every NIC the instance has is removed: one it
// inherits from a profile is masked with a `none` device of the same name,
// which is how Incus takes an inherited device away, and one of its own (the
// override ports put there) is deleted. Without none, the masks over profile
// NICs are lifted. A `none` device over anything else is someone else's
// decision and is left alone. Pure, so the rules are testable without Incus.
func networkPlan(inst *incus.Instance, profileNICs map[string]bool, none bool) (map[string]incus.Device, []string) {
	devices := make(map[string]incus.Device, len(inst.Devices))
	for name, dev := range inst.Devices {
		devices[name] = dev
	}
	var changes []string
	if none {
		for name := range inst.NICs() {
			if profileNICs[name] {
				devices[name] = incus.Device{"type": "none"}
				changes = append(changes, name+": mask the profile's NIC (no network device)")
			} else {
				delete(devices, name)
				changes = append(changes, name+": remove the NIC (no network device)")
			}
		}
	} else {
		for name, dev := range inst.Devices {
			if dev.Type() == "none" && profileNICs[name] {
				delete(devices, name)
				changes = append(changes, name+": lift the mask; the profile's NIC applies again")
			}
		}
	}
	sort.Strings(changes)
	return devices, changes
}

// reconcileNetwork brings an instance's network devices to what the manifest
// asks for, or with dryRun reports what it would change. A NIC is not changed
// on a running VM.
func (a *app) reconcileNetwork(inst *incus.Instance, none, dryRun bool) ([]string, error) {
	profileNICs := map[string]bool{}
	for _, name := range inst.Profiles {
		p, _, err := a.c.Profile(name)
		if err != nil {
			return nil, err
		}
		for dev, d := range p.Devices {
			if d.Type() == "nic" {
				profileNICs[dev] = true
			}
		}
	}
	devices, changes := networkPlan(inst, profileNICs, none)
	if len(changes) == 0 || dryRun {
		return changes, nil
	}
	if inst.Running() {
		return changes, fmt.Errorf("%s is running; its network device cannot be changed live.\n  rig stop %s, then rig apply -f again", inst.Name, inst.Name)
	}
	return changes, a.c.SetDevices(inst.Name, devices)
}

// checkCPUs refuses a pinned CPU set this host cannot honour, or one that
// would leave it no core of its own; see internal/cpuset.
func checkCPUs(m *manifest.Manifest) error {
	if m.Guest.CPUs.Set == "" {
		return nil
	}
	warnings, err := cpuset.Check(m.Guest.CPUs.Set)
	if err != nil {
		return fmt.Errorf("guest.cpus: %w", err)
	}
	for _, w := range warnings {
		note("WARNING: guest.cpus: %s", w)
	}
	return nil
}
