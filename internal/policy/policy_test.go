package policy

import (
	"strings"
	"testing"

	"rig/internal/incus"
)

func nic(acls, egress string) incus.Device {
	d := incus.Device{"type": "nic"}
	if acls != "" {
		d["security.acls"] = acls
	}
	if egress != "" {
		d["security.acls.default.egress.action"] = egress
	}
	return d
}

func TestReport(t *testing.T) {
	cases := []struct {
		name              string
		dev               incus.Device
		unisolated, noEg  int
	}{
		{"correct", nic("vm-isolate", "allow"), 0, 0},
		{"no acl", nic("", ""), 1, 0},
		{"other acl only", nic("something-else", "allow"), 1, 0},
		// The blackout case: attaching an ACL flips the NIC to default-reject,
		// so a denylist without egress=allow leaves the guest with no internet.
		{"acl but no egress", nic("vm-isolate", ""), 0, 1},
		{"acl with reject egress", nic("vm-isolate", "reject"), 0, 1},
		// A NIC may carry several ACLs; ours only has to be among them.
		{"acl in a list", nic("other,vm-isolate", "allow"), 0, 0},
		{"acl in a spaced list", nic("other, vm-isolate", "allow"), 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := &incus.Instance{ExpandedDevices: map[string]incus.Device{"eth0": tc.dev}}
			unisolated, noEgress := Report(inst, "vm-isolate")
			if len(unisolated) != tc.unisolated || len(noEgress) != tc.noEg {
				t.Fatalf("got unisolated=%v noEgress=%v, want %d/%d",
					unisolated, noEgress, tc.unisolated, tc.noEg)
			}
		})
	}
}

func TestReportIgnoresNonNICs(t *testing.T) {
	inst := &incus.Instance{ExpandedDevices: map[string]incus.Device{
		"root": {"type": "disk"},
		"gpu0": {"type": "gpu"},
	}}
	if unisolated, _ := Report(inst, "vm-isolate"); len(unisolated) != 0 {
		t.Fatalf("a diskless-nic instance should not report NICs: %v", unisolated)
	}
}

func TestRuleDiffIgnoresOrder(t *testing.T) {
	want := DesiredACL("vm-isolate")
	shuffled := DesiredACL("vm-isolate")
	e := shuffled.Egress
	e[0], e[len(e)-1] = e[len(e)-1], e[0]
	if diff := ruleDiff(shuffled, want); diff != "" {
		t.Fatalf("reordered rules should compare equal, got %q", diff)
	}
}

func TestRuleDiffNamesMissingAndExtra(t *testing.T) {
	want := DesiredACL("vm-isolate")
	cur := DesiredACL("vm-isolate")
	cur.Egress = cur.Egress[:len(cur.Egress)-1] // drop 100.64.0.0/10
	cur.Egress = append(cur.Egress, incus.ACLRule{
		Action: "reject", Destination: "198.18.0.0/15", State: "enabled",
	})

	diff := ruleDiff(cur, want)
	if !strings.Contains(diff, "missing 100.64.0.0/10") {
		t.Errorf("diff should name the missing range, got %q", diff)
	}
	if !strings.Contains(diff, "unexpected 198.18.0.0/15") {
		t.Errorf("diff should name the extra range, got %q", diff)
	}
}

// The Tailscale range is not covered by RFC1918 and was a real gap once.
func TestRejectRangesCoverCGNAT(t *testing.T) {
	for _, want := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"169.254.0.0/16", "100.64.0.0/10"} {
		found := false
		for _, have := range RejectRanges {
			if have == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from RejectRanges", want)
		}
	}
}

func TestRemoveACL(t *testing.T) {
	cases := map[string]string{
		"vm-isolate":              "",
		"a,vm-isolate":            "a",
		"vm-isolate,b":            "b",
		"a, vm-isolate ,b":        "a,b",
		"":                        "",
		"vm-isolate-v2":           "vm-isolate-v2", // prefix must not match
	}
	for in, want := range cases {
		if got := removeACL(in, "vm-isolate"); got != want {
			t.Errorf("removeACL(%q) = %q, want %q", in, got, want)
		}
	}
}
