package policy

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/tbrockman/rig/internal/incus"
)

// The proxy network. A guest whose manifest proxies credentials gets a second
// NIC, on a bridge of its own: no NAT, no routing, no DNS, and an ACL on the
// NIC that allows one thing, TCP to that guest's own proxy on the bridge
// address. The isolation ACL stays on the first NIC, untouched. On this one
// there is nothing to isolate from, because the host offers nothing there
// but the proxies, and the host's own firewall for the bridge is Incus's.
//
// A second NIC rather than a hole in the first, because Incus evaluates an
// ACL's rejects before its allows across every ACL on a NIC: an allow for one
// address inside a rejected range is shadowed. A NIC of its own has an ACL of
// its own, with no reject in it to shadow anything.
//
// Each guest has a proxy of its own: a witmproxy instance on the host (one
// process, one database, one CA, one policy) listening on the bridge address
// at a port derived from the guest's address, so nothing needs recording:
// 10.207.0.<n> reaches 10.207.0.1:<ProxyPortBase+n> and nothing else, and
// rig reaches that proxy's management API at 127.0.0.1:<APIPortBase+n>. The
// guest's address is set in the guest by rig at each start and Incus, with
// ipv4_filtering, drops anything the guest sends from another one. (Incus
// would also lease it by DHCP, but a host firewall that knows nothing of the
// bridge drops that exchange, and a route is the last thing wanted on it.)
const (
	ProxyNetwork = "rigproxy0"
	ProxyAddr    = "10.207.0.1"
	ProxyPrefix  = 24
	// ProxyDevice is the NIC's name on the instance; ProxyGuestNIC its name
	// inside the guest, which the base image's udev rule gives it by MAC.
	ProxyDevice   = "proxy"
	ProxyGuestNIC = "proxy0"
	// proxyHWPrefix is locally administered ("RIG"); the last octet is the
	// address's, so the MAC says what the IP says.
	proxyHWPrefix = "02:52:49:47:00:"
	// ProxyPortBase + the guest's last octet is its proxy's port on the
	// bridge address; APIPortBase + the same is that proxy's management API
	// on the host's loopback. Guests are .2 to .254.
	ProxyPortBase  = 3100
	APIPortBase    = 3400
	proxyACLPrefix = "rig-proxy-"
)

// lastOctet of an IPv4 address, 0 for anything else.
func lastOctet(ip string) int {
	v4 := net.ParseIP(ip).To4()
	if v4 == nil {
		return 0
	}
	return int(v4[3])
}

// ProxyPortOf is the port of the guest at ip's proxy, on the bridge address.
func ProxyPortOf(ip string) int { return ProxyPortBase + lastOctet(ip) }

// APIPortOf is the port of that proxy's management API, on loopback.
func APIPortOf(ip string) int { return APIPortBase + lastOctet(ip) }

// ProxyPortRange is every port a guest's proxy may use, as ufw spells a
// range.
func ProxyPortRange() string {
	return fmt.Sprintf("%d:%d", ProxyPortBase+2, ProxyPortBase+254)
}

// ProxyURLFor is what the guest at ip's HTTPS_PROXY names.
func ProxyURLFor(ip string) string {
	return fmt.Sprintf("http://%s:%d", ProxyAddr, ProxyPortOf(ip))
}

// ProxySubnet is the bridge's network, for a human.
func ProxySubnet() string {
	ip := net.ParseIP(ProxyAddr).To4()
	return fmt.Sprintf("%s/%d", ip.Mask(net.CIDRMask(ProxyPrefix, 32)), ProxyPrefix)
}

// DesiredProxyNetwork is the bridge: an address for the proxies to listen
// on, and nothing else. No NAT and no routing, so even without the ACL a
// packet for anywhere but the bridge goes nowhere.
func DesiredProxyNetwork() *incus.Network {
	return &incus.Network{
		Name:        ProxyNetwork,
		Type:        "bridge",
		Description: "rig: the guests' way to their host proxies, and to nothing else",
		Config: map[string]string{
			"ipv4.address": fmt.Sprintf("%s/%d", ProxyAddr, ProxyPrefix),
			"ipv4.nat":     "false",
			"ipv4.routing": "false",
			"ipv4.dhcp":    "true",
			"ipv6.address": "none",
			"dns.mode":     "none",
		},
	}
}

// ProxyACLName is the ACL on one guest's proxy NIC.
func ProxyACLName(vm string) string { return proxyACLPrefix + vm }

// IsProxyACL reports whether an ACL name is one of rig's proxy ACLs, and
// whose.
func IsProxyACL(name string) (vm string, ok bool) {
	return strings.CutPrefix(name, proxyACLPrefix)
}

// DesiredProxyACL allows one guest's own proxy and nothing else. No DHCP:
// the guest's address is pinned to the NIC by the host and set by rig, not
// negotiated.
func DesiredProxyACL(vm, ip string) *incus.ACL {
	return &incus.ACL{
		Name:        ProxyACLName(vm),
		Description: "rig: " + vm + "'s proxy NIC reaches its own proxy and nothing else",
		Ingress:     []incus.ACLRule{},
		Egress: []incus.ACLRule{{
			Action:          "allow",
			Destination:     ProxyAddr + "/32",
			Protocol:        "tcp",
			DestinationPort: fmt.Sprint(ProxyPortOf(ip)),
			State:           "enabled",
		}},
		Config: map[string]string{},
	}
}

