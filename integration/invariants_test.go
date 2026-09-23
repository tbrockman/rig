//go:build integration

// Package integration exercises rig against a real Incus daemon and a real
// card. It is build-tagged out of `go test ./...` because it is destructive: it
// creates and deletes instances, moves the GPU between them, and starts a VM
// with no network isolation on purpose.
//
//	go test -tags integration ./integration/ -v
//
// A mock daemon would be no use here. Every invariant this guards lives in
// VFIO, QEMU and Incus, and a mock will cheerfully report that everything is
// fine — which is exactly the failure mode being guarded against.
package integration

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tbrockman/rig/internal/devices"
	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/policy"
	"github.com/tbrockman/rig/internal/verify"
)

const (
	instA      = "gputest-a"
	instB      = "gputest-b"
	badProfile = "gputest-bad"
)

var (
	c   = incus.New("")
	cfg = devices.ConfigFromEnv()
)

func image() string {
	if v := os.Getenv("RIG_IMAGE"); v != "" {
		return v
	}
	return "nixos-gpu-base"
}

// TestInvariants runs as ordered subtests sharing state, the way the states
// they exercise actually arise. Each one names what it leaves behind.
func TestInvariants(t *testing.T) {
	preflight(t)
	t.Cleanup(func() {
		_ = c.SetProfiles(instA, []string{"default"})
		_ = c.DeleteProfile(badProfile)
		teardown(t, instA, instB)
	})

	create(t, instA)
	create(t, instB)

	t.Run("claim attaches the device to a stopped instance", func(t *testing.T) {
		if err := devices.Claim(c, cfg, instA); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if pci := gpuPCI(t, instA); pci == "" {
			t.Fatal("no GPU device on gputest-a after a successful claim")
		}
	})

	t.Run("the card migrates between two stopped instances", func(t *testing.T) {
		if err := devices.Claim(c, cfg, instB); err != nil {
			t.Fatalf("claim refused between two stopped instances: %v", err)
		}
		if gpuPCI(t, instB) == "" {
			t.Error("gputest-b did not get the card")
		}
		if gpuPCI(t, instA) != "" {
			t.Error("gputest-a still has the card: it is now configured on both")
		}
	})

	// The big one. Starting a second VM with the same GPU hot-unplugs the card
	// from the running one, and Incus reports that only on the VM that failed to
	// start — the victim keeps showing RUNNING with a healthy IP.
	t.Run("the card cannot be taken from a running instance", func(t *testing.T) {
		if err := devices.Start(c, cfg, instB, false, 180); err != nil {
			t.Fatalf("start: %v", err)
		}
		if err := c.WaitAgent(instB, 3*time.Minute); err != nil {
			t.Fatalf("guest never came up: %v", err)
		}

		if err := devices.Claim(c, cfg, instA); err == nil {
			t.Error("claim SUCCEEDED against a running holder — invariant violated")
		}
		// Asserting the refusal is not enough: the failure that matters is
		// silent damage to the instance that was already running.
		if _, err := c.Exec(instB, "gpu-check", incus.ExecOpts{Timeout: 30 * time.Second}); err != nil {
			t.Errorf("gputest-b's GPU is gone or unhealthy after the refused claim: %v", err)
		}
	})

	t.Run("release refuses a running holder without force", func(t *testing.T) {
		if _, err := devices.Release(c, cfg, false); err == nil {
			t.Error("release detached the card from a running instance without --force")
		}
	})

	// Runs while gputest-b is up from the previous subtest, so it costs no extra
	// boot. It is the positive half of the pair completed by
	// TestVerifyDetectsAnUnisolatedGuest: a suite that only ever sees isolated
	// guests cannot tell a working detector from one that always says "blocked".
	t.Run("verify proves an isolated guest", func(t *testing.T) {
		report := runVerify(t, instB)
		if report.Verdict != verify.Proven {
			t.Errorf("verify said %s on an isolated guest: %s", report.Verdict, report.Summary)
			for _, ch := range append(report.Controls, report.Checks...) {
				if ch.Outcome != verify.Pass {
					t.Logf("  %s: %s (%s)", ch.Outcome, ch.Label, ch.Detail)
				}
			}
		}
	})

	t.Run("concurrent claims serialise to exactly one winner", func(t *testing.T) {
		if err := devices.Stop(c, cfg, instB, 120); err != nil {
			t.Fatalf("stop: %v", err)
		}
		// Separate flock() calls even in one process contend: the lock belongs
		// to the open file description, not the process.
		var wg sync.WaitGroup
		for _, name := range []string{instA, instB} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = devices.Claim(c, cfg, name)
			}()
		}
		wg.Wait()

		if n := len(holders(t)); n != 1 {
			t.Errorf("ended with %d holders, want exactly 1", n)
		}
	})

	// An instance-level device named gpu0 — exactly what rig creates — masks a
	// profile's gpu0, so scanning instances' expanded devices reports all-clear
	// while the profile stays primed to misconfigure the next instance created.
	t.Run("a GPU in a profile is rejected", func(t *testing.T) {
		if err := devices.Claim(c, cfg, instA); err != nil {
			t.Fatalf("claim: %v", err)
		}
		pci, err := devices.DiscoverPCI(c, cfg)
		if err != nil {
			t.Fatalf("discover: %v", err)
		}
		err = c.CreateProfile(badProfile, map[string]incus.Device{
			"gpu0": {"type": "gpu", "gputype": "physical", "pci": pci},
		})
		if err != nil {
			t.Fatalf("creating the poisoned profile: %v", err)
		}
		defer c.DeleteProfile(badProfile)

		// No instance uses it yet. It is still primed to damage the next one, so
		// it has to be rejected on its own.
		if err := checkProfiles(); err == nil {
			t.Error("poisoned profile not detected while unused")
		}

		if err := c.SetProfiles(instA, []string{"default", badProfile}); err != nil {
			t.Fatalf("attaching the poisoned profile: %v", err)
		}
		defer c.SetProfiles(instA, []string{"default"})

		if err := checkProfiles(); err == nil {
			t.Error("profile-borne GPU not detected: masked by the instance's own gpu0")
		}
	})

	t.Run("start refuses an instance with no network isolation", func(t *testing.T) {
		if _, err := devices.Release(c, cfg, false); err != nil {
			t.Fatalf("release: %v", err)
		}
		unisolate(t, instA)

		if err := devices.Start(c, cfg, instA, false, 180); err == nil {
			t.Error("start SUCCEEDED on an instance with no isolation ACL")
		}
		// The isolation check runs before the claim, so a refusal must not have
		// moved the card. Otherwise a refused start still perturbs ownership.
		if n := len(holders(t)); n != 0 {
			t.Errorf("a refused start left the card attached (%d holders)", n)
		}

		if err := devices.Start(c, cfg, instA, true, 180); err != nil {
			t.Errorf("--allow-unisolated did not start the instance: %v", err)
		}
		// Stop it here rather than leaving it to the cleanup at the end of the
		// parent test. It has no isolation: while it runs it can reach this
		// host and the LAN, so the window belongs to this subtest alone.
		teardown(t, instA)
	})
}

