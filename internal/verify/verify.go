// Package verify proves, against a live guest, that the isolation policy
// actually holds.
//
// This is the counterpart to `rig doctor`, not a duplicate of it. Doctor reads
// configuration and answers "is this VM set up the way it should be". Verify
// sends real packets and answers "is that setup actually true". Configuration
// can be right and the effect still absent — Incus's own ACL handling has
// produced exactly that here more than once.
//
// Two rules shape everything below, both learned from tests in this repo that
// passed while proving nothing:
//
//  1. A connection that fails because nothing is listening is not evidence of
//     blocking. Every block check proves the target is reachable FROM THE HOST
//     first, and reports unproven — never passed — when it is not.
//  2. A probe that could not run is not a block. A missing binary or an
//     unusable shell feature looks identical to a refused connection. Each
//     probe mechanism therefore has a positive control, and its block results
//     are only believed once that control passes.
package verify

import (
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/policy"
)

type Outcome string

const (
	Pass     Outcome = "pass"
	Fail     Outcome = "fail"
	Unproven Outcome = "unproven"
)

type Check struct {
	Label   string  `json:"label"`
	Outcome Outcome `json:"outcome"`
	Detail  string  `json:"detail,omitempty"`
	// Proves is the reject range this check covers, when a block here is
	// attributable to the ACL. Empty when it is not.
	Proves string `json:"proves,omitempty"`
	// Advisory marks a check that proves nothing either way — an outcome worth
	// regressing whose cause lies elsewhere. It is reported, but it neither
	// earns coverage nor makes the run inconclusive when it cannot run.
	Advisory bool `json:"advisory,omitempty"`
}

type RangeCoverage struct {
	Range        string   `json:"range"`
	Proofs       []string `json:"proofs"`
	Acknowledged bool     `json:"acknowledged"`
}

type Verdict string

const (
	Proven       Verdict = "proven"
	Violated     Verdict = "violated"
	Inconclusive Verdict = "inconclusive"
)

type Report struct {
	Instance string          `json:"instance"`
	ACL      string          `json:"acl"`
	Isolated bool            `json:"isolated"`
	Controls []Check         `json:"controls"`
	Checks   []Check         `json:"checks"`
	Coverage []RangeCoverage `json:"coverage"`
	Verdict  Verdict         `json:"verdict"`
	Summary  string          `json:"summary"`
}

// ExitCode separates "a property is broken" from "I could not tell", because
// acting on those differently is the whole point of tracking them apart.
func (r *Report) ExitCode() int {
	switch r.Verdict {
	case Violated:
		return 1
	case Inconclusive:
		return 2
	}
	return 0
}

// DefaultAllowGaps are reject ranges this project has accepted as unprovable.
//
// 169.254.0.0/16 is in the policy because it covers every cloud metadata
// address, but nothing on this host answers on it, so a guest failing to reach
// it demonstrates nothing. It is listed here rather than dropped from the
// coverage report: an acknowledged gap stays visible, an omitted one does not.
var DefaultAllowGaps = []string{"169.254.0.0/16"}

type Runner struct {
	C        *incus.Client
	Instance string
	ACL      string
	Bridge   string
	// AllowGaps are reject ranges accepted as unprovable on this host. They are
	// still reported; they just do not make the run inconclusive.
	AllowGaps []string
	Out       io.Writer

	report   Report
	working  map[Mechanism]bool
	coverage map[string][]string
	// guestOwn are addresses the guest holds itself. A probe at one of these
	// never leaves the guest, so it can neither prove nor disprove isolation.
	guestOwn map[string]bool
}

// guestAddresses reads the IPv4 addresses the guest holds on its own
// interfaces.
//
// This exists because "the host's addresses" and "the guest's addresses" can
// overlap, and when they do a probe silently changes meaning. Docker picks
// 172.17.0.1/16 for its default bridge on every machine, so a host running
// docker and a guest running docker hold the same address — and a guest
// probing "the host at 172.17.0.1:22" reaches its own sshd, one hop, never
// touching the wire. verify reported that as the guest reaching the host: the
// single most serious verdict it has, from a probe that proved nothing.
//
// Failing loudly was luck rather than design. The same collision on a target
// whose expectation was Reachable would have produced a comfortable PASS for a
// path that was never tested.
func guestAddresses(c *incus.Client, instance string) map[string]bool {
	out, err := c.Exec(instance, "ip -4 -o addr show", incus.ExecOpts{Timeout: 15 * time.Second})
	if err != nil {
		return nil
	}
	own := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f != "inet" || i+1 >= len(fields) {
				continue
			}
			if ip, _, ok := strings.Cut(fields[i+1], "/"); ok && ip != "" {
				own[ip] = true
			}
		}
	}
	return own
}

func (r *Runner) logf(format string, args ...any) {
	if r.Out != nil {
		fmt.Fprintf(r.Out, format+"\n", args...)
	}
}

