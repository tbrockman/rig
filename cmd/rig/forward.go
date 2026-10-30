package main

import (
	"context"
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
	var toGuest, bindAll, stopIt, background, follow bool
	var hostPort int

	cmd := &cobra.Command{
		Use:     "forward <vm> [<port>...]",
		GroupID: "guest",
		Short:   "Forward TCP ports between this host and a guest, over vsock",
		Long: "Carries TCP ports between this host and a guest without opening the\n" +
			"network isolation. vsock is not IP, so the ACL that rejects every private\n" +
			"range — and that `rig verify` proves — is untouched.\n\n" +
			"By default the guest's ports are exposed here, on loopback:\n" +
			"  rig forward <vm> 9222              # guest:9222 -> 127.0.0.1:9222 here\n" +
			"  rig forward <vm>                   # the manifest's guest.forwards\n\n" +
			"With --to-guest, this host's port is exposed there:\n" +
			"  rig forward <vm> 8787 --to-guest   # here:8787 -> 127.0.0.1:8787 in the guest\n\n" +
			"Blocks while it holds the tunnel; Ctrl-C, or closing the terminal, tears\n" +
			"down both ends. --background holds it as a transient user unit instead,\n" +
			"rig-forward-<vm>, which ends when the VM stops; rig start leaves one for\n" +
			"the manifest's forwards, so this is rarely typed. --stop with no ports\n" +
			"ends that unit; with ports it removes a guest half left behind by a rig\n" +
			"that was killed outright.\n\n" +
			"To reach it from a third machine — a laptop ssh'd into this host — do not\n" +
			"bind this to a public address. Tunnel over the ssh you already have:\n" +
			"  ssh -L 9222:127.0.0.1:9222 <this-host>",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			ports, err := parsePorts(args[1:])
			if err != nil {
				return err
			}
			inst, _, err := a.c.Instance(name)
			if err != nil {
				return err
			}
			if stopIt && len(ports) == 0 {
				if !forwardActive(name) {
					note("no forward holder is running for %s", name)
					return nil
				}
				return stopForwardUnit(name)
			}
			if len(ports) == 0 {
				if toGuest {
					return fmt.Errorf("--to-guest needs the port to expose in the guest")
				}
				if ports, err = recordedForwards(inst); err != nil {
					return err
				}
				if len(ports) == 0 {
					return fmt.Errorf("no port given, and %s's manifest names none under guest.forwards", name)
				}
			}
			if hostPort != 0 && len(ports) != 1 {
				return fmt.Errorf("--host-port renames one port; %d were given", len(ports))
			}
			if stopIt {
				for _, port := range ports {
					pidfile := guestPIDFile(port)
					out, _ := a.exec(name, guestPIDAlive(pidfile))
					pid := strings.TrimSpace(out)
					if !inst.Running() || pid == "" {
						note("no forward for port %d is running in %s", port, name)
						continue
					}
					if _, err := a.exec(name, guestStop(pidfile)); err != nil {
						return fmt.Errorf("stopping the guest half: %w", err)
					}
					note("stopped the guest half of the forward for port %d in %s (pid %s)", port, name, pid)
				}
				return nil
			}
			if !inst.Running() {
				return fmt.Errorf("%s is %s; a tunnel needs a running guest",
					name, strings.ToLower(inst.Status))
			}
			if background {
				return startForwardUnit(name, ports, toGuest)
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
					"  rig exec %s -- nix profile install nixpkgs#socat", name, name)
			}

			// One tunnel per port. A pid that is still alive is a live tunnel
			// or a leftover from a rig that died without tearing down; either
			// way it is named rather than killed, because killing something
			// this did not start is not this command's decision to make.
			for _, port := range ports {
				if out, _ := a.exec(name, guestPIDAlive(guestPIDFile(port))); strings.TrimSpace(out) != "" {
					pid := strings.TrimSpace(out)
					return fmt.Errorf("a forward for port %d is already running in %s (pid %s).\n"+
						"  rig forward %s %d --stop   # if it is a leftover rather than someone's live tunnel",
						port, name, pid, name, port)
				}
			}
			for _, port := range ports {
				if err := a.startGuestHalf(name, port, toGuest); err != nil {
					for _, p := range ports {
						_, _ = a.exec(name, guestStop(guestPIDFile(p)))
					}
					return err
				}
			}
			defer func() {
				for _, port := range ports {
					_, _ = a.exec(name, guestStop(guestPIDFile(port)))
				}
			}()

			// SIGHUP too: closing the terminal is the other ordinary way this
			// ends, and without it the guest half outlived every such tunnel.
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			if follow {
				ctx = a.untilStopped(ctx, name, cid, "%s stopped; its forwards end with it")
			}
			if toGuest {
				return a.forwardToGuest(ctx, name, ports[0], firstPort(hostPort, ports[0]))
			}
			return a.forwardFromGuest(ctx, name, cid, ports, hostPort, bindAll)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&toGuest, "to-guest", false, "expose this host's port inside the guest, instead of the other way round")
	f.IntVar(&hostPort, "host-port", 0, "listen on this port here instead of the same number (one port only)")
	f.BoolVar(&stopIt, "stop", false, "end the background holder (no ports), or the guest half of each port named, left behind by a rig that did not exit cleanly")
	f.BoolVar(&bindAll, "bind-all", false,
		"listen on 0.0.0.0 rather than 127.0.0.1 — this exposes a guest port to your whole LAN")
	f.BoolVar(&background, "background", false,
		"hold the tunnel as a transient user unit, rig-forward-<vm>, that ends when the VM stops")
	f.BoolVar(&follow, "follow", false, "exit when the VM stops (what the background unit runs)")
	_ = f.MarkHidden("follow")
	return cmd
}

