package main

import (
	"strings"
	"testing"

	"rig/internal/incus"
)

func TestGuestCIDReadsWhatIncusAssigned(t *testing.T) {
	inst := &incus.Instance{Name: "og", Config: map[string]string{"volatile.vsock_id": "1016339200"}}
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
	inst := &incus.Instance{Name: "og", Config: map[string]string{"volatile.vsock_id": "not-a-number"}}
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
