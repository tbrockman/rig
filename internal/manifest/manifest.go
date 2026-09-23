// Package manifest reads rig.yaml: what a project wants its VM to be given.
//
// The file is declared intent, in the shape of a Compose file because that is
// the shape people and models already know. Incus stays the record of what is
// true. `rig new -f` turns the file into an instance, recording on it what it
// was given; `rig apply -f` reconciles an existing instance to the file; and
// `rig doctor` reports where the two have drifted.
//
// The host block names devices on this machine — their addresses, and how the
// host takes each one back after a VM stops. It repeats per project, which is
// a cost paid deliberately: a project's file then describes everything the VM
// needs without a second file somewhere else to find.
package manifest

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultFile is where the verbs look when -f is not given.
const DefaultFile = "rig.yaml"

type Manifest struct {
	Host  Host  `yaml:"host"`
	Guest Guest `yaml:"guest"`

	// Path is the file this came from, absolute; Dir is what relative paths
	// inside it resolve against.
	Path string `yaml:"-"`
	Dir  string `yaml:"-"`
}

type Host struct {
	Devices map[string]Device `yaml:"devices"`
}

// Device is one host device a VM may be given.
//
// Kind selects everything rig knows about it: how to express it to Incus, and
// whether it is exclusive. gpu and pci pass a whole PCI function through VFIO,
// so at most one VM may hold one at a time and the host loses it meanwhile;
// usb attaches one USB device by identity and can be shared or hotplugged.
type Device struct {
	Kind string `yaml:"kind"`
	PCI  string `yaml:"pci,omitempty"` // gpu, pci: 0000:3c:00.3
	// ID is vendor:product for usb, which is how the device is found. For gpu
	// and pci it is vendor:device, what must be at the address: PCI addresses
	// are renumbered when a device appears or disappears ahead of them (a BIOS
	// setting is enough), and an address alone once handed a VM the host's SATA
	// controller in place of a USB one.
	ID string `yaml:"id,omitempty"`

	// Return says how the host takes an exclusive device back once no VM
	// holds it. Absent means it stays parked on vfio-pci, which is right for a
	// device the host never uses itself.
	Return *Return `yaml:"return,omitempty"`
}

// Return is the host-side recipe for one device, as data rather than code.
type Return struct {
	// Modules to unload before the unbind and reload after the reset. The
	// NVIDIA driver needs this; most drivers do not.
	Modules []string `yaml:"modules,omitempty"`
	// Reset issues a function-level reset before the host driver binds. A
	// card handed back from a guest keeps that guest's state, and on some
	// hardware the host driver cannot initialise on top of it.
	Reset bool `yaml:"reset,omitempty"`
	// Alive is a glob under the device's sysfs directory that exists only
	// once the host driver has actually brought the device up: drm/card* for
	// a display adapter, usb* for a USB controller. Binding is not working;
	// this is the check that says it worked.
	Alive string `yaml:"alive,omitempty"`
	// Unit is a host systemd unit that uses the device — a display manager,
	// typically. Stopped before a VM claims the device, started after it is
	// returned.
	Unit string `yaml:"unit,omitempty"`
}

type Guest struct {
	Name    string   `yaml:"name"`
	Image   string   `yaml:"image,omitempty"`
	Build   string   `yaml:"build,omitempty"`
	CPUs    CPUs     `yaml:"cpus,omitempty"`
	Memory  string   `yaml:"memory,omitempty"`
	Disk    string   `yaml:"disk,omitempty"`
	EnvFile string   `yaml:"env_file,omitempty"`
	Devices []string `yaml:"devices,omitempty"`
	Ports   []Port   `yaml:"ports,omitempty"`
	// Volumes are Incus custom storage volumes mounted into the guest. They
	// outlive the VM: `rig rm` leaves them, so what is on them survives the
	// VM being recreated from a new image.
	Volumes []Volume `yaml:"volumes,omitempty"`
	// Network is empty for the profile's NIC, the isolated default, or
	// NetworkNone for no network device at all: the profile's NIC masked. A
	// guest with none still has every rig verb, which runs over vsock.
	Network string `yaml:"network,omitempty"`
	// Input is empty, or InputHost to have `rig start` lend this host's
	// keyboard and mouse to the guest as events (`rig host input`) for as
	// long as the VM runs.
	Input string `yaml:"input,omitempty"`
}

