package main

import (
	"strings"
	"testing"

	"github.com/tbrockman/rig/internal/incus"
)

func TestGuestCIDReadsWhatIncusAssigned(t *testing.T) {
	inst := &incus.Instance{Name: "myvm", Config: map[string]string{"volatile.vsock_id": "1016339200"}}
	cid, err := guestCID(inst)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cid != 1016339200 {
		t.Fatalf("cid = %d, want 1016339200", cid)
	}
}

// A CID is assigned when a VM first starts, so its absence is a real state to
// be in — and one worth explaining rather than forwarding into a connect that
// fails obscurely.
func TestGuestCIDExplainsAnInstanceThatHasNoneYet(t *testing.T) {
	inst := &incus.Instance{Name: "fresh", Config: map[string]string{}}
	_, err := guestCID(inst)
	if err == nil {
		t.Fatal("an instance with no vsock_id was accepted")
	}
	for _, want := range []string{"fresh", "vsock_id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestGuestCIDRejectsGarbage(t *testing.T) {
	inst := &incus.Instance{Name: "myvm", Config: map[string]string{"volatile.vsock_id": "not-a-number"}}
	if _, err := guestCID(inst); err == nil {
		t.Fatal("an unparseable vsock_id was accepted")
	}
}

// The forward defaults to loopback. Binding a guest's port to every interface
// hands it to the LAN, which is the opposite of what this VM is for, so it must
// stay an explicit choice rather than a default.
func TestForwardBindsLoopbackUnlessTold(t *testing.T) {
	a := &app{}
	cmd := a.forwardCmd()
	f := cmd.Flags().Lookup("bind-all")
	if f == nil {
		t.Fatal("no --bind-all flag")
	}
	if f.DefValue != "false" {
		t.Errorf("--bind-all defaults to %q; loopback must be the default", f.DefValue)
	}
	if !strings.Contains(f.Usage, "LAN") {
		t.Error("the flag must say what it exposes; an operator reading --help is the last check")
	}
}

// The help has to answer the question an operator actually has, which is how to
// reach this from the laptop they are ssh'd in from — not from the host itself.
func TestForwardHelpExplainsTheThirdMachineCase(t *testing.T) {
	a := &app{}
	long := a.forwardCmd().Long
	if !strings.Contains(long, "ssh -L") {
		t.Error("help must show the ssh tunnel, or an operator will reach for --bind-all")
	}
}

// Teardown has to kill the process that was started, which means knowing its
// pid. The first version stashed a token in socat's environment and ran
// `pkill -f` on it — which matches argument lists, never environments — so
// every tunnel left its socat behind and the next forward of the same port
// inherited a listener it had not started.
func TestForwardTracksTheGuestHalfByPid(t *testing.T) {
	pidfile := guestPIDFile(9222)
	start := guestStart(pidfile, "socat VSOCK-LISTEN:9222,reuseaddr,fork TCP:127.0.0.1:9222")
	if !strings.Contains(start, "echo $$ > "+pidfile) || !strings.Contains(start, "; exec socat") {
		t.Errorf("the wrapper must record its own pid and then exec socat in place, got:\n%s", start)
	}
	// `mkdir && setsid ... &` backgrounds a list, and the subshell running it
	// keeps the exec's stdout open for as long as socat lives; the exec then
	// never returns. Only the setsid command may be the background job.
	if strings.Contains(start, "&& setsid") {
		t.Errorf("only the setsid command may be backgrounded, got:\n%s", start)
	}
	stop := guestStop(pidfile)
	if !strings.Contains(stop, "cat "+pidfile) || strings.Contains(stop, "pkill") {
		t.Errorf("teardown must kill the recorded pid, not pattern-match a command line, got:\n%s", stop)
	}
	if !strings.Contains(guestPIDAlive(pidfile), "kill -0") {
		t.Error("a stale pidfile after a guest reboot must not read as a live tunnel")
	}
}

// The guest half starts in the background, so the tunnel must not be announced
// until it is listening: a connection arriving before socat has bound is
// refused and dropped. The integration test hit that window every time.
func TestForwardWaitsForTheGuestHalfToListen(t *testing.T) {
	fromGuest := guestListening(guestPIDFile(9222), 9222, false)
	if !strings.Contains(fromGuest, "ss -lH --vsock") || !strings.Contains(fromGuest, ":9222") {
		t.Errorf("the from-guest half listens on vsock; the wait must look there, got:\n%s", fromGuest)
	}
	toGuest := guestListening(guestPIDFile(8787), 8787, true)
	if !strings.Contains(toGuest, "ss -ltnH") || !strings.Contains(toGuest, "127.0.0.1:8787") {
		t.Errorf("the to-guest half listens on loopback TCP; the wait must look there, got:\n%s", toGuest)
	}
	if !strings.Contains(toGuest, "exit 1") || !strings.Contains(toGuest, ".log") {
		t.Error("a half that never listens must fail the wait and show socat's own output")
	}
}