func (r *Runner) Run() (*Report, error) {
	r.report.Instance = r.Instance
	r.report.ACL = r.ACL
	r.working = map[Mechanism]bool{}
	r.coverage = map[string][]string{}

	inst, _, err := r.C.Instance(r.Instance)
	if err != nil {
		return nil, err
	}
	if !inst.Running() {
		return nil, fmt.Errorf("%s is %s; verify needs a running guest to probe from",
			r.Instance, inst.Status)
	}
	if err := r.C.WaitAgent(r.Instance, 20*time.Second); err != nil {
		return nil, err
	}
	// And wait for an address. The agent answers several seconds before DHCP
	// finishes, and probing in that window reports every target as blocked —
	// including the ones that must be reachable, which reads as total isolation
	// rather than as a guest that is not on the network yet.
	if _, err := r.C.WaitAddress(r.Instance, 90*time.Second); err != nil {
		return nil, fmt.Errorf("%w\n  Nothing can be probed from a guest with no address.", err)
	}
	unisolated, noEgress := policy.Report(inst, r.ACL)
	r.report.Isolated = len(unisolated) == 0

	targets := discover(r.Bridge)
	r.guestOwn = guestAddresses(r.C, r.Instance)
	r.describe(inst, targets, unisolated, noEgress)

	r.logf("\n--- controls: each probe mechanism must be shown to work ---")
	r.logf("    (an unproven mechanism cannot distinguish blocked from broken)")
	r.controls()

	r.logf("\n--- the host itself, on every address it holds ---")
	r.logf("    (sshd listens on 0.0.0.0; reaching it is the breach that matters)")
	for _, addr := range targets.HostAddrs {
		r.check(Target{
			Label: "host " + addr + " tcp/22", Mechanism: TCP, IP: addr, Port: 22,
			Expect: Blocked, Attributable: true,
		})
	}

	r.logf("\n--- must be blocked ---")
	for _, t := range r.blockTargets(targets) {
		r.check(t)
	}

	r.logf("\n--- must be allowed (the guest is useless otherwise) ---")
	for _, t := range r.allowTargets(targets) {
		r.check(t)
	}
	r.guestChecks()

	r.logf("\n--- bypass ---")
	r.bypassChecks()

	r.finish()
	return &r.report, nil
}

func (r *Runner) describe(inst *incus.Instance, t Targets, unisolated, noEgress []string) {
	r.logf("instance     : %s (%s)", r.Instance, inst.Status)
	if len(unisolated) > 0 {
		r.logf("isolation    : NONE — %s carries no %s ACL", join(unisolated), r.ACL)
	} else if len(noEgress) > 0 {
		r.logf("isolation    : %s attached, but egress default is not allow on %s", r.ACL, join(noEgress))
	} else {
		r.logf("isolation    : %s", r.ACL)
	}
	r.logf("host addrs   : %s", firstNonEmpty(join(t.HostAddrs), "<none in a reject range>"))
	r.logf("lan gateway  : %s", firstNonEmpty(t.Gateway, "<none>"))
	r.logf("lan peer     : %s", firstNonEmpty(t.LANPeer, "<none reachable>"))
	r.logf("tailnet peer : %s", firstNonEmpty(t.Tailnet, "<none online>"))
	r.logf("bridge       : %s", firstNonEmpty(t.Bridge, "<unknown>"))
	r.logf("docker       : %d outcome-only target(s)", len(t.Docker))
	for _, n := range t.Notes {
		r.logf("note         : %s", n)
	}
}

// controls establish that each probe mechanism works at all, by using it
// against something that must be reachable. Public addresses are outside every
// reject range, so a working mechanism proves itself without weakening anything.
func (r *Runner) controls() {
	type control struct {
		label string
		m     Mechanism
		ip    string
		port  int
	}
	list := []control{
		{"tcp works (1.1.1.1:443)", TCP, "1.1.1.1", 443},
		{"icmp works (1.1.1.1)", ICMP, "1.1.1.1", 0},
	}
	// The bridge resolver is reachable by design — Incus inserts its DHCP/DNS
	// rules ahead of ACL rules — which makes it the only DNS target that can
	// serve as a control.
	if r.Bridge != "" {
		list = append(list, control{"dns works (bridge resolver)", DNS, r.Bridge, 0})
	}

	for _, c := range list {
		reach, detail := r.probeGuest(c.m, c.ip, c.port)
		ok := reach == Reachable
		r.working[c.m] = ok
		outcome := Pass
		if ok {
			detail = "reachable"
		} else {
			outcome = Unproven
			detail = firstNonEmpty(detail, "mechanism unusable: "+string(c.m)+" results cannot be trusted")
		}
		r.record(Check{Label: c.label, Outcome: outcome, Detail: detail}, true)
	}
	if r.Bridge == "" {
		r.record(Check{Label: "dns works (bridge resolver)", Outcome: Unproven,
			Detail: "no bridge address discovered"}, true)
	}
}