// NetworkNone asks for a VM with no network device.
const NetworkNone = "none"

// InputHost asks for the host's keyboard and mouse, forwarded from start to
// stop.
const InputHost = "host"

// CPUs is guest.cpus: a count of vCPUs ("8"), or a set of host CPUs to pin
// them to ("4-7,12-15"), one vCPU per host CPU. Pinning keeps the host's
// scheduler from moving a vCPU mid-deadline, which is what a real-time guest
// (a DAW) needs and a batch one does not.
type CPUs struct {
	Count int
	Set   string
}

var cpuSetRE = regexp.MustCompile(`^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`)

func (c *CPUs) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: guest.cpus is a count (8) or a set of host CPUs (\"4-7,12-15\")", n.Line)
	}
	if count, err := strconv.Atoi(n.Value); err == nil {
		if count < 1 {
			return fmt.Errorf("line %d: guest.cpus must be at least 1", n.Line)
		}
		*c = CPUs{Count: count}
		return nil
	}
	if !cpuSetRE.MatchString(n.Value) {
		return fmt.Errorf("line %d: guest.cpus %q is neither a count nor a CPU set like \"4-7,12-15\"", n.Line, n.Value)
	}
	for _, part := range strings.Split(n.Value, ",") {
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, _ := strconv.Atoi(lo)
			b, _ := strconv.Atoi(hi)
			if b < a {
				return fmt.Errorf("line %d: guest.cpus range %s runs backwards", n.Line, part)
			}
		}
	}
	*c = CPUs{Set: n.Value}
	return nil
}

// Limit is the value for Incus's limits.cpu, or "" when unset.
func (c CPUs) Limit() string {
	if c.Set != "" {
		return c.Set
	}
	if c.Count > 0 {
		return strconv.Itoa(c.Count)
	}
	return ""
}

// Volume is one entry of guest.volumes: an Incus custom storage volume by
// name, and where the guest mounts it. The short form is Compose's,
// "name:/path"; the long form adds a size, and the uid:gid that owns the
// volume's root when it is created (the guest's desktop user, typically: a
// volume is created root-owned otherwise, and an application running as that
// user cannot write to it).
//
// Only named volumes. A host path on the left, Compose's bind mount, is
// refused: a directory of this host inside the guest is the kind of path the
// isolation exists to keep out, and an untrusted guest could write to it.
type Volume struct {
	Name  string `yaml:"source"`
	Path  string `yaml:"target"`
	Size  string `yaml:"size,omitempty"`
	Owner string `yaml:"owner,omitempty"`
}

var (
	volumeNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	ownerRE      = regexp.MustCompile(`^[0-9]+:[0-9]+$`)
)

func (v *Volume) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		name, path, ok := strings.Cut(n.Value, ":")
		if !ok {
			return fmt.Errorf("line %d: volume %q: want name:/path in the guest", n.Line, n.Value)
		}
		*v = Volume{Name: name, Path: path}
	} else {
		type long Volume
		var l long
		if err := n.Decode(&l); err != nil {
			return err
		}
		*v = Volume(l)
	}
	return v.validate(n.Line)
}

