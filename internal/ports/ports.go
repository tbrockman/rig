// Package ports publishes guest ports to the LAN, as a manifest's `ports:`
// declares, and undoes it when the declaration goes away.
//
// Three things stand between a packet arriving at this host and a service in
// the guest, and this package arranges two of them. Incus does the mapping
// from the host's address to the guest's, which needs the guest to hold a
// fixed address, so one is pinned on the instance's NIC. That mapped traffic
// still arrives at the guest's NIC as ingress, which the isolation ACL rejects
// by default, so a second, per-instance ACL sits beside it and allows exactly
// the declared ports. The third thing is the guest's own firewall, which is
// the guest image's business (`openFirewall` in NixOS terms) and cannot be
// reached from here.
//
// This is a deliberate, declared opening of the isolation's ingress side, for
// one guest and named ports. It does not touch what the isolation is for,
// which is the guest reaching out to the host and the LAN.
package ports

import (
	"fmt"
	"hash/fnv"
	"net"
	"os/exec"
	"sort"
	"strings"

	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
	"github.com/tbrockman/rig/internal/policy"
	"github.com/tbrockman/rig/internal/verify"
)

// ACLName is the per-instance ACL that admits the published ports.
func ACLName(instance string) string { return "rig-fwd-" + instance }

// devicePrefix marks the proxy devices this package owns on an instance, so
// it removes only its own.
const devicePrefix = "rig-port-"

// DeviceName is the proxy device for one published port or range.
func DeviceName(p manifest.Port) string {
	return devicePrefix + p.Protocol + "-" + strings.ReplaceAll(p.Published, "-", "to")
}

// ProxyDevice is the Incus device that maps one published port or range on
// one host address to the guest's.
//
// NAT mode is the only mode Incus offers a VM (6.0.5 refuses the other with
// "Only NAT mode is supported for proxies on VM instances"), and it is the
// right one anyway: it is nftables on the host rather than a process copying
// bytes. Two things it insists on, both learned by being refused: the listen
// address is one concrete host address, never the wildcard, and the
// instance's NIC carries a static address, which a wildcard connect address
// is filled in with.
func ProxyDevice(p manifest.Port, hostIP string) incus.Device {
	listen := p.HostIP
	if listen == "" {
		listen = hostIP
	}
	return incus.Device{
		"type":    "proxy",
		"listen":  p.Protocol + ":" + listen + ":" + p.Published,
		"connect": p.Protocol + ":0.0.0.0:" + p.Target,
		"nat":     "true",
	}
}

// LANAddress is the host's IPv4 address on its default-route interface: what
// a machine on the LAN, or a Moonlight client, would dial.
func LANAddress() (string, error) {
	ipnet, err := lanNet()
	if err != nil {
		return "", err
	}
	return ipnet.IP.String(), nil
}

// LANNetwork is the subnet that address sits in, for scoping a firewall rule
// to the machines that can reach the published ports anyway.
func LANNetwork() (string, error) {
	ipnet, err := lanNet()
	if err != nil {
		return "", err
	}
	return (&net.IPNet{IP: ipnet.IP.Mask(ipnet.Mask), Mask: ipnet.Mask}).String(), nil
}

func lanNet() (*net.IPNet, error) {
	iface, _ := verify.DefaultRoute()
	if iface == "" {
		return nil, fmt.Errorf("this host has no default route, so there is no LAN address to publish on; name one with host_ip")
	}
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, err
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return &net.IPNet{IP: ipnet.IP.To4(), Mask: ipnet.Mask}, nil
		}
	}
	return nil, fmt.Errorf("%s carries the default route but has no IPv4 address; name one with host_ip", iface)
}

// --- the host's own firewall ---------------------------------------------

// HostFirewall reports whether ufw is active on this host. ufw filters the
// forward path a mapped packet takes from the LAN into the guest, and drops
// it by default — while traffic from the host itself takes a path ufw does
// not filter, so a test from the host passes and a client on the LAN fails.
func HostFirewall() bool {
	out, _ := exec.Command("systemctl", "is-active", "ufw").Output()
	return strings.TrimSpace(string(out)) == "active"
}

// RecentlyBlocked counts forwarded packets to the guest that ufw logged as
// dropped in the last few minutes. The kernel log is readable without root,
// which ufw's own status is not, so this is the evidence rig can actually
// get: a client trying and failing right now.
func RecentlyBlocked(guestIP string) int {
	out, err := exec.Command("journalctl", "-k", "--no-pager", "--since", "-10 minutes").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "UFW BLOCK") && strings.Contains(line, "DST="+guestIP+" ") && strings.Contains(line, "OUT=") && !strings.Contains(line, "OUT= ") {
			n++
		}
	}
	return n
}

