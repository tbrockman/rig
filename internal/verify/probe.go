package verify

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/tbrockman/rig/internal/incus"
)

// Reach is what one probe found.
type Reach int

const (
	Reachable Reach = iota
	Blocked
	// Unknown means the probe itself did not run. It is a distinct outcome
	// because conflating it with Blocked is how this project produced false
	// PASSes twice: a probe binary missing from the guest's PATH looks exactly
	// like a target that refused the connection, and reads as perfect isolation.
	Unknown
)

func (r Reach) String() string {
	switch r {
	case Reachable:
		return "reachable"
	case Blocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// Mechanism is a family of probe sharing one transport. Each has a positive
// control; block results are only trusted when the control passed.
type Mechanism string

const (
	TCP  Mechanism = "tcp"
	ICMP Mechanism = "icmp"
	DNS  Mechanism = "dns"
)

const probeSeconds = 4

// --- guest side ----------------------------------------------------------

// guest runs one probe inside the guest and classifies the result.
//
// Probes are shell one-liners rather than anything installed in the image, so
// that verifying a guest needs nothing of the guest beyond bash. They run
// through incus.Exec, which uses a login shell — a bare `bash -c` in an Incus
// guest gets a stub PATH (/usr/bin:/bin and friends) that does not exist on
// NixOS.
func (r *Runner) guest(cmd string) (Reach, string) {
	out, err := r.C.Exec(r.Instance, cmd, incus.ExecOpts{
		Timeout: time.Duration(probeSeconds+16) * time.Second,
	})
	if err == nil {
		return Reachable, ""
	}
	var exit *incus.ExitError
	if errors.As(err, &exit) {
		switch exit.Code {
		case 127:
			return Unknown, "probe command missing in the guest: " + firstLine(out)
		case 124, 137:
			return Blocked, "no answer within " + fmt.Sprint(probeSeconds) + "s"
		}
		return Blocked, ""
	}
	return Unknown, err.Error()
}

func (r *Runner) guestTCP(ip string, port int) (Reach, string) {
	return r.guest(fmt.Sprintf(
		"timeout %d bash -c 'exec 3<>/dev/tcp/%s/%d'", probeSeconds, ip, port))
}

func (r *Runner) guestICMP(ip string) (Reach, string) {
	return r.guest(fmt.Sprintf("ping -n -c 2 -W 2 %s >/dev/null", ip))
}

// guestDNS sends a real query and waits for a real answer. Anything less cannot
// tell "the packet was dropped" from "the packet arrived and nothing replied",
// and UDP gives no other signal.
func (r *Runner) guestDNS(ip string) (Reach, string) {
	// The escapes stay literal inside bash's double quotes — \x is not one of
	// the sequences bash itself expands there — so printf is what interprets
	// them, which is the intent.
	return r.guest(fmt.Sprintf(
		`timeout %d bash -c 'exec 3<>/dev/udp/%s/53 && printf "%s" >&3 && read -r -N1 -t %d _ <&3'`,
		probeSeconds, ip, dnsQueryEscaped(), probeSeconds-1))
}

func (r *Runner) probeGuest(m Mechanism, ip string, port int) (Reach, string) {
	switch m {
	case TCP:
		return r.guestTCP(ip, port)
	case ICMP:
		return r.guestICMP(ip)
	case DNS:
		return r.guestDNS(ip)
	}
	return Unknown, "no such probe mechanism: " + string(m)
}

// --- host side -----------------------------------------------------------

// hostReaches answers the precondition every block check depends on: can this
// host reach the target at all? A connection that fails because nothing is
// listening is not evidence of blocking.
func hostReaches(m Mechanism, ip string, port int) bool {
	timeout := probeSeconds * time.Second
	switch m {
	case TCP:
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, fmt.Sprint(port)), timeout)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	case ICMP:
		ok, _ := hostICMP(ip, timeout)
		return ok
	case DNS:
		return hostDNS(ip, timeout)
	}
	return false
}

// hostICMP sends an echo request over an unprivileged ICMP socket. The error is
// returned separately so a permissions problem reads as "ICMP is unavailable"
// rather than "the target is unreachable" — those would otherwise both silently
// remove every ICMP check from the run.
func hostICMP(ip string, timeout time.Duration) (bool, error) {
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return false, err
	}
	defer conn.Close()

	// The kernel rewrites the ID on an unprivileged socket, so replies are
	// matched on the payload instead.
	payload := []byte(fmt.Sprintf("rig-verify-%d", os.Getpid()))
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Body: &icmp.Echo{Seq: 1, Data: payload},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return false, err
	}
	if _, err := conn.WriteTo(wire, &net.UDPAddr{IP: net.ParseIP(ip)}); err != nil {
		return false, err
	}

	deadline := time.Now().Add(timeout)
	_ = conn.SetReadDeadline(deadline)
	buf := make([]byte, 1500)
	for time.Now().Before(deadline) {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return false, nil // deadline: unreachable, not an ICMP failure
		}
		reply, err := icmp.ParseMessage(1, buf[:n])
		if err != nil {
			continue
		}
		if echo, ok := reply.Body.(*icmp.Echo); ok && string(echo.Data) == string(payload) {
			return true, nil
		}
	}
	return false, nil
}

// hostDNS sends the same query the guest probe sends, over the same transport,
// so the two sides differ only in where they run.
func hostDNS(ip string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("udp", net.JoinHostPort(ip, "53"), timeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(dnsQuery); err != nil {
		return false
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	return err == nil && n > 0
}

// icmpAvailable reports whether unprivileged ICMP sockets work here, so an
// unusable net.ipv4.ping_group_range is named rather than quietly dropping
// every ICMP check.
func icmpAvailable() error {
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return fmt.Errorf("%w\n  net.ipv4.ping_group_range must include this user's gid", err)
	}
	return conn.Close()
}

// --- the query -----------------------------------------------------------

// A minimal A query for example.com. Built once and rendered for the guest from
// the same bytes, so the two sides cannot drift.
var dnsQuery = []byte{
	0xaa, 0xaa, // transaction id
	0x01, 0x00, // standard query, recursion desired
	0x00, 0x01, // one question
	0x00, 0x00, // no answers
	0x00, 0x00, // no authority records
	0x00, 0x00, // no additional records
	0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e',
	0x03, 'c', 'o', 'm',
	0x00,       // end of name
	0x00, 0x01, // type A
	0x00, 0x01, // class IN
}

// dnsQueryEscaped renders the query for bash's printf. Every byte is a
// two-digit escape: \x takes up to two hex digits, so a literal character after
// a shorter escape would be swallowed into it.
func dnsQueryEscaped() string {
	var b strings.Builder
	for _, c := range dnsQuery {
		fmt.Fprintf(&b, `\x%02x`, c)
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
