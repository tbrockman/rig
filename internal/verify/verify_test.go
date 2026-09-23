package verify

import (
	"regexp"
	"testing"
)

func TestHexLEToIP(t *testing.T) {
	// /proc/net/route prints a big-endian address as %08X of its native u32, so
	// on a little-endian host the bytes come out reversed. Getting this backwards
	// produced a gateway of 1.18.168.192 that happened to be a live public
	// address answering on port 80 — a check that ran, passed, and meant nothing.
	cases := map[string]string{
		"0112A8C0": "192.168.18.1",
		"010011AC": "172.17.0.1",
		"00000000": "",
		"":         "",
		"zzzz":     "",
	}
	for in, want := range cases {
		if got := hexLEToIP(in); got != want {
			t.Errorf("hexLEToIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRangeContaining(t *testing.T) {
	cases := map[string]string{
		"10.187.156.1":    "10.0.0.0/8",
		"172.17.0.1":      "172.16.0.0/12",
		"192.168.18.2":    "192.168.0.0/16",
		"169.254.169.254": "169.254.0.0/16",
		"100.64.0.7":      "100.64.0.0/10", // CGNAT: not covered by RFC1918
		"1.1.1.1":         "",
		"172.32.0.1":      "", // just outside 172.16/12
		"100.128.0.1":     "", // just outside 100.64/10
		"not-an-ip":       "",
	}
	for in, want := range cases {
		if got := rangeContaining(in); got != want {
			t.Errorf("rangeContaining(%q) = %q, want %q", in, got, want)
		}
	}
}

// The guest probe interpolates this into bash's printf. \x consumes up to two
// hex digits, so a one-digit escape would swallow the character after it:
// \x7example would be byte 0x7e followed by "xample".
func TestDNSQueryEscapedIsUnambiguous(t *testing.T) {
	got := dnsQueryEscaped()
	if !regexp.MustCompile(`^(\\x[0-9a-f]{2})+$`).MatchString(got) {
		t.Fatalf("escaped query has a non two-digit escape: %s", got)
	}
	if len(got) != len(dnsQuery)*4 {
		t.Errorf("escaped %d chars for %d bytes; want %d", len(got), len(dnsQuery), len(dnsQuery)*4)
	}
}

func TestVerdict(t *testing.T) {
	proof := Check{Outcome: Pass, Proves: "10.0.0.0/8"}
	covering := func() []Check {
		var out []Check
		for _, cidr := range []string{
			"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
			"169.254.0.0/16", "100.64.0.0/10",
		} {
			out = append(out, Check{Outcome: Pass, Proves: cidr})
		}
		return out
	}

	all := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}

	cases := []struct {
		name       string
		checks     []Check
		candidates []string // ranges some target on the host fell in
		allowGaps  []string
		want       Verdict
	}{
		{"everything proven", covering(), all, nil, Proven},
		{
			"a failure outranks everything",
			append(covering(), Check{Outcome: Fail}, Check{Outcome: Unproven}),
			all, nil, Violated,
		},
		{
			"an unproven check is not a pass",
			append(covering(), Check{Outcome: Unproven}),
			all, nil, Inconclusive,
		},
		{
			// Docker's container targets are the case: they stay blocked with the
			// ACL removed, so they prove nothing, and losing one should not make
			// the run look less conclusive than it is.
			"an advisory check that could not run does not downgrade the verdict",
			append(covering(), Check{Outcome: Unproven, Advisory: true}),
			all, nil, Proven,
		},
		{
			// A target fell in the range and produced no proof: the thing that
			// happened when 169.254.169.254 stopped answering. That is a gap.
			"a range whose target produced no proof is a gap",
			[]Check{proof},
			all, nil, Inconclusive,
		},
		{
			"an acknowledged gap is not",
			[]Check{proof},
			all, []string{"172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"},
			Proven,
		},
		{
			// A host with no Tailscale and no docker has nothing in 100.64/10 or
			// 172.16/12. There is nothing there for the guest to reach, so there
			// is nothing to prove, and calling that unproven made verify
			// inconclusive on every host but the one it was written on.
			"a range nothing on this host is in is moot, not a gap",
			[]Check{proof},
			[]string{"10.0.0.0/8"}, nil, Proven,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{AllowGaps: tc.allowGaps, coverage: map[string][]string{}, candidates: map[string]bool{}}
			for _, cidr := range tc.candidates {
				r.candidates[cidr] = true
			}
			for _, c := range tc.checks {
				r.report.Checks = append(r.report.Checks, c)
				if c.Proves != "" {
					r.coverage[c.Proves] = append(r.coverage[c.Proves], c.Label)
				}
			}
			r.finish()
			if r.report.Verdict != tc.want {
				t.Errorf("verdict = %s (%s), want %s",
					r.report.Verdict, r.report.Summary, tc.want)
			}
			if r.report.ExitCode() == 0 && tc.want != Proven {
				t.Errorf("exit code 0 for a %s verdict", tc.want)
			}
		})
	}
}

// --allow-gap was decoration until this: a range acknowledged as unprovable
// still produced a non-advisory Unproven check, so the run stayed INCONCLUSIVE
// for exactly the reason the operator had already accepted. It went unnoticed
// because the acknowledged range happened to have a real proof on the day the
// suite was written.
func TestAcknowledgedRangeDoesNotKeepTheRunInconclusive(t *testing.T) {
	r := &Runner{AllowGaps: DefaultAllowGaps, coverage: map[string][]string{}, candidates: map[string]bool{}}
	if !r.acknowledged("169.254.169.254") {
		t.Fatal("an address in an acknowledged range must be treated as acknowledged")
	}
	if r.acknowledged("10.187.156.1") {
		t.Error("10.0.0.0/8 is not in DefaultAllowGaps; it must still bind")
	}
	if r.acknowledged("1.1.1.1") {
		t.Error("an address in no reject range is not acknowledged")
	}
}

// Acknowledging a range accepts "we could not prove this", never "the guest may
// reach it". A breach into an acknowledged range is still a breach.
func TestAcknowledgementDoesNotExcuseAFailure(t *testing.T) {
	r := &Runner{
		AllowGaps:  DefaultAllowGaps,
		coverage:   map[string][]string{},
		candidates: map[string]bool{},
		report: Report{Checks: []Check{{
			Label: "link-local 169.254.169.254 tcp/80", Outcome: Fail, Advisory: true,
		}}},
	}
	r.finish()
	if r.report.Verdict != Violated {
		t.Fatalf("verdict = %v, want Violated: an advisory check that FAILS is still a breach",
			r.report.Verdict)
	}
}

// A target that could not be derived — no Tailscale peer on a host without
// Tailscale, no neighbour that answers — is nothing to probe, not a failure to
// prove. It is reported, and it does not make the run inconclusive; a target
// that exists and cannot be reached from the host still does.
func TestATargetThatDoesNotExistHereIsAdvisory(t *testing.T) {
	r := &Runner{AllowGaps: nil, coverage: map[string][]string{}, candidates: map[string]bool{},
		working: map[Mechanism]bool{ICMP: true}}
	r.check(Target{Label: "tailnet peer icmp", Mechanism: ICMP, IP: "", Expect: Blocked, Attributable: true})
	if got := r.report.Checks[0]; got.Outcome != Unproven || !got.Advisory {
		t.Fatalf("an underivable target must be advisory-unproven, got %+v", got)
	}
	if r.candidates["100.64.0.0/10"] {
		t.Error("an underivable target is not a candidate; its range is moot, not a gap")
	}
}