func (r *Runner) blockTargets(t Targets) []Target {
	var out []Target
	// A target that could not be derived is kept in the list rather than
	// dropped, so it is reported in place as unproven instead of vanishing.
	add := func(label string, m Mechanism, ip string, port int, attributable bool, note string) {
		out = append(out, Target{Label: label, Mechanism: m, IP: ip, Port: port,
			Expect: Blocked, Attributable: attributable, Note: note})
	}

	add("LAN gateway tcp/80", TCP, t.Gateway, 80, true, "")
	add("LAN gateway icmp", ICMP, t.Gateway, 0, true, "")
	add("LAN peer icmp", ICMP, t.LANPeer, 0, true, "")
	add("tailnet peer icmp", ICMP, t.Tailnet, 0, true, "")
	add("LAN gateway udp/53", DNS, t.Gateway, 0, true, "")
	add("link-local 169.254.169.254 tcp/80", TCP, "169.254.169.254", 80, true, "")

	// Docker installs FORWARD rules that already drop traffic arriving from
	// another bridge, so these stay blocked with the ACL removed. Worth
	// regressing as an outcome; never citable as evidence the ACL works.
	for _, d := range t.Docker {
		host, port, err := net.SplitHostPort(d)
		if err != nil {
			continue
		}
		p, err := strconv.Atoi(port)
		if err != nil {
			continue
		}
		add("docker "+d, TCP, host, p, false, "not ACL-attributable")
	}
	return out
}

func (r *Runner) allowTargets(t Targets) []Target {
	out := []Target{
		{Label: "public IPv4 tcp/443 (1.1.1.1)", Mechanism: TCP, IP: "1.1.1.1", Port: 443, Expect: Reachable},
	}
	if t.Bridge != "" {
		// Scoped exception, not a hole: udp/53 on the bridge is reachable by
		// design while tcp/22 on the same address is blocked above. The pairing
		// is what makes it scoped.
		out = append(out, Target{Label: "bridge resolver udp/53", Mechanism: DNS,
			IP: t.Bridge, Expect: Reachable})
	}
	return out
}

// check runs one target and records the result.
// acknowledged reports whether an address falls in a reject range the operator
// has already accepted as unprovable on this host.
//
// Such a check may still FAIL — a guest that reaches an acknowledged range has
// broken out, and that is not something an operator can wave through in
// advance. What the acknowledgement covers is the *unproven* outcome, which is
// the exact thing it was written to accept.
func (r *Runner) acknowledged(ip string) bool {
	rng := rangeContaining(ip)
	return rng != "" && slices.Contains(r.AllowGaps, rng)
}

func (r *Runner) check(t Target) {
	// An unproven result inside an acknowledged range is the acknowledgement
	// being used, not a new problem. Counting it anyway made --allow-gap
	// self-defeating: the run stayed INCONCLUSIVE for precisely the reason the
	// operator had already accepted, so the flag could never let anything pass.
	//
	// This went unnoticed because the acknowledged range had a real proof on the
	// day the suite was written — something on this host answered on
	// 169.254.169.254:80 — so the accepting path was never reached. It stopped
	// answering, and a flag that had always been decoration became visible.
	advisory := !t.Attributable || r.acknowledged(t.IP)

	if t.IP == "" {
		r.record(Check{Label: t.Label, Outcome: Unproven, Advisory: !t.Attributable,
			Detail: "no target could be derived on this host"}, false)
		return
	}
	// An address the guest also holds is not a target. The probe would loop
	// back inside the guest and report on the guest's own listeners, which says
	// nothing about what the ACL does or does not let out.
	if r.guestOwn[t.IP] {
		r.record(Check{Label: t.Label, Outcome: Unproven, Advisory: advisory,
			Detail: "the guest holds " + t.IP + " itself; a probe there never leaves the guest"}, false)
		return
	}
	// A block claim needs the mechanism to be known good, or "blocked" and
	// "broken" are indistinguishable.
	if t.Expect == Blocked && !r.working[t.Mechanism] {
		r.record(Check{Label: t.Label, Outcome: Unproven, Advisory: advisory,
			Detail: string(t.Mechanism) + " probes are not working; a block here would prove nothing"}, false)
		return
	}
	// And it needs the target to be reachable from here, or a failure in the
	// guest says nothing about the guest.
	if t.Expect == Blocked && !hostReaches(t.Mechanism, t.IP, t.Port) {
		r.record(Check{Label: t.Label, Outcome: Unproven, Advisory: advisory,
			Detail: "the host cannot reach it either — proves nothing"}, false)
		return
	}

	reach, detail := r.probeGuest(t.Mechanism, t.IP, t.Port)
	c := Check{Label: t.Label, Detail: detail, Advisory: advisory && t.Expect == Blocked}
	switch {
	case reach == Unknown:
		c.Outcome = Unproven
	case reach == t.Expect:
		c.Outcome = Pass
		c.Detail = reach.String()
		if detail != "" {
			c.Detail += " (" + detail + ")"
		}
		if t.Attributable && t.Expect == Blocked {
			c.Proves = rangeContaining(t.IP)
		}
	default:
		c.Outcome = Fail
		c.Detail = fmt.Sprintf("expected %s, got %s", t.Expect, reach)
	}
	if t.Note != "" {
		c.Detail += " [" + t.Note + "]"
	}
	r.record(c, false)
}