// TestVerifyDetectsAnUnisolatedGuest is the negative control for `rig verify`.
//
// Every other check in this file confirms something is refused. This one
// confirms the detector fires: it starts a guest with the isolation ACL
// removed and requires verify to call that a violation. Without it, a verify
// that reported "blocked" unconditionally would pass every other test here.
//
// The guest reaches this host and the LAN while it runs, which is the point.
// It is stopped as soon as the report is in.
func TestVerifyDetectsAnUnisolatedGuest(t *testing.T) {
	preflight(t)
	const name = "gputest-open"

	create(t, name)
	t.Cleanup(func() { teardown(t, name) })
	unisolate(t, name)

	if err := devices.Start(c, cfg, name, true, 180); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := c.WaitAgent(name, 3*time.Minute); err != nil {
		t.Fatalf("guest never came up: %v", err)
	}
	if _, err := c.WaitAddress(name, 2*time.Minute); err != nil {
		t.Fatalf("guest never got an address: %v", err)
	}

	report := runVerify(t, name)
	teardown(t, name) // the exposure ends the moment the report is in

	if report.Verdict != verify.Violated {
		t.Fatalf("verify said %s for a guest with no isolation at all; it must say %s.\n"+
			"summary: %s", report.Verdict, verify.Violated, report.Summary)
	}

	// And it must be failing for the right reason: the guest reaching this host.
	var reachedHost bool
	for _, ch := range report.Checks {
		if ch.Outcome == verify.Fail && strings.HasPrefix(ch.Label, "host ") {
			reachedHost = true
			t.Logf("correctly caught: %s — %s", ch.Label, ch.Detail)
		}
	}
	if !reachedHost {
		t.Error("verify reported a violation, but not that the guest reached this host")
	}
}

