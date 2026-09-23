// Package policy holds the declared intent for guest isolation and reconciles
// Incus to it. `incus admin init --preseed` does not cover network ACLs, so
// this is the source of truth for the policy; Incus stays the source of truth
// for state.
package policy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tbrockman/rig/internal/incus"
)

// RejectRanges is IPv4-only by decision. The guest bridge is expected to run
// with ipv6.address=none, and `rig verify` fails a guest that holds a global
// IPv6 address, because a denylist cannot be extended to IPv6 — that needs the
// opposite posture (default-deny plus an allowlist). See docs/DESIGN.md.
var RejectRanges = []string{
	"10.0.0.0/8",     // RFC1918. Where Incus puts its bridge by default.
	"172.16.0.0/12",  // RFC1918. Where docker puts its bridges by default.
	"192.168.0.0/16", // RFC1918. Most home and office LANs.
	"169.254.0.0/16", // Link-local, and every cloud metadata address.
	"100.64.0.0/10",  // CGNAT, which Tailscale uses. NOT covered by RFC1918.
}

const aclDescription = "Guest isolation: public internet only (IPv4 denylist)"

// DefaultProfile is the profile rig's VMs inherit their NIC and root disk
// from, and the one `rig setup` puts the isolation on. rig's own rather than
// Incus's `default`, so installing rig changes nothing about instances it did
// not create.
const DefaultProfile = "rig"

// seedProfile is where a missing rig profile takes its NIC and root disk from:
// the bridge and storage pool the host already chose.
const seedProfile = "default"

// Seed is the device set a new rig profile starts from: the NICs and root
// disk of seed, and nothing else. A passthrough device someone put on
// `default` must not follow every rig VM.
func Seed(seed map[string]incus.Device) map[string]incus.Device {
	out := map[string]incus.Device{}
	for name, dev := range seed {
		if dev.Type() == "nic" || (dev.Type() == "disk" && dev["path"] == "/") {
			out[name] = cloneDevice(dev)
		}
	}
	return out
}

func cloneDevice(d incus.Device) incus.Device {
	out := incus.Device{}
	for k, v := range d {
		out[k] = v
	}
	return out
}

// NICKeys are the three settings that make the policy. All three matter:
// attaching an ACL makes Incus default to *reject* in both directions, so a
// denylist ACL without egress.action=allow is a blackout — and a quiet one,
// because Incus's own DHCP/DNS rules keep the bridge resolver working.
func NICKeys(acl string) map[string]string {
	return map[string]string{
		"security.acls":                        acl,
		"security.acls.default.egress.action":  "allow",
		"security.acls.default.ingress.action": "reject",
	}
}

func DesiredACL(name string) *incus.ACL {
	egress := make([]incus.ACLRule, 0, len(RejectRanges))
	for _, cidr := range RejectRanges {
		egress = append(egress, incus.ACLRule{
			Action: "reject", Destination: cidr, State: "enabled",
		})
	}
	return &incus.ACL{
		Name:        name,
		Description: aclDescription,
		Ingress:     []incus.ACLRule{},
		Egress:      egress,
		Config:      map[string]string{},
	}
}

// Report classifies one instance's NICs.
//
// unisolated: no isolation ACL, so the guest can reach this host and the LAN.
// noEgress:   ACL attached but the egress default is not allow, so the guest has
// no internet at all — broken rather than unsafe.
func Report(inst *incus.Instance, acl string) (unisolated, noEgress []string) {
	for name, dev := range inst.NICs() {
		if !hasACL(dev["security.acls"], acl) {
			unisolated = append(unisolated, name)
		} else if dev["security.acls.default.egress.action"] != "allow" {
			noEgress = append(noEgress, name)
		}
	}
	sort.Strings(unisolated)
	sort.Strings(noEgress)
	return
}

func Isolated(inst *incus.Instance, acl string) bool {
	unisolated, _ := Report(inst, acl)
	return len(unisolated) == 0
}

func hasACL(list, want string) bool {
	for _, a := range strings.Split(list, ",") {
		if strings.TrimSpace(a) == want {
			return true
		}
	}
	return false
}

// --- reconcile -----------------------------------------------------------