func (v *Volume) validate(line int) error {
	switch {
	case strings.HasPrefix(v.Name, "/") || strings.HasPrefix(v.Name, ".") || strings.HasPrefix(v.Name, "~"):
		return fmt.Errorf("line %d: volume source %q is a host path. Only named volumes: a host directory "+
			"inside the guest is a path out of the isolation", line, v.Name)
	case !volumeNameRE.MatchString(v.Name):
		return fmt.Errorf("line %d: volume name %q: lowercase letters, digits, - and _", line, v.Name)
	case !strings.HasPrefix(v.Path, "/") || v.Path == "/" || strings.Contains(v.Path, ".."):
		return fmt.Errorf("line %d: volume %s: target %q must be an absolute path in the guest, not / itself", line, v.Name, v.Path)
	case v.Owner != "" && !ownerRE.MatchString(v.Owner):
		return fmt.Errorf("line %d: volume %s: owner is uid:gid, e.g. 1000:100", line, v.Name)
	}
	return nil
}

// Port is one published port, in Compose's terms: Published is the host side,
// Target the guest side, each a port or an inclusive range "a-b". HostIP is
// the host address to publish on; empty means the address on the host's
// default route, which is the one a machine on the LAN would use.
type Port struct {
	HostIP    string `yaml:"host_ip,omitempty"`
	Published string `yaml:"published"`
	Target    string `yaml:"target"`
	Protocol  string `yaml:"protocol,omitempty"`
}

// String renders the port back in Compose's short form.
func (p Port) String() string {
	s := p.Published + ":" + p.Target + "/" + p.Protocol
	if p.HostIP != "" {
		s = p.HostIP + ":" + s
	}
	return s
}

// UnmarshalYAML accepts both Compose forms: the short string
// "published:target/protocol" and the long mapping.
func (p *Port) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		parsed, err := ParsePort(n.Value)
		if err != nil {
			return err
		}
		*p = parsed
		return nil
	}
	type long Port
	var l long
	if err := n.Decode(&l); err != nil {
		return err
	}
	*p = Port(l)
	if p.Target == "" {
		p.Target = p.Published
	}
	if p.Published == "" {
		p.Published = p.Target
	}
	return p.validate()
}

var portRE = regexp.MustCompile(`^(?:(\d+\.\d+\.\d+\.\d+):)?(\d+(?:-\d+)?)(?::(\d+(?:-\d+)?))?(?:/(tcp|udp))?$`)

// ParsePort reads Compose's short syntax: "8080", "8080:80", "8080:80/udp",
// "5000-5010:5000-5010/tcp", "192.168.1.2:8080:80".
func ParsePort(s string) (Port, error) {
	m := portRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Port{}, fmt.Errorf("port %q: want [host_ip:]published[:target][/tcp|udp], e.g. \"47989:47989/tcp\"", s)
	}
	p := Port{HostIP: m[1], Published: m[2], Target: m[3], Protocol: m[4]}
	if p.Target == "" {
		p.Target = p.Published
	}
	return p, p.validate()
}

func (p *Port) validate() error {
	if p.Protocol == "" {
		p.Protocol = "tcp"
	}
	if p.Protocol != "tcp" && p.Protocol != "udp" {
		return fmt.Errorf("port %s: protocol must be tcp or udp", p.String())
	}
	if p.HostIP != "" {
		ip := net.ParseIP(p.HostIP)
		if ip == nil || ip.To4() == nil || ip.IsUnspecified() {
			return fmt.Errorf("port %s: host_ip must be one IPv4 address this host holds (not a wildcard: Incus maps ports on one address)", p.String())
		}
	}
	pubLo, pubHi, err := portRange(p.Published)
	if err != nil {
		return fmt.Errorf("port %s: %w", p.String(), err)
	}
	tgtLo, tgtHi, err := portRange(p.Target)
	if err != nil {
		return fmt.Errorf("port %s: %w", p.String(), err)
	}
	if pubHi-pubLo != tgtHi-tgtLo {
		return fmt.Errorf("port %s: published and target ranges differ in size", p.String())
	}
	return nil
}