// guestChecks are properties of the guest itself rather than reachability of a
// particular address.
func (r *Runner) guestChecks() {
	r.guestAssert("DNS resolution", "getent hosts cache.nixos.org >/dev/null")
	r.guestAssert("public HTTPS", "curl -sSf -m 20 -o /dev/null https://cache.nixos.org/nix-cache-info")
	// `nix store info` is not a network test: it answers from the local store
	// and passed here once while HTTPS was fully blocked. A cache-busting query
	// string forces a real fetch, because that URL has never been seen before.
	r.guestAssert("nix fetches from the cache",
		`nix store prefetch-file --json "https://cache.nixos.org/nix-cache-info?rigverify=$RANDOM$RANDOM" >/dev/null`)
}

func (r *Runner) bypassChecks() {
	// The IPv4-only rules are sufficient only because the guest has no IPv6
	// stack. If a global v6 address ever appears, every rule above is
	// bypassable, and this is the check that says so.
	r.guestAssert("no global IPv6 address", `[ -z "$(ip -6 addr show scope global 2>/dev/null)" ]`)
	r.guestAssert("no IPv6 default route", `[ -z "$(ip -6 route show default 2>/dev/null)" ]`)
}

func (r *Runner) guestAssert(label, cmd string) {
	reach, detail := r.guest(cmd)
	switch reach {
	case Reachable:
		r.record(Check{Label: label, Outcome: Pass, Detail: "ok"}, false)
	case Unknown:
		r.record(Check{Label: label, Outcome: Unproven, Detail: detail}, false)
	default:
		r.record(Check{Label: label, Outcome: Fail, Detail: firstNonEmpty(detail, "command failed")}, false)
	}
}

func (r *Runner) record(c Check, control bool) {
	if control {
		r.report.Controls = append(r.report.Controls, c)
	} else {
		r.report.Checks = append(r.report.Checks, c)
	}
	if c.Proves != "" {
		r.coverage[c.Proves] = append(r.coverage[c.Proves], c.Label)
	}
	mark := map[Outcome]string{Pass: "PASS", Fail: "FAIL", Unproven: "????"}[c.Outcome]
	r.logf("  %s  %-42s %s", mark, c.Label, c.Detail)
}

// finish computes coverage and the verdict.
//
// Coverage is the check the old shell suite could not make: it discovered
// targets and the policy declared ranges, and nothing joined the two, so a
// target disappearing quietly reduced what was being proven while the summary
// still said everything passed.
func (r *Runner) finish() {
	for _, cidr := range policy.RejectRanges {
		r.report.Coverage = append(r.report.Coverage, RangeCoverage{
			Range:        cidr,
			Proofs:       r.coverage[cidr],
			Acknowledged: slices.Contains(r.AllowGaps, cidr),
		})
	}

	var failed, unproven, gaps int
	for _, c := range append(append([]Check{}, r.report.Controls...), r.report.Checks...) {
		switch {
		case c.Outcome == Fail:
			failed++
		case c.Outcome == Unproven && !c.Advisory:
			unproven++
		}
	}
	for _, cov := range r.report.Coverage {
		if len(cov.Proofs) == 0 && !cov.Acknowledged {
			gaps++
		}
	}

	switch {
	case failed > 0:
		r.report.Verdict = Violated
		r.report.Summary = fmt.Sprintf("%d check(s) failed", failed)
	case unproven > 0 || gaps > 0:
		r.report.Verdict = Inconclusive
		r.report.Summary = fmt.Sprintf("%d unproven, %d range(s) with no attributable proof", unproven, gaps)
	default:
		r.report.Verdict = Proven
		r.report.Summary = fmt.Sprintf("%d check(s) passed; every reject range proven or acknowledged",
			len(r.report.Checks)+len(r.report.Controls))
	}
}

// rangeContaining returns the declared reject range an address falls in, or "".
func rangeContaining(ip string) string {
	addr := net.ParseIP(ip)
	if addr == nil {
		return ""
	}
	for _, cidr := range policy.RejectRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil && network.Contains(addr) {
			return cidr
		}
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func join(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
