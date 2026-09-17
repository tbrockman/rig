package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/vsock"
)

// Forwarding a TCP port between this host and a guest, over vsock.
//
// The isolation ACL rejects every private range, so there is no IP route from a
// guest to this host or back — deliberately, and `rig verify` proves it. That
// closes ssh, vnc and any network port forward, and it should stay closed.
//
// vsock is the way through, for the same reason `rig exec` and `rig mount`
// work: it is not IP. The guest agent already uses it. Incus proxy devices
// cannot help — 6.0.5 refuses anything but NAT mode for proxies on VMs, which
// is host-to-guest via nftables, the wrong direction and through the NIC.
//
// The motivating case is a captcha. An unattended agent that hits an
// interactive challenge cannot solve it, and neither can an operator handed a
// screenshot: these are bound to the browser session that raised them and
// expire in under a minute. Forwarding the browser's remote-debugging port lets
// a human drive that exact page, in that session, with its cookies — and then
// hand it back.
//
// The guest half is socat rather than more Go. It has to run *in* the guest,
// and rig has no agent there beyond Incus's; shipping a binary to run one
// splice would be a worse trade than requiring a package the guest image
// already carries.

// vsockPort is the port used on the vsock side. Deriving it from the TCP port
// keeps one number in the operator's head instead of two.
func vsockPort(tcpPort int) uint32 { return uint32(tcpPort) }

func (a *app) forwardCmd() *cobra.Command {
	var toGuest bool
	var hostPort int
	var bindAll bool

	cmd := &cobra.Command{
		Use:     "forward <vm> <port>",
		GroupID: "guest",
		Short:   "Forward a TCP port between this host and a guest, over vsock",
		Long: "Carries one TCP port between this host and a guest without opening the\n" +
			"network isolation. vsock is not IP, so the ACL that rejects every private\n" +
			"range — and that `rig verify` proves — is untouched.\n\n" +
			"By default the guest's port is exposed here:\n" +
			"  rig forward <vm> 9222              # guest:9222 -> 127.0.0.1:9222 here\n\n" +
			"With --to-guest, this host's port is exposed there:\n" +
			"  rig forward <vm> 8787 --to-guest   # here:8787 -> 127.0.0.1:8787 in the guest\n\n" +
			"Blocks while it holds the tunnel; Ctrl-C tears down both ends.\n\n" +
			"To reach it from a third machine — a laptop ssh'd into this host — do not\n" +
			"bind this to a public address. Tunnel over the ssh you already have:\n" +
			"  ssh -L 9222:127.0.0.1:9222 <this-host>",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			port, err := strconv.Atoi(args[1])
			if err != nil || port < 1 || port > 65535 {
				return fmt.Errorf("not a port number: %s", args[1])
			}
			if hostPort == 0 {
				hostPort = port
			}

			inst, _, err := a.c.Instance(name)
			if err != nil {
				return err
			}
			if !inst.Running() {
				return fmt.Errorf("%s is %s; a tunnel needs a running guest",
					name, strings.ToLower(inst.Status))
			}
			cid, err := guestCID(inst)
			if err != nil {
				return err
			}
			// socat runs the guest half. Check before starting anything, so a
			// missing package is one clear sentence rather than a tunnel that
			// accepts connections and drops every one of them.
			if out, err := a.exec(name, "command -v socat || true"); err != nil || strings.TrimSpace(out) == "" {
				return fmt.Errorf("no `socat` in %s, which runs the guest half of the tunnel.\n"+
					"The base image carries it; this guest was made from one that predates that.\n"+
					"Rebuild and recreate (rig image build, then rig new), or install it by hand:\n"+
					"  rig exec %s nix profile install nixpkgs#socat", name, name)
			}

			var guestCmd string
			if toGuest {
				guestCmd = fmt.Sprintf(
					"socat TCP-LISTEN:%d,bind=127.0.0.1,reuseaddr,fork VSOCK-CONNECT:%d:%d",
					port, vsock.CIDHost, vsockPort(port))
			} else {
				guestCmd = fmt.Sprintf(
					"socat VSOCK-LISTEN:%d,reuseaddr,fork TCP:127.0.0.1:%d",
					vsockPort(port), port)
			}

			// One tunnel per port. A pid that is still alive is a live tunnel
			// or a leftover from a rig that died without tearing down; either
			// way it is named rather than killed, because killing something
			// this did not start is not this command's decision to make.
			pidfile := guestPIDFile(port)
			if out, _ := a.exec(name, guestPIDAlive(pidfile)); strings.TrimSpace(out) != "" {
				pid := strings.TrimSpace(out)
				return fmt.Errorf("a forward for port %d is already running in %s (pid %s).\n"+
					"  rig exec %s kill %s   # if it is a leftover rather than someone's live tunnel",
					port, name, pid, name, pid)
			}
			if _, err := a.exec(name, guestStart(pidfile, guestCmd)); err != nil {
				return fmt.Errorf("starting the guest half: %w", err)
			}
			stopGuest := func() { _, _ = a.exec(name, guestStop(pidfile)) }
			defer stopGuest()

			// Announce the tunnel only once the guest half is listening. It
			// starts in the background, and a connection that arrives before
			// socat has bound is refused and dropped — a race a human never
			// hits and a script hits every time.
			if out, err := a.exec(name, guestListening(pidfile, port, toGuest)); err != nil {
				return fmt.Errorf("the guest half never started listening on port %d.\n%s", port, strings.TrimSpace(out))
			}

			stop := make(chan os.Signal, 1)
			signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

			if toGuest {
				return a.forwardToGuest(name, port, hostPort, stop)
			}
			return a.forwardFromGuest(name, cid, port, hostPort, bindAll, stop)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&toGuest, "to-guest", false, "expose this host's port inside the guest, instead of the other way round")
	f.IntVar(&hostPort, "host-port", 0, "listen on this port here instead of the same number")
	f.BoolVar(&bindAll, "bind-all", false,
		"listen on 0.0.0.0 rather than 127.0.0.1 — this exposes a guest port to your whole LAN")
	return cmd
}