// parsePorts reads the port operands.
func parsePorts(args []string) ([]int, error) {
	var ports []int
	seen := map[int]bool{}
	for _, arg := range args {
		port, err := strconv.Atoi(arg)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("not a port number: %s", arg)
		}
		if seen[port] {
			return nil, fmt.Errorf("port %d is given twice", port)
		}
		seen[port] = true
		ports = append(ports, port)
	}
	return ports, nil
}

func firstPort(given, fallback int) int {
	if given != 0 {
		return given
	}
	return fallback
}

// startGuestHalf starts socat in the guest for one port and waits until it
// listens. The tunnel is announced only once the guest half is listening: it
// starts in the background, and a connection that arrives before socat has
// bound is refused and dropped — a race a human never hits and a script hits
// every time.
func (a *app) startGuestHalf(name string, port int, toGuest bool) error {
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
	pidfile := guestPIDFile(port)
	if _, err := a.exec(name, guestStart(pidfile, guestCmd)); err != nil {
		return fmt.Errorf("starting the guest half for port %d: %w", port, err)
	}
	if out, err := a.exec(name, guestListening(pidfile, port, toGuest)); err != nil {
		return fmt.Errorf("the guest half never started listening on port %d.\n%s", port, strings.TrimSpace(out))
	}
	return nil
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

// forwardFromGuest listens here, one listener per port, and dials the guest
// for each connection, until ctx ends. A dial that fails has the guest half
// started again before it is given up on: a guest that rebooted under a
// holder lost its socat, and nothing else would put it back.
func (a *app) forwardFromGuest(ctx context.Context, name string, cid uint32, ports []int, hostPort int, bindAll bool) error {
	bind := "127.0.0.1"
	if bindAll {
		bind = "0.0.0.0"
		note("WARNING: --bind-all exposes %s's ports to your whole LAN, not just this host", name)
	}
	var listeners []net.Listener
	for _, port := range ports {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, firstPort(hostPort, port)))
		if err != nil {
			for _, l := range listeners {
				l.Close()
			}
			return err
		}
		listeners = append(listeners, ln)
		note("%s:%d is now %s:%d here (over vsock cid %d)", name, port, bind, firstPort(hostPort, port), cid)
	}
	note("from another machine, tunnel over ssh rather than opening this up:")
	note("  ssh -L %d:127.0.0.1:%d %s@%s", firstPort(hostPort, ports[0]), firstPort(hostPort, ports[0]), os.Getenv("USER"), hostname())
	note("Ctrl-C to stop.")

	var wg sync.WaitGroup
	for i, ln := range listeners {
		port := ports[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				conn, err := ln.Accept()
				if err != nil {
					return // the listener was closed: an ordinary shutdown
				}
				go func() {
					defer conn.Close()
					remote, err := vsock.Dial(cid, vsockPort(port))
					if err != nil {
						if err := a.startGuestHalf(name, port, false); err == nil {
							remote, err = vsock.Dial(cid, vsockPort(port))
						}
					}
					if err != nil {
						note("connection to %s:%d dropped: %v", name, port, err)
						return
					}
					defer remote.Close()
					splice(conn, remote)
				}()
			}
		}()
	}
	<-ctx.Done()
	for _, ln := range listeners {
		ln.Close()
	}
	wg.Wait()
	return nil
}

// forwardToGuest listens on vsock here and dials this host's own port.
func (a *app) forwardToGuest(ctx context.Context, name string, port, hostPort int) error {
	ln, err := vsock.Listen(vsockPort(port))
	if err != nil {
		return err
	}
	defer ln.Close()

	note("127.0.0.1:%d here is now 127.0.0.1:%d inside %s (over vsock)", hostPort, port, name)
	note("Ctrl-C to stop.")

	go func() { <-ctx.Done(); ln.Close() }()
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