// UFWRules are the ufw commands that admit the published ports through the
// host's forward chain, scoped to where each port is published from — the
// tailnet for a port on the host's Tailscale address, else the LAN — and to
// this guest's address.
func UFWRules(guestIP string, ports []manifest.Port) []string {
	type key struct{ from, proto string }
	groups := map[key][]string{}
	for _, p := range ports {
		k := key{sourceScope(p.HostIP), p.Protocol}
		groups[k] = append(groups[k], strings.ReplaceAll(p.Target, "-", ":"))
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].from != keys[b].from {
			return keys[a].from < keys[b].from
		}
		return keys[a].proto < keys[b].proto
	})
	var out []string
	for _, k := range keys {
		list := groups[k]
		sort.Strings(list)
		out = append(out, fmt.Sprintf("sudo ufw route allow proto %s from %s to %s port %s", k.proto, k.from, guestIP, strings.Join(list, ",")))
	}
	return out
}

// tailnet is the CGNAT range Tailscale assigns from.
var tailnet = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()

// Reach names who can reach a set of published ports, for a report: "tailnet"
// when every port sits on a Tailscale address, "LAN" when none does, else both.
func Reach(ports []manifest.Port) string {
	tail, lan := false, false
	for _, p := range ports {
		if ip := net.ParseIP(p.HostIP); ip != nil && tailnet.Contains(ip) {
			tail = true
		} else {
			lan = true
		}
	}
	switch {
	case tail && lan:
		return "tailnet and LAN"
	case tail:
		return "tailnet"
	}
	return "LAN"
}

// sourceScope is who can reach a port published on hostIP: tailnet peers for
// a Tailscale address, otherwise the LAN the host's default route is on.
func sourceScope(hostIP string) string {
	if ip := net.ParseIP(hostIP); ip != nil && tailnet.Contains(ip) {
		return tailnet.String()
	}
	lan, err := LANNetwork()
	if err != nil {
		return "any"
	}
	return lan
}

// DesiredACL is the ingress allow-list for a set of ports: one rule per
// protocol, all its ports in one comma-separated list, the form Incus takes.
func DesiredACL(instance string, ports []manifest.Port) *incus.ACL {
	byProto := map[string][]string{}
	for _, p := range ports {
		byProto[p.Protocol] = append(byProto[p.Protocol], p.Target)
	}
	var rules []incus.ACLRule
	for _, proto := range sortedKeys(byProto) {
		list := byProto[proto]
		sort.Strings(list)
		rules = append(rules, incus.ACLRule{
			Action: "allow", Protocol: proto, DestinationPort: strings.Join(list, ","), State: "enabled",
		})
	}
	if rules == nil {
		rules = []incus.ACLRule{}
	}
	return &incus.ACL{
		Name:        ACLName(instance),
		Description: "rig: published ports for " + instance + " (ingress allow-list)",
		Ingress:     rules,
		Egress:      []incus.ACLRule{},
		Config:      map[string]string{},
	}
}