// Apply reconciles the ACL object and the profile NIC. Returns the changes it
// made (or would make, when dryRun).
func Apply(c *incus.Client, aclName, profileName string, dryRun bool) ([]string, error) {
	var changes []string

	names, err := c.ACLNames()
	if err != nil {
		return nil, err
	}
	want := DesiredACL(aclName)

	if !slicesContains(names, aclName) {
		changes = append(changes, fmt.Sprintf("create ACL %q (%d egress rejects)", aclName, len(want.Egress)))
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
		if diff := ruleDiff(cur, want); diff != "" {
			changes = append(changes, fmt.Sprintf("rewrite ACL %q rules (%s)", aclName, diff))
			if !dryRun {
				if err := RewriteACL(c, aclName, want, etag, len(cur.UsedBy) > 0); err != nil {
					return changes, err
				}
			}
		}
	}

	profiles, err := c.Profiles()
	if err != nil {
		return changes, err
	}
	exists := false
	for _, p := range profiles {
		exists = exists || p.Name == profileName
	}
	if !exists {
		seed, _, err := c.Profile(seedProfile)
		if err != nil {
			return changes, fmt.Errorf("no profile %q to take the bridge and storage pool from: %w", seedProfile, err)
		}
		devs := Seed(seed.Devices)
		changes = append(changes, fmt.Sprintf("create profile %q with %s's NIC and root disk", profileName, seedProfile))
		if dryRun {
			// Report the NIC keys the new profile would get, from the seed.
			changes = append(changes, nicChanges(profileName, cloneDevices(devs), aclName)...)
			return changes, nil
		}
		if err := c.CreateProfile(profileName, devs); err != nil {
			return changes, err
		}
	}

	prof, etag, err := c.Profile(profileName)
	if err != nil {
		return changes, err
	}
	devices := cloneDevices(prof.Devices)
	nicLines := nicChanges(profileName, devices, aclName)
	changes = append(changes, nicLines...)
	dirty := len(nicLines) > 0 && !strings.HasPrefix(nicLines[0], "WARNING")
	if dirty && !dryRun {
		if err := c.SetProfileDevices(profileName, prof, devices, etag); err != nil {
			return changes, err
		}
	}
	return changes, nil
}

// RewriteACL changes an ACL's rules.
//
// Incus refuses any rule change while the ACL is attached to something: it
// flushes an nftables chain named acl.<bridge> that it never creates (it uses
// fwd.<bridge>) and fails. So when the ACL is in use, detach it from every NIC,
// rewrite, and reattach. Consumers must be stopped — a running guest would be
// briefly unisolated otherwise, which is the exact failure this policy exists
// to prevent. Exported because a per-instance ACL (see internal/ports) has
// exactly the same constraint.
func RewriteACL(c *incus.Client, name string, want *incus.ACL, etag string, inUse bool) error {
	if !inUse {
		return c.PutACL(name, want, etag)
	}

	instances, err := c.Instances()
	if err != nil {
		return err
	}

	type attachment struct {
		instance string // empty means the profile
		profile  string
		nic      string
		value    string
	}
	var attached []attachment

	profiles, err := c.Profiles()
	if err != nil {
		return err
	}
	for _, p := range profiles {
		for nic, dev := range p.Devices {
			if dev.Type() == "nic" && hasACL(dev["security.acls"], name) {
				attached = append(attached, attachment{profile: p.Name, nic: nic, value: dev["security.acls"]})
			}
		}
	}
	for _, inst := range instances {
		for nic, dev := range inst.Devices {
			if dev.Type() == "nic" && hasACL(dev["security.acls"], name) {
				attached = append(attached, attachment{instance: inst.Name, nic: nic, value: dev["security.acls"]})
			}
		}
		if inst.Running() && hasACL(effectiveACLs(&inst), name) {
			return fmt.Errorf("cannot rewrite ACL %q while %s is running: Incus rejects rule "+
				"changes on an attached ACL, so it has to be detached first and that would "+
				"briefly unisolate a live guest.\n  Stop it:  rig stop %s",
				name, inst.Name, inst.Name)
		}
	}

	detach := func(a attachment) error {
		remaining := removeACL(a.value, name)
		if a.instance != "" {
			inst, _, err := c.Instance(a.instance)
			if err != nil {
				return err
			}
			devs := cloneDevices(inst.Devices)
			setOrDelete(devs[a.nic], "security.acls", remaining)
			return c.SetDevices(a.instance, devs)
		}
		p, etag, err := c.Profile(a.profile)
		if err != nil {
			return err
		}
		devs := cloneDevices(p.Devices)
		setOrDelete(devs[a.nic], "security.acls", remaining)
		return c.SetProfileDevices(a.profile, p, devs, etag)
	}
	reattach := func(a attachment) error {
		if a.instance != "" {
			inst, _, err := c.Instance(a.instance)
			if err != nil {
				return err
			}
			devs := cloneDevices(inst.Devices)
			devs[a.nic]["security.acls"] = a.value
			return c.SetDevices(a.instance, devs)
		}
		p, etag, err := c.Profile(a.profile)
		if err != nil {
			return err
		}
		devs := cloneDevices(p.Devices)
		devs[a.nic]["security.acls"] = a.value
		return c.SetProfileDevices(a.profile, p, devs, etag)
	}

	var detached []attachment
	restore := func() {
		for _, a := range detached {
			_ = reattach(a)
		}
	}
	for _, a := range attached {
		if err := detach(a); err != nil {
			restore()
			return fmt.Errorf("detaching %s from %s: %w", name, attachName(a), err)
		}
		detached = append(detached, a)
	}

	// Re-read: the ETag from before the detaches is stale.
	_, freshETag, err := c.ACL(name)
	if err != nil {
		restore()
		return err
	}
	if err := c.PutACL(name, want, freshETag); err != nil {
		restore()
		return err
	}
	for _, a := range detached {
		if err := reattach(a); err != nil {
			return fmt.Errorf("reattaching %s to %s: %w\n"+
				"THE ACL IS NOT ATTACHED THERE. Fix it before starting that instance.",
				name, attachName(a), err)
		}
	}
	return nil
}