// ProxyNIC is the instance device for one guest at one address.
func ProxyNIC(vm, ip string) incus.Device {
	return incus.Device{
		"type":                                 "nic",
		"network":                              ProxyNetwork,
		"name":                                 ProxyGuestNIC,
		"hwaddr":                               ProxyHWAddr(ip),
		"ipv4.address":                         ip,
		"security.ipv4_filtering":              "true",
		"security.mac_filtering":               "true",
		"security.acls":                        ProxyACLName(vm),
		"security.acls.default.egress.action":  "reject",
		"security.acls.default.ingress.action": "reject",
	}
}

// ProxyHWAddr derives the NIC's MAC from its address.
func ProxyHWAddr(ip string) string {
	v4 := net.ParseIP(ip).To4()
	if v4 == nil {
		return proxyHWPrefix + "00"
	}
	return fmt.Sprintf("%s%02x", proxyHWPrefix, v4[3])
}

// IsProxyNIC reports whether a device is a NIC on the proxy bridge: that is
// what makes it the proxy NIC, whatever it is called.
func IsProxyNIC(dev incus.Device) bool {
	return dev.Type() == "nic" && dev["network"] == ProxyNetwork
}

// ProxyIPOf is a guest's address on the bridge, or "" when it has no proxy
// NIC.
func ProxyIPOf(inst *incus.Instance) string {
	if dev, ok := inst.Devices[ProxyDevice]; ok {
		return dev["ipv4.address"]
	}
	for _, dev := range inst.ExpandedDevices {
		if IsProxyNIC(dev) {
			return dev["ipv4.address"]
		}
	}
	return ""
}

// AllocateProxyIP picks the lowest free address on the bridge, given the ones
// in use.
func AllocateProxyIP(used map[string]bool) (string, error) {
	base := net.ParseIP(ProxyAddr).To4()
	for n := 2; n <= 254; n++ {
		ip := fmt.Sprintf("%d.%d.%d.%d", base[0], base[1], base[2], n)
		if ip != ProxyAddr && !used[ip] {
			return ip, nil
		}
	}
	return "", fmt.Errorf("no address left on %s: %d guests already have one", ProxyNetwork, len(used))
}

// UsedProxyIPs collects every instance's address on the bridge.
func UsedProxyIPs(instances []incus.Instance) map[string]bool {
	used := map[string]bool{}
	for i := range instances {
		if ip := ProxyIPOf(&instances[i]); ip != "" {
			used[ip] = true
		}
	}
	return used
}

// ApplyProxy reconciles the bridge, like Apply does the isolation ACL and
// the profile. A bridge that exists with other settings is reported and
// left alone: it is someone's decision, and changing a network under
// running guests is not this verb's to make. The ACLs are per guest and
// made with the NIC (EnsureProxyACL).
func ApplyProxy(c *incus.Client, dryRun bool) ([]string, error) {
	var changes []string
	want := DesiredProxyNetwork()
	have, err := c.Network(ProxyNetwork)
	switch {
	case err != nil:
		changes = append(changes, fmt.Sprintf("create network %s (%s, no NAT, no routing)", ProxyNetwork, ProxySubnet()))
		if !dryRun {
			if err := c.CreateNetwork(want); err != nil {
				return changes, fmt.Errorf("creating network %s: %w", ProxyNetwork, err)
			}
		}
	default:
		var diffs []string
		for _, k := range sortedKeys(want.Config) {
			if have.Config[k] != want.Config[k] {
				diffs = append(diffs, fmt.Sprintf("%s=%q (rig wants %q)", k, have.Config[k], want.Config[k]))
			}
		}
		if len(diffs) > 0 {
			changes = append(changes, "WARNING: network "+ProxyNetwork+" exists with other settings, left alone: "+strings.Join(diffs, ", "))
		}
	}
	sort.Strings(changes)
	return changes, nil
}

// EnsureProxyACL makes one guest's proxy ACL what rig wants: created when
// missing, rewritten when it says anything else. Before the NIC that
// references it is added.
func EnsureProxyACL(c *incus.Client, vm, ip string, dryRun bool) ([]string, error) {
	want := DesiredProxyACL(vm, ip)
	name := want.Name
	cur, etag, err := c.ACL(name)
	switch {
	case err != nil:
		change := fmt.Sprintf("create ACL %s (allow tcp to %s:%d, nothing else)", name, ProxyAddr, ProxyPortOf(ip))
		if dryRun {
			return []string{change}, nil
		}
		if err := c.CreateACL(want); err != nil {
			return nil, fmt.Errorf("creating ACL %s: %w", name, err)
		}
		return []string{change}, nil
	case RuleKey(cur.Egress) != RuleKey(want.Egress) || RuleKey(cur.Ingress) != RuleKey(want.Ingress):
		change := fmt.Sprintf("rewrite ACL %s to rig's one rule (allow tcp to %s:%d)", name, ProxyAddr, ProxyPortOf(ip))
		if dryRun {
			return []string{change}, nil
		}
		return []string{change}, RewriteACL(c, name, want, etag, len(cur.UsedBy) > 0)
	}
	return nil, nil
}

// DeleteProxyACL removes one guest's proxy ACL, once nothing references it.
// None to remove is not an error.
func DeleteProxyACL(c *incus.Client, vm string) error {
	name := ProxyACLName(vm)
	cur, _, err := c.ACL(name)
	if err != nil {
		return nil
	}
	if len(cur.UsedBy) > 0 {
		return fmt.Errorf("ACL %s is still in use by %s", name, strings.Join(cur.UsedBy, ", "))
	}
	return c.DeleteACL(name)
}
