// Package cpuset checks a pinned vCPU set against this host's CPUs.
//
// Pinning is not an isolation boundary: a guest's CPU use is capped by its
// vCPU count pinned or not, and an unpinned VM may run on any host CPU
// already. What pinning can do wrong is availability and latency. A set that
// covers every host CPU leaves the host no core of its own, which matters
// most if the vCPU threads are ever given real-time priority, the next
// latency step for a guest like a DAW: then a busy guest could starve the
// host. A set naming CPUs that do not exist fails at start with an unclear
// error. And a set taking one thread of a core leaves the guest sharing that
// core with host work, which undoes what pinning was for.
package cpuset

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Root is the sysfs CPU directory; a variable so tests can use a fake tree.
var Root = "/sys/devices/system/cpu"

// IsSet reports whether a limits.cpu value is a pinned set rather than a count.
func IsSet(limit string) bool {
	return strings.ContainsAny(limit, "-,") // a bare number is a count to Incus, never a CPU
}

// Parse expands "4-7,12-15" into its CPUs, sorted.
func Parse(set string) ([]int, error) {
	seen := map[int]bool{}
	for _, part := range strings.Split(set, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("%q is not a CPU set", set)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, fmt.Errorf("%q is not a CPU set", set)
			}
		}
		for n := a; n <= b; n++ {
			seen[n] = true
		}
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

// Check refuses a set that names a CPU this host does not have online, or
// leaves the host no whole physical core; it warns about a set that takes
// only one thread of a core.
func Check(set string) (warnings []string, err error) {
	cpus, err := Parse(set)
	if err != nil {
		return nil, err
	}
	online, err := readList(filepath.Join(Root, "online"))
	if err != nil {
		return nil, fmt.Errorf("cannot read this host's online CPUs: %w", err)
	}
	isOnline := map[int]bool{}
	for _, n := range online {
		isOnline[n] = true
	}
	pinned := map[int]bool{}
	for _, n := range cpus {
		if !isOnline[n] {
			return nil, fmt.Errorf("CPU %d in %q is not online on this host (online: %s)", n, set, strings.TrimSpace(read(filepath.Join(Root, "online"))))
		}
		pinned[n] = true
	}

	// Group the host's CPUs into physical cores by SMT siblings.
	cores := map[string][]int{}
	for _, n := range online {
		sib := strings.TrimSpace(read(filepath.Join(Root, fmt.Sprintf("cpu%d", n), "topology", "thread_siblings_list")))
		if sib == "" {
			sib = strconv.Itoa(n)
		}
		cores[sib] = append(cores[sib], n)
	}
	hostKeepsACore := false
	var split []string
	for sib, members := range cores {
		taken := 0
		for _, n := range members {
			if pinned[n] {
				taken++
			}
		}
		switch {
		case taken == 0:
			hostKeepsACore = true
		case taken < len(members):
			split = append(split, sib)
		}
	}
	if !hostKeepsACore {
		return nil, fmt.Errorf("%q leaves this host no physical core of its own; pin the guest to a subset "+
			"and keep at least one whole core (both of its threads) for the host", set)
	}
	sort.Strings(split)
	for _, sib := range split {
		warnings = append(warnings, fmt.Sprintf("the set takes only part of the core whose threads are %s, "+
			"so the guest shares that core with host work; pin both threads or neither", sib))
	}
	return warnings, nil
}

func read(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func readList(path string) ([]int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(strings.TrimSpace(string(b)))
}