// Parse reads the recorded list back into ports. The record is the manifest's
// short strings, so a bad one is a bug in what wrote it, not operator input.
func Parse(recorded []string) ([]manifest.Port, error) {
	out := make([]manifest.Port, 0, len(recorded))
	for _, s := range recorded {
		p, err := manifest.ParsePort(s)
		if err != nil {
			return nil, fmt.Errorf("recorded port %q: %w", s, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// PickAddress chooses a static address for an instance inside the bridge's
// subnet, avoiding what other instances hold. Deterministic in the name, so
// re-running lands on the same address, and drawn from the top of the range,
// where dnsmasq's dynamic leases are least likely to be.
func PickAddress(name, subnet string, taken map[string]bool) (string, error) {
	ip, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", fmt.Errorf("bridge address %q: %w", subnet, err)
	}
	ones, bits := ipnet.Mask.Size()
	size := 1 << (bits - ones)
	if size < 8 {
		return "", fmt.Errorf("bridge subnet %s is too small to pick a static address from", subnet)
	}
	base := ipnet.IP.To4()
	if base == nil {
		return "", fmt.Errorf("bridge %s is not IPv4", subnet)
	}
	gateway := ip.To4().String()

	h := fnv.New32a()
	h.Write([]byte(name))
	// Candidates from the top of the range downward, offset by the hash so
	// two instances do not fight over .254.
	start := size - 2 - int(h.Sum32()%uint32(size/4))
	for i := 0; i < size-2; i++ {
		n := start - i
		if n < 1 {
			n += size - 2
		}
		cand := make(net.IP, 4)
		copy(cand, base)
		v := (uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])) + uint32(n)
		cand[0], cand[1], cand[2], cand[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
		s := cand.String()
		if s == gateway || taken[s] {
			continue
		}
		return s, nil
	}
	return "", fmt.Errorf("no free address in %s", subnet)
}

// --- reconcile -----------------------------------------------------------

// Reconcile makes Incus publish exactly ports for the instance: the ACL, the
// NIC's static address and ACL list, and the proxy devices. With an empty
// list it removes all three and hands the NIC back to the profile. Returns
// what it changed, or with dryRun what it would.
func Reconcile(c *incus.Client, baseACL string, inst *incus.Instance, ports []manifest.Port, dryRun bool) ([]string, error) {
	var changes []string
	aclName := ACLName(inst.Name)
	nicName, nic := oneNIC(inst)
	if nicName == "" {
		// A VM with no network device at all (its profile NIC masked with a
		// `none` device) has nothing to publish and nothing to clean up on a
		// NIC. That is a deliberate state, not an error: apply and doctor
		// both come through here.
		if len(ports) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("%s has no NIC to publish ports on", inst.Name)
	}
	ours := policyHasACL(nic["security.acls"], aclName)

	if len(ports) == 0 {
		return removeAll(c, inst, nicName, aclName, ours, dryRun)
	}

	// 1. The ACL object.
	want := DesiredACL(inst.Name, ports)
	names, err := c.ACLNames()
	if err != nil {
		return nil, err
	}
	if !contains(names, aclName) {
		changes = append(changes, fmt.Sprintf("create ACL %s allowing ingress %s", aclName, describe(ports)))
		if !dryRun {
			if err := c.CreateACL(want); err != nil {
				return changes, err
			}
		}
	} else {
		cur, etag, err := c.ACL(aclName)
		if err != nil {
			return changes, err
		}
		if policy.RuleKey(cur.Ingress) != policy.RuleKey(want.Ingress) {
			changes = append(changes, fmt.Sprintf("rewrite ACL %s to allow ingress %s", aclName, describe(ports)))
			if !dryRun {
				if err := policy.RewriteACL(c, aclName, want, etag, len(cur.UsedBy) > 0); err != nil {
					return changes, err
				}
			}
		}
	}

	// 2. The NIC: a static address, and our ACL beside the isolation's.
	//
	// The instance's NIC normally comes from the profile. Overriding it means
	// copying the whole device onto the instance and changing two keys; the
	// isolation keys come along, so `rig setup` still sees an isolated NIC.
	local := incus.Device{}
	for k, v := range nic {
		local[k] = v
	}
	nicChanged := false
	if local["ipv4.address"] == "" {
		addr, err := staticAddress(c, inst, nic)
		if err != nil {
			return changes, err
		}
		local["ipv4.address"] = addr
		nicChanged = true
		changes = append(changes, fmt.Sprintf("%s/%s: pin ipv4.address %s (NAT-mode proxies need a fixed guest address)", inst.Name, nicName, addr))
	}
	if !ours {
		local["security.acls"] = addACL(local["security.acls"], aclName)
		nicChanged = true
		changes = append(changes, fmt.Sprintf("%s/%s: security.acls %q -> %q", inst.Name, nicName, nic["security.acls"], local["security.acls"]))
	}
	if !policyHasACL(local["security.acls"], baseACL) {
		return changes, fmt.Errorf("%s/%s does not carry the isolation ACL %q; publishing ports onto an "+
			"unisolated NIC is not something rig will do. Run rig setup first.", inst.Name, nicName, baseACL)
	}

	// 3. The proxy devices.
	devices := cloneDevices(inst.Devices)
	if nicChanged {
		if inst.Running() {
			return changes, fmt.Errorf("%s is running; its NIC cannot be changed live.\n  rig stop %s, then rig apply -f again", inst.Name, inst.Name)
		}
		devices[nicName] = local
	}
	hostIP := ""
	for _, p := range ports {
		if p.HostIP == "" {
			addr, err := LANAddress()
			if err != nil {
				return changes, err
			}
			hostIP = addr
			break
		}
	}
	wantDevices := map[string]incus.Device{}
	for _, p := range ports {
		wantDevices[DeviceName(p)] = ProxyDevice(p, hostIP)
	}
	devChanged := nicChanged
	for name, dev := range devices {
		if strings.HasPrefix(name, devicePrefix) && wantDevices[name] == nil {
			delete(devices, name)
			devChanged = true
			changes = append(changes, fmt.Sprintf("%s: remove proxy %s (%s)", inst.Name, name, dev["listen"]))
		}
	}
	for _, name := range sortedKeys(wantDevices) {
		if equal(devices[name], wantDevices[name]) {
			continue
		}
		devices[name] = wantDevices[name]
		devChanged = true
		changes = append(changes, fmt.Sprintf("%s: publish %s -> guest %s", inst.Name, wantDevices[name]["listen"], strings.TrimPrefix(wantDevices[name]["connect"], wantDevices[name]["type"])))
	}
	if devChanged && !dryRun {
		if err := c.SetDevices(inst.Name, devices); err != nil {
			return changes, err
		}
	}
	return changes, nil
}

func removeAll(c *incus.Client, inst *incus.Instance, nicName, aclName string, ours, dryRun bool) ([]string, error) {
	var changes []string
	devices := cloneDevices(inst.Devices)
	changed := false
	for name, dev := range devices {
		if strings.HasPrefix(name, devicePrefix) {
			delete(devices, name)
			changed = true
			changes = append(changes, fmt.Sprintf("%s: remove proxy %s (%s)", inst.Name, name, dev["listen"]))
		}
	}
	if ours {
		if _, isLocal := devices[nicName]; isLocal {
			// The override existed for the ports; without them the profile's
			// NIC is the right one again. Dropping the override also drops
			// the static address, which nothing needs any more.
			delete(devices, nicName)
			changed = true
			changes = append(changes, fmt.Sprintf("%s/%s: drop the NIC override; the profile's NIC applies again", inst.Name, nicName))
		}
	}
	if changed {
		if inst.Running() {
			return changes, fmt.Errorf("%s is running; its NIC cannot be changed live.\n  rig stop %s, then rig apply -f again", inst.Name, inst.Name)
		}
		if !dryRun {
			if err := c.SetDevices(inst.Name, devices); err != nil {
				return changes, err
			}
		}
	}
	names, err := c.ACLNames()
	if err != nil {
		return changes, err
	}
	if contains(names, aclName) {
		changes = append(changes, "delete ACL "+aclName)
		if !dryRun {
			if err := c.DeleteACL(aclName); err != nil {
				return changes, err
			}
		}
	}
	return changes, nil
}

// Cleanup removes the per-instance ACL after the instance is gone; the proxy
// devices and the NIC override went with the instance.
func Cleanup(c *incus.Client, instance string) error {
	names, err := c.ACLNames()
	if err != nil {
		return err
	}
	if contains(names, ACLName(instance)) {
		return c.DeleteACL(ACLName(instance))
	}
	return nil
}

// staticAddress picks an address on the NIC's bridge that no other instance
// has pinned.
func staticAddress(c *incus.Client, inst *incus.Instance, nic incus.Device) (string, error) {
	network := nic["network"]
	if network == "" {
		return "", fmt.Errorf("%s's NIC is not on a managed network, so rig cannot pick a static address for it", inst.Name)
	}
	n, err := c.Network(network)
	if err != nil {
		return "", err
	}
	subnet := n.Config["ipv4.address"]
	if subnet == "" || subnet == "none" {
		return "", fmt.Errorf("network %s has no IPv4 subnet to pick a static address from", network)
	}
	instances, err := c.Instances()
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, other := range instances {
		if other.Name == inst.Name {
			continue
		}
		for _, dev := range other.NICs() {
			if a := dev["ipv4.address"]; a != "" {
				taken[a] = true
			}
		}
		// Whatever a running guest currently leases counts too: a static
		// assignment colliding with a live lease is two guests on one address.
		if other.Running() {
			if addr, err := c.GlobalIPv4(other.Name); err == nil && addr != "" {
				taken[addr] = true
			}
		}
	}
	return PickAddress(inst.Name, subnet, taken)
}

// --- helpers -------------------------------------------------------------

func oneNIC(inst *incus.Instance) (string, incus.Device) {
	nics := inst.NICs()
	for _, name := range sortedKeys(nics) {
		return name, nics[name]
	}
	return "", nil
}

func describe(ports []manifest.Port) string {
	var parts []string
	for _, p := range ports {
		parts = append(parts, p.Target+"/"+p.Protocol)
	}
	return strings.Join(parts, ", ")
}

func policyHasACL(list, want string) bool {
	for _, a := range strings.Split(list, ",") {
		if strings.TrimSpace(a) == want {
			return true
		}
	}
	return false
}

func addACL(list, name string) string {
	if list == "" {
		return name
	}
	return list + "," + name
}

func equal(a, b incus.Device) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
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