func attachName(a struct {
	instance string
	profile  string
	nic      string
	value    string
}) string {
	if a.instance != "" {
		return "instance " + a.instance + "/" + a.nic
	}
	return "profile " + a.profile + "/" + a.nic
}

func effectiveACLs(inst *incus.Instance) string {
	var all []string
	for _, dev := range inst.NICs() {
		all = append(all, dev["security.acls"])
	}
	return strings.Join(all, ",")
}

func removeACL(list, name string) string {
	var keep []string
	for _, a := range strings.Split(list, ",") {
		if a = strings.TrimSpace(a); a != "" && a != name {
			keep = append(keep, a)
		}
	}
	return strings.Join(keep, ",")
}

func setOrDelete(dev incus.Device, key, value string) {
	if value == "" {
		delete(dev, key)
		return
	}
	dev[key] = value
}

// ruleDiff describes how cur differs from want, or "" if they match. Order is
// not significant: Incus returns rules in whatever order it stored them.
func ruleDiff(cur, want *incus.ACL) string {
	if RuleKey(cur.Egress) == RuleKey(want.Egress) && RuleKey(cur.Ingress) == RuleKey(want.Ingress) {
		return ""
	}
	have := map[string]bool{}
	for _, r := range cur.Egress {
		have[r.Destination] = true
	}
	var missing, extra []string
	for _, cidr := range RejectRanges {
		if !have[cidr] {
			missing = append(missing, cidr)
		}
	}
	wanted := map[string]bool{}
	for _, cidr := range RejectRanges {
		wanted[cidr] = true
	}
	for _, r := range cur.Egress {
		if !wanted[r.Destination] {
			extra = append(extra, r.Destination)
		}
	}
	sort.Strings(extra)
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "missing "+strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		parts = append(parts, "unexpected "+strings.Join(extra, ", "))
	}
	if len(parts) == 0 {
		parts = append(parts, "ingress differs")
	}
	return strings.Join(parts, "; ")
}

// RuleKey renders rules order-independently, for comparing two ACLs.
func RuleKey(rules []incus.ACLRule) string {
	keys := make([]string, 0, len(rules))
	for _, r := range rules {
		state := r.State
		if state == "" {
			state = "enabled"
		}
		keys = append(keys, strings.Join([]string{r.Action, r.Destination, r.Source, r.Protocol, r.DestinationPort, state}, "|"))
	}
	sort.Strings(keys)
	return strings.Join(keys, "\n")
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

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// nicChanges sets the isolation keys on every NIC in devices, in place, and
// describes each change; with no NIC at all it returns only a warning.
func nicChanges(profileName string, devices map[string]incus.Device, aclName string) []string {
	var changes []string
	nics := 0
	for _, nic := range sortedKeys(devices) {
		if devices[nic].Type() != "nic" {
			continue
		}
		nics++
		for _, key := range sortedKeys(NICKeys(aclName)) {
			val := NICKeys(aclName)[key]
			if devices[nic][key] != val {
				old := devices[nic][key]
				if old == "" {
					old = "<unset>"
				}
				changes = append(changes, fmt.Sprintf("profile %s/%s: %s %q -> %q", profileName, nic, key, old, val))
				devices[nic][key] = val
			}
		}
	}
	if nics == 0 {
		return []string{fmt.Sprintf("WARNING: profile %q has no NIC device, so nothing inherits the isolation", profileName)}
	}
	return changes
}
