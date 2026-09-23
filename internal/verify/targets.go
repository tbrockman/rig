package verify

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tbrockman/rig/internal/incus"
)

// A Target is one thing to probe, and the claim being made about it.
//
// Attributable records whether a block can be credited to the isolation ACL.
// Some targets are unreachable for reasons of their own — Docker drops
// cross-bridge traffic in its own FORWARD rules — and those prove the outcome
// without proving the cause, so they earn no coverage.
type Target struct {
	Label        string
	Mechanism    Mechanism
	IP           string
	Port         int
	Expect       Reach
	Attributable bool
	Note         string
}

// Targets are discovered from the live host rather than hardcoded: an address
// that rots turns a real test into a permanent skip.
type Targets struct {
	HostAddrs []string // host addresses inside a reject range
	Gateway   string
	LANPeer   string
	Tailnet   string
	Bridge    string
	Docker    []string // "ip:port"
	Notes     []string
}

func discover(bridgeAddr string) Targets {
	t := Targets{Bridge: bridgeAddr}

	iface, gw := defaultRoute()
	t.Gateway = gw
	if gw == "" {
		t.Notes = append(t.Notes, "no default route: the LAN gateway checks cannot run")
	}

	t.HostAddrs = hostAddressesInRejectRanges()
	t.LANPeer = lanPeer(iface, gw)
	t.Tailnet = tailnetPeer(t.HostAddrs)
	t.Docker = dockerTargets()
	return t
}

// hostAddressesInRejectRanges finds every address this host holds that falls
// inside a range the policy rejects.
//
// These are the strongest negative targets available: sshd listens on 0.0.0.0
// so each answers on tcp/22, reaching the host is the breach that actually
// matters, and traffic to a host address traverses INPUT rather than FORWARD —
// so nothing else on this machine can be silently doing the blocking.
func hostAddressesInRejectRanges() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if rangeContaining(ip.String()) != "" {
			out = append(out, ip.String())
		}
	}
	return dedupe(out)
}

// DefaultRoute reads the interface and gateway of the default route: the
// side of this host the LAN and the internet see.
func DefaultRoute() (iface, gateway string) { return defaultRoute() }

// defaultRoute reads the interface and gateway of the default route.
func defaultRoute() (iface, gateway string) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", ""
	}
	defer f.Close()

	scan := bufio.NewScanner(f)
	scan.Scan() // header
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		if ip := hexLEToIP(fields[2]); ip != "" {
			return fields[0], ip
		}
	}
	return "", ""
}

// lanPeer picks a neighbour that is not the gateway and that this host can
// actually reach, so the check has something to prove.
func lanPeer(iface, gateway string) string {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return ""
	}
	defer f.Close()

	scan := bufio.NewScanner(f)
	scan.Scan() // header
	tried := 0
	for scan.Scan() && tried < 8 {
		fields := strings.Fields(scan.Text())
		// IP, HW type, flags, HW address, mask, device. Flags 0x0 is an
		// incomplete entry — an address nothing answered for.
		if len(fields) < 6 || fields[2] == "0x0" {
			continue
		}
		if iface != "" && fields[5] != iface {
			continue
		}
		if fields[0] == gateway {
			continue
		}
		// A short timeout, and a bounded number of candidates: a table full of
		// stale entries must not make discovery take longer than the checks.
		tried++
		if ok, _ := hostICMP(fields[0], time.Second); ok {
			return fields[0]
		}
	}
	return ""
}

// tailnetPeer finds an online peer in 100.64.0.0/10 that is not this host.
//
// The range is not covered by RFC1918 and was a real gap here once, so it stays
// tested even though the host's own tailnet address already covers it. That
// address is precisely what must be excluded: `tailscale status` lists the
// local machine first, and testing it again would duplicate a host check while
// looking like it reached across the tailnet.
func tailnetPeer(own []string) string {
	out, err := exec.Command("tailscale", "status").Output()
	if err != nil {
		return ""
	}
	mine := map[string]bool{}
	for _, a := range own {
		mine[a] = true
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "100.") {
			continue
		}
		if strings.Contains(line, "offline") || mine[fields[0]] {
			continue
		}
		return fields[0]
	}
	return ""
}

// dockerTargets lists container IP:port pairs. Outcome checks only — see
// Target.Attributable.
func dockerTargets() []string {
	ids, err := exec.Command("docker", "ps", "-q").Output()
	if err != nil {
		return nil
	}
	var out []string
	for _, id := range strings.Fields(string(ids)) {
		raw, err := exec.Command("docker", "inspect", "-f",
			`{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}} {{range $p, $_ := .NetworkSettings.Ports}}{{$p}} {{end}}`,
			id).Output()
		if err != nil {
			continue
		}
		fields := strings.Fields(string(raw))
		if len(fields) < 2 || fields[0] == "" {
			continue
		}
		for _, p := range fields[1:] {
			port, ok := strings.CutSuffix(p, "/tcp")
			if !ok {
				continue
			}
			out = append(out, fmt.Sprintf("%s:%s", fields[0], port))
		}
	}
	return out
}

// hexLEToIP converts an address as /proc/net/route prints it to a dotted quad.
//
// The kernel formats the big-endian address with %08X of its native u32 value,
// so on a little-endian host the printed bytes come out in reverse order:
// 192.168.18.1 appears as 0112A8C0.
func hexLEToIP(s string) string {
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 4 {
		return ""
	}
	if raw[0] == 0 && raw[1] == 0 && raw[2] == 0 && raw[3] == 0 {
		return ""
	}
	return net.IPv4(raw[3], raw[2], raw[1], raw[0]).String()
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// BridgeAddress finds the resolver address the scoped DNS exception is tested
// against, by asking which network the instance's NIC is on rather than
// assuming a name.
//
// A failure to resolve it is not fatal: verify reports the DNS control as
// unproven, which is the honest outcome when there is nothing to control
// against.
func BridgeAddress(c *incus.Client, instance, override string) (string, error) {
	name := override
	if name == "" {
		inst, _, err := c.Instance(instance)
		if err != nil {
			return "", err
		}
		for _, dev := range inst.NICs() {
			if n := firstNonEmpty(dev["network"], dev["parent"]); n != "" {
				name = n
				break
			}
		}
	}
	if name == "" {
		return "", nil
	}
	network, err := c.Network(name)
	if err != nil {
		return "", nil
	}
	addr, _, _ := strings.Cut(network.Config["ipv4.address"], "/")
	return addr, nil
}