// --- helpers -------------------------------------------------------------

// preflight refuses to run while anything else is up. These tests move the card
// and delete instances; doing that around a live workload is how the silent
// hot-unplug happens for real.
func preflight(t *testing.T) {
	t.Helper()
	instances, err := c.Instances()
	if err != nil {
		t.Skipf("no reachable Incus daemon: %v", err)
	}
	if !c.ImageExists(image()) {
		t.Skipf("no %s image; build it with `rig image build`", image())
	}
	var debris []string
	for _, inst := range instances {
		if strings.HasPrefix(inst.Name, "gputest-") {
			debris = append(debris, inst.Name)
			continue
		}
		if inst.Running() {
			t.Fatalf("%s is running. These tests move the GPU and would disrupt it.\n"+
				"Stop it first:  rig stop %s", inst.Name, inst.Name)
		}
	}
	// Anything named gputest-* is debris from an earlier run. Clearing it here
	// rather than trusting the previous run's cleanup is what keeps each test
	// standing on its own — a cleanup that failed once already left an
	// unisolated guest running into the next test.
	teardown(t, debris...)
}

// teardown stops and deletes instances, reporting what it could not do. The
// errors matter: a swallowed failure here leaves a VM running, and the one this
// suite leaves behind is the one with no network isolation.
func teardown(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		inst, _, err := c.Instance(name)
		if err != nil {
			continue // already gone
		}
		if !inst.Stopped() {
			// A guest a few seconds into boot has no ACPI handler yet and
			// ignores a graceful stop, so fall back to killing it.
			if err := c.SetState(name, "stop", 60); err != nil {
				t.Logf("graceful stop of %s failed (%v); forcing", name, err)
				if err := c.SetState(name, "stop", 0); err != nil {
					t.Errorf("could not stop %s: %v — it may still be running", name, err)
					continue
				}
			}
		}
		if err := c.DeleteInstance(name); err != nil {
			t.Errorf("could not delete %s: %v", name, err)
		}
	}
}

func create(t *testing.T, name string) {
	t.Helper()
	teardown(t, name)
	err := c.CreateVM(incus.CreateOpts{
		Name: name, Image: image(), CPUs: 4, Memory: "8GiB",
		// Marked the way `rig new` marks them, so debris from a failed run can
		// be cleared with `rig rm` rather than needing raw incus.
		Config: map[string]string{"user.rig.managed": "true"},
	})
	if err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
}

// unisolate strips the isolation ACL by overriding the NIC onto the instance,
// simulating an instance created before the profile carried the policy.
func unisolate(t *testing.T, name string) {
	t.Helper()
	inst, _, err := c.Instance(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	devices := map[string]incus.Device{}
	for devName, dev := range inst.Devices {
		devices[devName] = dev
	}
	for nic, dev := range inst.NICs() {
		local := incus.Device{}
		for k, v := range dev {
			local[k] = v
		}
		delete(local, "security.acls")
		delete(local, "security.acls.default.egress.action")
		delete(local, "security.acls.default.ingress.action")
		devices[nic] = local
	}
	if err := c.SetDevices(name, devices); err != nil {
		t.Fatalf("overriding the NIC on %s: %v", name, err)
	}
	inst, _, err = c.Instance(name)
	if err != nil {
		t.Fatalf("re-reading %s: %v", name, err)
	}
	if policy.Isolated(inst, cfg.ACL) {
		t.Fatalf("%s is still isolated; the test cannot set up its own precondition", name)
	}
}

func runVerify(t *testing.T, name string) *verify.Report {
	t.Helper()
	bridge, err := verify.BridgeAddress(c, name, "")
	if err != nil {
		t.Fatalf("bridge address: %v", err)
	}
	r := &verify.Runner{
		C: c, Instance: name, ACL: cfg.ACL, Bridge: bridge,
		AllowGaps: verify.DefaultAllowGaps,
	}
	report, err := r.Run()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return report
}

func gpuPCI(t *testing.T, name string) string {
	t.Helper()
	inst, _, err := c.Instance(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	for _, dev := range inst.GPUDevices() {
		return dev["pci"]
	}
	return ""
}

func holders(t *testing.T) []devices.Holder {
	t.Helper()
	instances, err := c.Instances()
	if err != nil {
		t.Fatalf("listing instances: %v", err)
	}
	return devices.Holders(instances)
}

func checkProfiles() error {
	instances, err := c.Instances()
	if err != nil {
		return err
	}
	return devices.CheckProfiles(c, instances)
}