// Range returns the inclusive bounds of a "a" or "a-b" spec.
func portRange(s string) (lo, hi int, err error) {
	lo, hi = 0, 0
	parts := strings.SplitN(s, "-", 2)
	if lo, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, fmt.Errorf("%q is not a port", parts[0])
	}
	hi = lo
	if len(parts) == 2 {
		if hi, err = strconv.Atoi(parts[1]); err != nil {
			return 0, 0, fmt.Errorf("%q is not a port", parts[1])
		}
	}
	if lo < 1 || hi > 65535 || hi < lo {
		return 0, 0, fmt.Errorf("%s is not a valid port range", s)
	}
	return lo, hi, nil
}

// PublishedRange and TargetRange expose the bounds for callers that build
// firewall rules from them.
func (p Port) PublishedRange() (int, int) { lo, hi, _ := portRange(p.Published); return lo, hi }
func (p Port) TargetRange() (int, int)    { lo, hi, _ := portRange(p.Target); return lo, hi }

// --- loading -------------------------------------------------------------

// Load reads and validates one file. Unknown keys are errors, because a typo
// in a manifest that is silently ignored is a VM configured differently from
// what its file says, with nothing to say so.
func Load(path string) (*Manifest, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	m, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m.Path = abs
	m.Dir = filepath.Dir(abs)
	return m, nil
}

// Parse decodes and validates manifest text. Dir is left empty; Load fills it.
func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

var (
	nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,61}$`)
	pciRE  = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	usbRE  = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{4}$`)
)

// Kinds rig knows. The set is the whole vocabulary: everything else in rig is
// generic over it.
const (
	KindGPU = "gpu"
	KindPCI = "pci"
	KindUSB = "usb"
)

// Exclusive reports whether at most one VM may hold a device of this kind at
// a time. VFIO passthrough hands the whole function to one guest; a USB
// device redirected by identity does not.
func Exclusive(kind string) bool { return kind == KindGPU || kind == KindPCI }