// The guest half is tracked by a pidfile, never by a token in its command
// line. The first version put a token in socat's *environment* and tore down
// with `pkill -f`, which matches argument lists and cannot see an environment:
// the pkill matched nothing, every tunnel left its socat behind, and the next
// forward of the same port silently inherited a listener it had not started.
//
// The pid recorded is socat's own. The wrapper shell writes $$ and then execs
// socat in place, so the number is right whether or not setsid forked first.
func guestPIDFile(port int) string { return fmt.Sprintf("/run/rig/forward.%d.pid", port) }

// Only the setsid command is backgrounded, and its streams go to a log file
// next to the pidfile, so a socat that dies on startup can say why.
// Backgrounding `mkdir && setsid ...` as one list instead puts a subshell in
// front of it that inherits the exec's stdout and lives as long as socat does,
// so the exec never returns and `rig forward` hangs before it says anything.
// Found by the integration test, which waited 30s for a line that never came.
func guestStart(pidfile, socatCmd string) string {
	return fmt.Sprintf("mkdir -p /run/rig; setsid bash -c 'echo $$ > %s; exec %s' >%s 2>&1 & disown; true",
		pidfile, socatCmd, pidfile+".log")
}

func guestStop(pidfile string) string {
	return fmt.Sprintf("kill $(cat %s 2>/dev/null) 2>/dev/null; rm -f %s %s; true", pidfile, pidfile, pidfile+".log")
}

// guestListening polls until the guest half has bound its listener, and
// prints socat's own output if it has not within ten seconds. The from-guest
// half listens on vsock, which `ss --vsock` lists; the to-guest half on
// loopback TCP.
func guestListening(pidfile string, port int, toGuest bool) string {
	probe := fmt.Sprintf("ss -lH --vsock 2>/dev/null | grep -qE ':%d( |$)'", port)
	if toGuest {
		probe = fmt.Sprintf("ss -ltnH 2>/dev/null | grep -q '127.0.0.1:%d '", port)
	}
	return fmt.Sprintf("for i in $(seq 50); do %s && exit 0; sleep 0.2; done; cat %s 2>/dev/null; exit 1",
		probe, pidfile+".log")
}

// guestPIDAlive prints the recorded pid if that process still exists, and
// nothing otherwise — a stale pidfile is the ordinary aftermath of a guest
// reboot, and must not read as a tunnel.
func guestPIDAlive(pidfile string) string {
	return fmt.Sprintf(`p=$(cat %s 2>/dev/null); [ -n "$p" ] && kill -0 "$p" 2>/dev/null && echo "$p"; true`, pidfile)
}

// forwardFromGuest listens here and dials the guest for each connection.
func (a *app) forwardFromGuest(name string, cid uint32, port, hostPort int, bindAll bool, stop <-chan os.Signal) error {
	bind := "127.0.0.1"
	if bindAll {
		bind = "0.0.0.0"
		note("WARNING: --bind-all exposes %s:%d to your whole LAN, not just this host", name, port)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, hostPort))
	if err != nil {
		return err
	}
	defer ln.Close()

	note("%s:%d is now %s:%d here (over vsock cid %d)", name, port, bind, hostPort, cid)
	note("from another machine, tunnel over ssh rather than opening this up:")
	note("  ssh -L %d:127.0.0.1:%d %s@%s", hostPort, hostPort, os.Getenv("USER"), hostname())
	note("Ctrl-C to stop.")

	go func() { <-stop; ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil // the listener was closed: an ordinary shutdown
		}
		go func() {
			defer conn.Close()
			remote, err := vsock.Dial(cid, vsockPort(port))
			if err != nil {
				note("connection dropped: %v", err)
				return
			}
			defer remote.Close()
			splice(conn, remote)
		}()
	}
}

// forwardToGuest listens on vsock here and dials this host's own port.
func (a *app) forwardToGuest(name string, port, hostPort int, stop <-chan os.Signal) error {
	ln, err := vsock.Listen(vsockPort(port))
	if err != nil {
		return err
	}
	defer ln.Close()

	note("127.0.0.1:%d here is now 127.0.0.1:%d inside %s (over vsock)", hostPort, port, name)
	note("Ctrl-C to stop.")

	go func() { <-stop; ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil
		}
		go func() {
			defer conn.Close()
			local, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort))
			if err != nil {
				note("connection dropped: %v", err)
				return
			}
			defer local.Close()
			splice(local, conn)
		}()
	}
}

// splice copies in both directions and returns when either side is done.
func splice(a io.ReadWriteCloser, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(a, b); closeWrite(a) }()
	go func() { defer wg.Done(); _, _ = io.Copy(b, a); closeWrite(b) }()
	wg.Wait()
}

// closeWrite half-closes where it can, so the peer sees EOF rather than waiting
// for a timeout. A vsock *os.File cannot, and closing the pair handles it.
func closeWrite(c io.ReadWriteCloser) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// guestCID reads the context ID Incus assigned this instance.
func guestCID(inst *incus.Instance) (uint32, error) {
	raw := inst.Config["volatile.vsock_id"]
	if raw == "" {
		return 0, fmt.Errorf("%s has no volatile.vsock_id; it may not be a VM, or may never have started", inst.Name)
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("unreadable volatile.vsock_id %q: %w", raw, err)
	}
	return uint32(n), nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "this-host"
	}
	return h
}
