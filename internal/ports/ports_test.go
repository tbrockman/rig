package ports

import (
	"strings"
	"testing"

	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
)

func port(t *testing.T, s string) manifest.Port {
	t.Helper()
	p, err := manifest.ParsePort(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// One ACL rule per protocol, every port of that protocol in it. Incus takes
// ports and ranges as one comma-separated string, so that is the shape.
func TestDesiredACLGroupsPortsByProtocol(t *testing.T) {
	acl := DesiredACL("myproj", []manifest.Port{
		port(t, "47989:47989/tcp"), port(t, "48010:48010/udp"),
		port(t, "47998-48000:47998-48000/udp"), port(t, "47984:47984/tcp"),
	})
	if acl.Name != "rig-fwd-myproj" || len(acl.Egress) != 0 {
		t.Fatalf("acl = %+v", acl)
	}
	if len(acl.Ingress) != 2 {
		t.Fatalf("want one rule per protocol, got %+v", acl.Ingress)
	}
	tcp, udp := acl.Ingress[0], acl.Ingress[1]
	if tcp.Protocol != "tcp" || tcp.DestinationPort != "47984,47989" || tcp.Action != "allow" {
		t.Errorf("tcp rule = %+v", tcp)
	}
	if udp.Protocol != "udp" || udp.DestinationPort != "47998-48000,48010" {
		t.Errorf("udp rule = %+v", udp)
	}
	// The rule allows the guest-side port, since that is what the packet
	// carries after the NAT rewrite.
	remap := DesiredACL("x", []manifest.Port{port(t, "8080:80")})
	if remap.Ingress[0].DestinationPort != "80" {
		t.Errorf("remapped port allowed %s, want the target 80", remap.Ingress[0].DestinationPort)
	}
}

// Incus refuses a wildcard listen address in NAT mode, so the device names
// one host address: the manifest's when given, else the discovered one. The
// connect side stays a wildcard, which NAT mode fills with the guest's
// static address.
func TestProxyDeviceListensOnOneHostAddress(t *testing.T) {
	dev := ProxyDevice(port(t, "47998-48000:47998-48000/udp"), "192.168.18.2")
	if dev["type"] != "proxy" || dev["nat"] != "true" {
		t.Errorf("device = %v", dev)
	}
	if dev["listen"] != "udp:192.168.18.2:47998-48000" || dev["connect"] != "udp:0.0.0.0:47998-48000" {
		t.Errorf("listen/connect = %s / %s", dev["listen"], dev["connect"])
	}
	pinned := ProxyDevice(port(t, "10.0.0.5:8080:80"), "192.168.18.2")
	if pinned["listen"] != "tcp:10.0.0.5:8080" {
		t.Errorf("a host_ip in the manifest must win: %s", pinned["listen"])
	}
	if name := DeviceName(port(t, "47998-48000:47998-48000/udp")); name != "rig-port-udp-47998to48000" {
		t.Errorf("device name %q; Incus device names cannot carry a dash in a range", name)
	}
}

// The address is chosen inside the bridge's subnet, never the gateway, never
// one another instance pinned, and the same one every time for the same name.
func TestPickAddressIsStableAndAvoidsTakenOnes(t *testing.T) {
	first, err := PickAddress("myproj", "10.187.156.1/24", nil)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := PickAddress("myproj", "10.187.156.1/24", nil)
	if first != again {
		t.Errorf("not deterministic: %s then %s", first, again)
	}
	if !strings.HasPrefix(first, "10.187.156.") || first == "10.187.156.1" || strings.HasSuffix(first, ".0") || strings.HasSuffix(first, ".255") {
		t.Errorf("picked %s, outside the usable range", first)
	}
	moved, _ := PickAddress("myproj", "10.187.156.1/24", map[string]bool{first: true})
	if moved == first {
		t.Errorf("picked a taken address %s", moved)
	}
	other, _ := PickAddress("otherproj", "10.187.156.1/24", nil)
	if other == first {
		t.Logf("two names hashed to one address (%s); allowed, the taken set is what prevents a clash", other)
	}
	if _, err := PickAddress("x", "not a subnet", nil); err == nil {
		t.Error("a bad subnet was accepted")
	}
}

func TestParseRejectsACorruptRecord(t *testing.T) {
	if _, err := Parse([]string{"80:80/tcp", "nope"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v", err)
	}
	ps, err := Parse([]string{"80:80/tcp"})
	if err != nil || len(ps) != 1 || ps[0].Target != "80" {
		t.Errorf("ps = %v, %v", ps, err)
	}
}

// The ufw rules name the guest-side ports (what the packet carries after the
// NAT rewrite), one rule per protocol, ranges in ufw's colon form.
func TestUFWRulesNameTheGuestPortsPerProtocol(t *testing.T) {
	rules := UFWRules("10.187.156.198", []manifest.Port{
		port(t, "47989:47989/tcp"), port(t, "47998-48000:47998-48000/udp"), port(t, "8080:80"),
	})
	if len(rules) != 2 {
		t.Fatalf("rules = %v", rules)
	}
	if !strings.HasSuffix(rules[0], "to 10.187.156.198 port 47989,80") || !strings.Contains(rules[0], "proto tcp") {
		t.Errorf("tcp rule = %s", rules[0])
	}
	if !strings.HasSuffix(rules[1], "port 47998:48000") || !strings.Contains(rules[1], "proto udp") {
		t.Errorf("udp rule = %s", rules[1])
	}
}

// A port published on the host's Tailscale address is reachable by tailnet
// peers and nobody else, so the rule that admits it says so.
func TestUFWRulesScopeTailnetPortsToTheTailnet(t *testing.T) {
	rules := UFWRules("10.187.156.198", []manifest.Port{
		port(t, "100.64.0.7:47989:47989/tcp"), port(t, "100.64.0.7:47998-48000:47998-48000/udp"),
	})
	if len(rules) != 2 {
		t.Fatalf("rules = %v", rules)
	}
	for _, r := range rules {
		if !strings.Contains(r, "from 100.64.0.0/10 to 10.187.156.198") {
			t.Errorf("rule not scoped to the tailnet: %s", r)
		}
	}
}

// A VM whose profile NIC is masked with a `none` device has no network at all.
// With nothing to publish that is a deliberate state, and apply and doctor both
// reconcile through here, so it must not read as an error. Asking to publish
// onto it still is one.
func TestReconcileAcceptsAVMWithNoNIC(t *testing.T) {
	inst := &incus.Instance{
		Name: "daw",
		ExpandedDevices: map[string]incus.Device{
			"eth0": {"type": "none"},
			"root": {"type": "disk", "path": "/"},
		},
	}
	changes, err := Reconcile(nil, "vm-isolate", inst, nil, true)
	if err != nil || len(changes) != 0 {
		t.Fatalf("no NIC, no ports: got changes=%v err=%v, want nothing", changes, err)
	}
	if _, err := Reconcile(nil, "vm-isolate", inst, []manifest.Port{port(t, "8080:80/tcp")}, true); err == nil {
		t.Fatal("publishing a port onto a VM with no NIC must be refused")
	}
}