func (m *Manifest) validate() error {
	if m.Guest.Name == "" {
		return fmt.Errorf("guest.name is required")
	}
	if !nameRE.MatchString(m.Guest.Name) {
		return fmt.Errorf("guest.name %q is not a valid instance name", m.Guest.Name)
	}
	if m.Guest.Image == "" && m.Guest.Build == "" {
		return fmt.Errorf("guest needs image: <alias> or build: <flake directory>")
	}
	switch m.Guest.Network {
	case "", NetworkNone:
	default:
		return fmt.Errorf("guest.network %q: leave it out for the isolated default, or say %q for no network device", m.Guest.Network, NetworkNone)
	}
	switch m.Guest.Input {
	case "", InputHost:
	default:
		return fmt.Errorf("guest.input %q: leave it out, or say %q to lend the guest this host's keyboard and mouse", m.Guest.Input, InputHost)
	}
	if m.Guest.Network == NetworkNone && len(m.Guest.Ports) > 0 {
		return fmt.Errorf("guest.network is %q but guest.ports publishes %d port(s): a published port needs a network device", NetworkNone, len(m.Guest.Ports))
	}
	seenVol, seenPath := map[string]bool{}, map[string]bool{}
	for _, v := range m.Guest.Volumes {
		if seenVol[v.Name] || seenPath[v.Path] {
			return fmt.Errorf("guest.volumes: %s or %s is listed twice", v.Name, v.Path)
		}
		seenVol[v.Name], seenPath[v.Path] = true, true
	}
	for name, d := range m.Host.Devices {
		if !nameRE.MatchString(name) {
			return fmt.Errorf("host.devices.%s: not a valid device name", name)
		}
		switch d.Kind {
		case KindGPU, KindPCI:
			if !pciRE.MatchString(d.PCI) {
				return fmt.Errorf("host.devices.%s: kind %s needs pci: 0000:bb:dd.f (from lspci -D)", name, d.Kind)
			}
			switch {
			case d.ID != "" && !usbRE.MatchString(d.ID):
				return fmt.Errorf("host.devices.%s: id is vendor:device in hex, as lspci -nn prints it, e.g. 1022:2222", name)
			case d.ID == "" && d.Kind == KindPCI:
				// A card is at least checked for being an NVIDIA display
				// controller; an arbitrary PCI function has nothing to check
				// against but what the file says it is.
				return fmt.Errorf("host.devices.%s: kind pci needs id: vendor:device (lspci -nn -s %s), so a renumbered bus cannot hand the VM some other device", name, strings.TrimPrefix(d.PCI, "0000:"))
			}
		case KindUSB:
			if !usbRE.MatchString(d.ID) {
				return fmt.Errorf("host.devices.%s: kind usb needs id: vendor:product (from lsusb), e.g. 1234:5678", name)
			}
			if d.PCI != "" || d.Return != nil {
				return fmt.Errorf("host.devices.%s: a usb device has no pci address and nothing to return; the host keeps its controller", name)
			}
		case "":
			return fmt.Errorf("host.devices.%s: kind is required (gpu, pci or usb)", name)
		default:
			return fmt.Errorf("host.devices.%s: unknown kind %q (gpu, pci or usb)", name, d.Kind)
		}
		if d.Return != nil {
			if d.Return.Alive != "" && (strings.Contains(d.Return.Alive, "..") || strings.HasPrefix(d.Return.Alive, "/")) {
				return fmt.Errorf("host.devices.%s: return.alive is a pattern under the device's sysfs directory, not a path", name)
			}
			for _, mod := range d.Return.Modules {
				if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(mod) {
					return fmt.Errorf("host.devices.%s: %q is not a module name", name, mod)
				}
			}
		}
	}
	seenPCI := map[string]string{}
	for _, name := range sortedKeys(m.Host.Devices) {
		d := m.Host.Devices[name]
		if d.PCI == "" {
			continue
		}
		if prev, dup := seenPCI[d.PCI]; dup {
			return fmt.Errorf("host.devices: %s and %s name the same PCI address %s", prev, name, d.PCI)
		}
		seenPCI[d.PCI] = name
	}
	seenWant := map[string]bool{}
	for _, want := range m.Guest.Devices {
		if _, ok := m.Host.Devices[want]; !ok {
			return fmt.Errorf("guest.devices names %q, which host.devices does not declare", want)
		}
		if seenWant[want] {
			return fmt.Errorf("guest.devices lists %q twice", want)
		}
		seenWant[want] = true
	}
	return nil
}

// Wanted returns the devices the guest asks for, named, in the order listed.
func (m *Manifest) Wanted() []Named {
	out := make([]Named, 0, len(m.Guest.Devices))
	for _, name := range m.Guest.Devices {
		out = append(out, Named{Name: name, Device: m.Host.Devices[name]})
	}
	return out
}

// Named is a device with the name it was declared under.
type Named struct {
	Name string
	Device
}

// Resolve turns a path from the file into an absolute one, relative to the
// file's directory. Absolute paths pass through.
func (m *Manifest) Resolve(p string) string {
	p = ExpandHome(p)
	if p == "" || filepath.IsAbs(p) || m.Dir == "" {
		return p
	}
	return filepath.Join(m.Dir, p)
}

// ExpandHome turns a leading ~/ into the home directory, so a file can name a
// path outside any project, such as a credential under ~/.config/rig, the way
// a shell would. Anything else is returned as it is.
func ExpandHome(p string) string {
	rest, ok := strings.CutPrefix(p, "~/")
	if !ok {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, rest)
}

// ImageAlias is the Incus alias the guest is made from: the one named, else
// one derived from the project name for a `build:`.
func (m *Manifest) ImageAlias() string {
	if m.Guest.Image != "" {
		return m.Guest.Image
	}
	return m.Guest.Name + "-guest"
}

func sortedKeys[V any](in map[string]V) []string {
	out := make([]string, 0, len(in))
	for k := range in {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
