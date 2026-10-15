package proxy

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/tbrockman/rig/internal/policy"
)

// --- the host's own firewall ---------------------------------------------

// A guest talking to its proxy talks to the host's own address on the
// bridge, which is INPUT on the host, where ufw's default policy drops it.
// Incus's rules for the bridge do not help: an accept in one chain does not
// stop a drop in another. So the ports a guest may reach need a rule of the
// host's, and that is root's to add: `rig proxy setup` adds it under sudo,
// printing the rule first, as the host verbs do with what they run as root.

// ufwArgs is the rule that admits the guests' proxy ports on the bridge, and
// nothing else there: one rule for the whole range, once, rather than one
// per guest.
func ufwArgs() []string {
	return []string{"ufw", "allow", "in", "on", policy.ProxyNetwork, "to", policy.ProxyAddr, "port", policy.ProxyPortRange(), "proto", "tcp"}
}

// UFWRule is that rule as the operator would type it.
func UFWRule() string { return "sudo " + strings.Join(ufwArgs(), " ") }

// AllowUFW adds the rule under sudo, printing it first so the line is the
// whole truth about what runs as root. ufw skips a rule it already has, so
// this is safe to repeat, and repeat it must: ufw's own status is root's to
// read, so rig cannot check first.
func AllowUFW() error {
	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("admitting the guests' proxy ports needs root, but sudo is not on PATH.\n"+
			"  Run it as root:  %s", strings.Join(ufwArgs(), " "))
	}
	fmt.Fprintf(os.Stderr, "rig: this needs root. running: %s\n", UFWRule())
	cmd := exec.Command("sudo", append([]string{"--"}, ufwArgs()...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", UFWRule(), err)
	}
	return nil
}

// RecentlyBlocked counts packets from a guest to a proxy that ufw logged as
// dropped in the last ten minutes. The kernel log is readable without root,
// which ufw's own status is not, so this is the evidence rig can get.
func RecentlyBlocked() int {
	out, err := exec.Command("journalctl", "-k", "--no-pager", "--since", "-10 minutes").Output()
	if err != nil {
		return 0
	}
	return countBlocked(strings.Split(string(out), "\n"))
}

func countBlocked(lines []string) int {
	n := 0
	for _, line := range lines {
		if !strings.Contains(line, "UFW BLOCK") ||
			!strings.Contains(line, "IN="+policy.ProxyNetwork+" ") ||
			!strings.Contains(line, "DST="+policy.ProxyAddr+" ") {
			continue
		}
		_, rest, ok := strings.Cut(line, "DPT=")
		if !ok {
			continue
		}
		port, _, _ := strings.Cut(rest, " ")
		if p, err := strconv.Atoi(port); err == nil && p >= policy.ProxyPortBase+2 && p <= policy.ProxyPortBase+254 {
			n++
		}
	}
	return n
}
