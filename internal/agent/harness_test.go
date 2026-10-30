package agent

import (
	"io"
	"strings"
	"testing"
)

func TestThousands(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 50000000: "50,000,000", -1234: "-1,234"} {
		if got := Thousands(n); got != want {
			t.Errorf("Thousands(%d) = %q, want %q", n, got, want)
		}
	}
}

// stub is a harness with nothing behind it but a name.
type stub struct{ name string }

func (s stub) Name() string                                          { return s.name }
func (stub) Binary() string                                          { return "stub" }
func (stub) InstallHint() string                                     { return "true" }
func (stub) Probe() string                                           { return "true" }
func (stub) Runner() []byte                                          { return []byte("#!/bin/sh\n") }
func (stub) Credentials() []Credential                               { return nil }
func (stub) TrustEnv(_, _ string) []string                           { return nil }
func (stub) LastError(string) string                                 { return "" }
func (stub) ExplainError(m string) string                            { return m }
func (stub) AssistantText(string, int) []string                      { return nil }
func (stub) TailSpeechCmd(int) string                                { return "" }
func (stub) TailResultsCmd(int) string                               { return "" }
func (stub) Delivery() []DeliveryPath                                { return CommonDelivery() }
func (stub) Usage(io.Reader) (Consumption, bool)                     { return Consumption{}, false }
func (stub) QuotaRequests(map[string]string) ([]QuotaRequest, error) { return nil, nil }
func (stub) Overseer(OverseerOpts) ([]string, map[string][]byte, bool) {
	return nil, nil, false
}

// The registry is package state; each test starts from an empty one and puts
// back whatever it found, so tests cannot see each other's registrations.
func isolateRegistry(t *testing.T) {
	t.Helper()
	registryMu.Lock()
	saved := registry
	registry = map[string]Harness{}
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = saved
		registryMu.Unlock()
	})
}

func TestRegisterThenLookupByName(t *testing.T) {
	isolateRegistry(t)
	Register(stub{"zeta"})
	Register(stub{"alpha"})
	h, err := Lookup("zeta")
	if err != nil || h.Name() != "zeta" {
		t.Fatalf("Lookup(zeta) = %v, %v", h, err)
	}
	if got := strings.Join(Names(), ","); got != "alpha,zeta" {
		t.Errorf("Names() must be sorted, got %q", got)
	}
}

// The name comes from a manifest or an instance, so the error has to say what
// would have been accepted.
func TestLookupUnknownNamesTheKnownOnes(t *testing.T) {
	isolateRegistry(t)
	Register(stub{"alpha"})
	_, err := Lookup("nope")
	if err == nil {
		t.Fatal("an unknown name must be an error")
	}
	for _, want := range []string{`"nope"`, "alpha"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %s", err, want)
		}
	}
}

func TestEmptyNameIsTheDefault(t *testing.T) {
	isolateRegistry(t)
	Register(stub{"other"})
	Register(stub{defaultName})
	h, err := Lookup("")
	if err != nil {
		t.Fatal(err)
	}
	if h.Name() != defaultName || Default().Name() != defaultName {
		t.Errorf("Lookup(\"\") and Default() must both be %s", defaultName)
	}
}

// Nothing registered under the default name is a missing import, and the panic
// should say so rather than leave a nil interface to be dereferenced later.
func TestDefaultPanicsWhenNothingIsRegistered(t *testing.T) {
	isolateRegistry(t)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Default() with no registration must panic")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "claudecode") {
			t.Errorf("the panic should name the package to import, got %v", r)
		}
	}()
	Default()
}

func TestForInstanceReadsTheRecordedHarness(t *testing.T) {
	isolateRegistry(t)
	Register(stub{defaultName})
	Register(stub{"other"})
	if h, err := ForInstance(map[string]string{HarnessKey: "other"}); err != nil || h.Name() != "other" {
		t.Errorf("recorded harness: got %v, %v", h, err)
	}
	if h, err := ForInstance(map[string]string{}); err != nil || h.Name() != defaultName {
		t.Errorf("no record must mean the default: got %v, %v", h, err)
	}
	if _, err := ForInstance(map[string]string{HarnessKey: "gone"}); err == nil {
		t.Error("an instance recording a harness this rig lacks must be an error, not the default")
	}
}

// A second registration under the same name replaces the first: that is how
// a test puts a stub where the real harness would be.
func TestRegisterReplaces(t *testing.T) {
	isolateRegistry(t)
	Register(stub{"same"})
	Register(stub{"same"})
	if n := len(Names()); n != 1 {
		t.Errorf("one name, got %d", n)
	}
}
