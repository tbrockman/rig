package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/creds"
	"github.com/tbrockman/rig/internal/devices"
	"github.com/tbrockman/rig/internal/hostdev"
	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
)

func envFileAt(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "creds.env")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile is subject to umask; force the mode we asked for.
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// Not passing --env must mean "leave this VM's credentials alone", not "clear
// them". Every caller relies on the empty-in/empty-out contract to decide
// whether to touch the instance config at all.
func TestAbsEnvFileIgnoresTheEmptyFlag(t *testing.T) {
	got, err := absEnvFile("")
	if err != nil {
		t.Fatalf("empty --env is not an error, got %v", err)
	}
	if got != "" {
		t.Fatalf("empty --env must resolve to empty, got %q", got)
	}
}

// The path is stored on the instance and read back by a later `rig start`,
// possibly from a different working directory. A relative path would resolve
// against the wrong directory then, and the failure would look like missing
// credentials rather than a bad path.
func TestAbsEnvFileMakesThePathAbsolute(t *testing.T) {
	p := envFileAt(t, "KEY=value\n", 0o600)

	dir, base := filepath.Split(p)
	t.Chdir(dir)

	got, err := absEnvFile(base)
	if err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("got %q, want an absolute path", got)
	}
	if got != strings.TrimSuffix(p, "") {
		t.Fatalf("got %q, want %q", got, p)
	}
}

// Validation happens where the flag is parsed, not at injection time. Recording
// a path that `start` will later refuse turns one clear error into a confusing
// one two commands afterwards.
func TestAbsEnvFileRejectsWhatInjectionWouldRefuse(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		mode          os.FileMode
		wantErr       string
	}{
		{"group readable", "KEY=value\n", 0o640, "readable by other users"},
		{"world readable", "KEY=value\n", 0o644, "readable by other users"},
		{"shell command", "KEY=value\nrm -rf /\n", 0o600, "not KEY=VALUE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := envFileAt(t, tc.content, tc.mode)
			_, err := absEnvFile(p)
			if err == nil {
				t.Fatalf("%s was accepted; injection would have refused it", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error should mention %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestAbsEnvFileRejectsAMissingFile(t *testing.T) {
	_, err := absEnvFile(filepath.Join(t.TempDir(), "nope.env"))
	if err == nil {
		t.Fatal("a nonexistent env file was accepted")
	}
}

// The sshfs error has to name the fix. A bare "not found" sends the reader to
// a search engine for a dependency they were never told about.
func TestSshfsHintNamesBothRoutes(t *testing.T) {
	msg := fmt.Sprintf(sshfsHint, "myvm")
	for _, want := range []string{"nix shell nixpkgs#sshfs", "rig mount myvm", "apt install sshfs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("hint should contain %q, got:\n%s", want, msg)
		}
	}
}

// Guest files are root-owned, so git refuses the tree. That failure is opaque
// unless you have hit it before, so the path must be spelled out ready to paste.
func TestGitSafeHintIsPasteable(t *testing.T) {
	msg := gitSafeHint("/srv/rig/myvm-live")
	if !strings.Contains(msg, "git config --global --add safe.directory /srv/rig/myvm-live") {
		t.Errorf("hint must be a runnable command, got:\n%s", msg)
	}
}

// Ctrl-C is how `rig mount` is meant to end, so the verbs must judge the
// outcome — is this still a mount — rather than the exit code of the command
// they wrapped, which is non-zero for an interrupt that did exactly what was
// asked.
func TestIsMountedReadsProcMounts(t *testing.T) {
	if isMounted("/definitely/not/a/mount/point/xyzzy") {
		t.Error("reported a nonexistent path as mounted")
	}
	// / is always a mount on Linux; if this fails the parser is wrong.
	if !isMounted("/") {
		t.Error("did not recognise / as a mount; /proc/mounts parsing is broken")
	}
}

// `rig creds` exists so a stale credential does not cost a VM restart. Its
// signature and help are the contract an operator reads under time pressure,
// with an agent already failing to authenticate, so pin them.
func TestCredsCmdIsWiredForTheStaleSnapshotCase(t *testing.T) {
	c := (&app{}).credsCmd()

	if c.GroupID != "guest" {
		t.Errorf("belongs with the verbs that work inside a guest, got %q", c.GroupID)
	}
	// The credential file is an argument, not a flag over a remembered path.
	// Defaulting to whatever the instance had recorded meant the command named
	// neither the file it read nor the directory it read it from.
	if err := c.Args(c, []string{"vm"}); err == nil {
		t.Error("must refuse a bare VM name: the credential file has to be named")
	}
	if err := c.Args(c, []string{"vm", "creds.env"}); err != nil {
		t.Errorf("must accept <vm> <env-file>: %v", err)
	}
	if c.Flags().Lookup("env") != nil {
		t.Error("--env must be gone; the file is a positional argument now")
	}
	if !strings.Contains(c.Use, "<vm>") || !strings.Contains(c.Use, "<env-file>") {
		t.Errorf("usage must name both operands, got %q", c.Use)
	}
	if !strings.Contains(c.Long, creds.GuestPath) {
		t.Error("help must name where the credential lands")
	}
	if !strings.Contains(c.Long, "next restarts") {
		t.Error("help must say a running agent is not interrupted — that is the point of the verb")
	}
	if !strings.Contains(c.Long, "current directory") {
		t.Error("help must say what a relative path resolves against")
	}
}

// Every verb that takes a VM says so. `<name>` reads like a free-form label —
// an instance name is the one thing all of these share, and the placeholder is
// where an operator learns it.
func TestVerbsNameTheirVMOperand(t *testing.T) {
	a := &app{}
	for _, c := range []*cobra.Command{
		a.newCmd(), a.startCmd(), a.stopCmd(), a.restartCmd(), a.rmCmd(),
		a.doctorCmd(), a.verifyCmd(), a.logsCmd(), a.execCmd(), a.shellCmd(),
		a.pushCmd(), a.pullCmd(), a.credsCmd(), a.mountCmd(), a.claimCmd(),
	} {
		if strings.Contains(c.Use, "<name>") {
			t.Errorf("%q still says <name>; an operand that is a VM should say <vm>", c.Use)
		}
	}
}

// The default --flake is relative, so `rig image build` outside the rig
// checkout resolved a path the operator never typed. The error has to name the
// assumption, not just the path it produced.
func TestFlakeRefExplainsTheDefaultItAssumed(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "base")

	_, err := flakeRef(missing, false)
	if err == nil {
		t.Fatal("a directory with no flake.nix must be an error")
	}
	for _, want := range []string{"--flake", "current directory", "RIG_FLAKE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("an unnamed default must explain %q; got: %v", want, err)
		}
	}

	// When the operator named it, the path is the whole story.
	_, err = flakeRef(missing, true)
	if err == nil {
		t.Fatal("a named directory with no flake.nix is still an error")
	}
	if strings.Contains(err.Error(), "defaulted to") {
		t.Errorf("a named --flake must not be reported as a default: %v", err)
	}
}

// A flake ref is passed through untouched: it is not a path and must not be
// resolved against the current directory.
func TestFlakeRefPassesAReferenceThrough(t *testing.T) {
	got, err := flakeRef("github:owner/repo", false)
	if err != nil || got != "github:owner/repo" {
		t.Errorf("flake refs go through unchanged, got %q (%v)", got, err)
	}
}

// Inside a git tree the ref is the bare path, so nix reads it through git and
// a relative input such as project-template/guest's `path:../../base` resolves.
// With a `path:` prefix the directory is copied into the store first and the
// relative input is resolved against the copy, where it points at nothing —
// verified as "access to absolute path '/nix/base/flake.nix' is forbidden".
func TestFlakeRefIsBareInsideAGitTreeAndPathOutsideOne(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, "flake.nix"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := flakeRef(plain, true); err != nil || got != "path:"+plain {
		t.Errorf("outside git: got %q (%v), want path:%s", got, err, plain)
	}

	repo := t.TempDir()
	dir := filepath.Join(repo, "guest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "flake.nix"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	got, err := flakeRef(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got, "path:") {
		t.Errorf("inside git the ref must be bare so relative inputs resolve, got %q", got)
	}
	// Resolved through git; the untracked flake.nix is the case the warning is for.
	if !inGitTree(dir) || len(untrackedFiles(dir)) == 0 {
		t.Error("expected the untracked flake.nix to be reported")
	}
}

// A project with its own guest image must not be reported as drifted from a
// base it was never built from. The recorded alias is the honest comparison;
// the default is only right while there is one image on the host.
func TestDriftAliasPrefersWhatTheVMWasBuiltFrom(t *testing.T) {
	if got := driftAlias("myproj-guest", defaultImage, false); got != "myproj-guest" {
		t.Errorf("compared against %q, want the recorded alias myproj-guest", got)
	}
	// Nothing recorded: instances created before the key existed still compare
	// against the default rather than against nothing.
	if got := driftAlias("", defaultImage, false); got != defaultImage {
		t.Errorf("compared against %q, want the default %q", got, defaultImage)
	}
	// An explicit --image is a different question, asked on purpose.
	if got := driftAlias("myproj-guest", "some-other", true); got != "some-other" {
		t.Errorf("compared against %q, want the explicit flag value", got)
	}
}

// The manifest becomes instance config, and drift is read back from the same
// keys, so a round trip through both must be silent.
func TestDesiredConfigRoundTripsThroughDrift(t *testing.T) {
	env := envFileAt(t, "KEY=value\n", 0o600)
	m, err := manifest.Parse([]byte(`
host:
  devices:
    gpu: { kind: gpu, pci: "0000:2b:00.0", return: { modules: [nvidia], reset: true, alive: drm/card* } }
    desk-usb: { kind: pci, pci: "0000:3c:00.3", id: "1022:1111" }
guest:
  name: myproj
  flake: ./guest
  cpus: 4
  memory: 8GiB
  env_file: ` + env + `
  devices: [gpu, desk-usb]
  ports: ["47989:47989/tcp", "47998-48000:47998-48000/udp"]
`))
	if err != nil {
		t.Fatal(err)
	}
	m.Path = "/somewhere/rig.yaml"
	want, err := desired(m)
	if err != nil {
		t.Fatal(err)
	}
	inst := &incus.Instance{Name: "myproj", Config: want}
	if drift := configDrift(inst, want); len(drift) != 0 {
		t.Fatalf("an instance made from the manifest drifts from it: %v", drift)
	}
	if want[imageKey] != "myproj-guest" || want["limits.cpu"] != "4" || want[creds.InstanceKey] != env {
		t.Errorf("desired config wrong: %v", want)
	}
	wanted, err := devices.Wanted(inst, devices.Config{})
	if err != nil || len(wanted) != 2 || wanted[0].Return == nil || wanted[0].Return.Alive != "drm/card*" || wanted[1].Return != nil {
		t.Errorf("recorded devices did not round-trip: %+v %v", wanted, err)
	}
	if want[portsKey] != `["47989:47989/tcp","47998-48000:47998-48000/udp"]` {
		t.Errorf("ports recorded as %s", want[portsKey])
	}

	// Change one thing in the file: exactly that key drifts, and the path the
	// file records for itself never counts.
	m.Guest.CPUs = manifest.CPUs{Count: 8}
	m.Path = "/elsewhere/rig.yaml"
	changed, _ := desired(m)
	drift := configDrift(inst, changed)
	if len(drift) != 1 || !strings.HasPrefix(drift[0], "limits.cpu: 4 -> 8") {
		t.Errorf("drift = %v, want only limits.cpu", drift)
	}
}

// The argv handed to sudo is the whole truth about what runs as root, so it
// must carry every field of the recipe and nothing the recipe left out.
func TestReturnArgsCarryTheWholeRecipe(t *testing.T) {
	full := hostdev.Spec{PCI: "0000:2b:00.0", Modules: []string{"nvidia_drm", "nvidia"}, Reset: true, Alive: "drm/card*", Unit: "gdm"}
	got := strings.Join(returnArgs(full), " ")
	want := "host return --pci=0000:2b:00.0 --modules=nvidia_drm,nvidia --reset --alive=drm/card* --unit=gdm"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	bare := strings.Join(returnArgs(hostdev.Spec{PCI: "0000:3c:00.3", Reset: true, Alive: "usb*"}), " ")
	if strings.Contains(bare, "--modules") || strings.Contains(bare, "--unit") {
		t.Errorf("a recipe without modules or a unit must not name them: %q", bare)
	}
	if got := strings.Join(freeArgs(full), " "); got != "host free --pci=0000:2b:00.0 --unit=gdm --modules=nvidia_drm,nvidia" {
		t.Errorf("free args = %q", got)
	}
}

// A device is "the same" when its identity matches, whatever else Incus has
// added to the map since — it decorates devices with keys rig never set.
func TestSameDeviceIgnoresWhatIncusAdds(t *testing.T) {
	d := devices.Decl{Name: "gpu", Kind: "gpu", PCI: "0000:2b:00.0"}
	if !sameDevice(incus.Device{"type": "gpu", "gputype": "physical", "pci": "0000:2b:00.0", "extra": "x"}, d) {
		t.Error("an extra key made the same device look different")
	}
	if sameDevice(incus.Device{"type": "gpu", "gputype": "physical", "pci": "0000:05:00.0"}, d) {
		t.Error("a different address was accepted")
	}
}

// The guest probe for a USB device must not depend on lsusb, which the image
// does not carry, and the gpu probe is the image's own check.
func TestGuestProbeShapes(t *testing.T) {
	cmd, label := guestProbe(devices.Decl{Name: "gpu", Kind: "gpu"})
	if !strings.HasSuffix(cmd, "; gpu-check") || !strings.Contains(cmd, "rig.nixosModules.nvidia") || label == "" {
		t.Errorf("gpu probe = %q %q", cmd, label)
	}
	cmd, label = guestProbe(devices.Decl{Name: "mouse", Kind: "usb", ID: "1234:5678"})
	if !strings.Contains(cmd, "/sys/bus/usb/devices") || strings.Contains(cmd, "lsusb") || label != "mouse visible in guest" {
		t.Errorf("usb probe = %q %q", cmd, label)
	}
}

// The NAR hash mismatch is what nix says when a guest flake's lock pins a base
// copy that a rebuilt rig has since rewritten. It happened twice in one
// afternoon here; the hint has to name the lock file to delete.
func TestStaleLockIsRecognisedAndNamesTheLock(t *testing.T) {
	stderr := "       error: NAR hash mismatch in input 'path:/home/me/.cache/rig/base-abc-dirty?narHash=sha256-2Inh%3D', " +
		"expected 'sha256-2Inh=' but got 'sha256-tSbj='\n"
	input, ok := staleLock(stderr)
	if !ok || input != "path:/home/me/.cache/rig/base-abc-dirty?narHash=sha256-2Inh%3D" {
		t.Fatalf("staleLock = %q, %v", input, ok)
	}
	hint := staleLockHint("path:/home/me/proj/guest#nixosConfigurations.guest.config.system.build.qemuImage", input)
	for _, want := range []string{"rm /home/me/proj/guest/flake.lock", "/home/me/.cache/rig/base-abc-dirty\n"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint should contain %q:\n%s", want, hint)
		}
	}
	if strings.Contains(hint, "narHash") {
		t.Error("the hint repeats the hash query, which says nothing to a reader")
	}
	if _, ok := staleLock("error: builder for '/nix/store/x.drv' failed with exit code 1"); ok {
		t.Error("an ordinary build failure was called a stale lock")
	}
}

// network: none removes every NIC: a profile's by masking it (the only way to
// take an inherited device away in Incus), the instance's own (the override
// ports leave) by deleting it. Lifting it removes only masks over profile NICs;
// a `none` device over something else was put there by someone else.
func TestNetworkPlanMasksInheritedNICsAndLiftsOnlyThoseMasks(t *testing.T) {
	profileNICs := map[string]bool{"eth0": true}
	withNIC := &incus.Instance{
		Name:    "daw",
		Devices: map[string]incus.Device{"eth1": {"type": "nic", "network": "incusbr0"}},
		ExpandedDevices: map[string]incus.Device{
			"eth0": {"type": "nic", "network": "incusbr0"},
			"eth1": {"type": "nic", "network": "incusbr0"},
			"root": {"type": "disk"},
		},
	}
	devices, changes := networkPlan(withNIC, profileNICs, true)
	if devices["eth0"].Type() != "none" {
		t.Errorf("the profile's eth0 must be masked, got %v", devices["eth0"])
	}
	if _, ok := devices["eth1"]; ok {
		t.Errorf("the instance's own eth1 must be deleted, got %v", devices["eth1"])
	}
	if len(changes) != 2 {
		t.Errorf("want one change per NIC, got %v", changes)
	}

	masked := &incus.Instance{
		Name: "daw",
		Devices: map[string]incus.Device{
			"eth0":  {"type": "none"},
			"other": {"type": "none"},
		},
		ExpandedDevices: map[string]incus.Device{"eth0": {"type": "none"}, "other": {"type": "none"}},
	}
	if _, changes := networkPlan(masked, profileNICs, true); len(changes) != 0 {
		t.Errorf("already without a NIC: want no changes, got %v", changes)
	}
	devices, changes = networkPlan(masked, profileNICs, false)
	if _, ok := devices["eth0"]; ok || len(changes) != 1 {
		t.Errorf("the mask over the profile's eth0 must be lifted: devices=%v changes=%v", devices, changes)
	}
	if devices["other"].Type() != "none" {
		t.Error("a none device over something that is not a profile NIC is not rig's to remove")
	}
}

// rig init shows the card, identity included, as a commented example, and
// leaves the id line out rather than inventing one when it could not be read.
// Either way the result must be a manifest that parses and grants nothing.
func TestFillManifestWritesTheCardIdentityOrNone(t *testing.T) {
	tmpl, err := os.ReadFile("../../project-template/rig.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"10de:abcd", ""} {
		filled := fillManifest(string(tmpl), "proj", "0000:2b:00.0", id)
		m, err := manifest.Parse([]byte(filled))
		if err != nil {
			t.Fatalf("id %q: filled template does not parse: %v", id, err)
		}
		if m.Guest.Name != "proj" || len(m.Host.Devices) != 0 || len(m.Guest.Devices) != 0 {
			t.Errorf("id %q: the template must grant nothing: %+v", id, m)
		}
		if id != "" && !strings.Contains(filled, "#   id: "+id) {
			t.Errorf("id %q: the card's identity is not in the example", id)
		}
		if !strings.Contains(filled, "#   pci: 0000:2b:00.0") {
			t.Error("the card's address is not in the example")
		}
		if strings.Contains(filled, "GPU_ID") || strings.Contains(filled, "GPU_PCI") {
			t.Errorf("id %q: a placeholder survived", id)
		}
	}
}

// The one-forwarder rule reads systemd's listing of the background units.
func TestInputUnitVMsReadsTheListing(t *testing.T) {
	listing := "rig-input-daw.service loaded active running rig: this host's keyboard and mouse, lent to daw\n" +
		"rig-input-my-proj.service loaded active running rig: this host's keyboard and mouse, lent to my-proj\n"
	got := inputUnitVMs(listing)
	if len(got) != 2 || got[0] != "daw" || got[1] != "my-proj" {
		t.Fatalf("got %v", got)
	}
	if got := inputUnitVMs(""); len(got) != 0 {
		t.Fatalf("empty listing: got %v", got)
	}
}

// guest.input is always recorded, so taking the line out of the file is
// drift that apply undoes, not silence that leaves input lent.
func TestRemovingInputFromTheFileIsDrift(t *testing.T) {
	m, err := manifest.Parse([]byte("guest:\n  name: daw\n  image: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := desired(m)
	if err != nil {
		t.Fatal(err)
	}
	inst := &incus.Instance{Name: "daw", Config: map[string]string{}}
	for k, v := range want {
		inst.Config[k] = v
	}
	inst.Config[inputKey] = manifest.InputHost
	drift := configDrift(inst, want)
	if len(drift) != 1 || !strings.HasPrefix(drift[0], inputKey+": host -> <unset>") {
		t.Fatalf("drift = %v", drift)
	}
}

// A forwarder started by hand holds the keyboard as surely as the background
// unit does; the root half's command line is how it is recognised, and sudo's
// own process, which carries the same arguments, counts too.
func TestTerminalForwarderIsRecognisedByItsArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"/home/me/rig", "host", "input", "daw", "--cid", "5"}, true},
		{[]string{"sudo", "--", "/home/me/rig", "host", "input", "daw", "--cid", "5"}, true},
		{[]string{"/home/me/rig", "host", "input", "daw", "--cid", "5", "--follow"}, false},
		{[]string{"/home/me/rig", "host", "input", "daw"}, false}, // the unprivileged half, still checking
		{[]string{"/home/me/rig", "doctor", "daw"}, false},
	} {
		if got := isTerminalForwarder(tc.args); got != tc.want {
			t.Errorf("%v: got %v", tc.args, got)
		}
	}
}

// --with nvidia grants the card the template shows, --with desktop also lends
// input, and --image swaps the flake for an alias; each result must parse and
// say exactly that.
func TestInitVariantsWriteManifestsThatSayWhatWasChosen(t *testing.T) {
	tmpl, err := os.ReadFile("../../project-template/rig.yaml")
	if err != nil {
		t.Fatal(err)
	}
	base := fillManifest(string(tmpl), "proj", "0000:2b:00.0", "10de:abcd")

	m, err := manifest.Parse([]byte(uncommentLine(grantCard(base), "input: host")))
	if err != nil {
		t.Fatalf("granted: %v", err)
	}
	gpu := m.Host.Devices["gpu"]
	if gpu.PCI != "0000:2b:00.0" || gpu.ID != "10de:abcd" || gpu.Return == nil || len(gpu.Return.Modules) != 4 {
		t.Errorf("gpu not granted as the template shows it: %+v", gpu)
	}
	if len(m.Guest.Devices) != 1 || m.Guest.Devices[0] != "gpu" || m.Guest.Input != manifest.InputHost {
		t.Errorf("guest: %+v", m.Guest)
	}

	m, err = manifest.Parse([]byte(useImage(base, "rig-nvidia")))
	if err != nil || m.Guest.Image != "rig-nvidia" || m.Guest.Flake != "" {
		t.Fatalf("image: %+v %v", m, err)
	}
}

func TestInitModules(t *testing.T) {
	if _, err := parseWith([]string{"cuda"}); err == nil {
		t.Error("an unknown module must be refused")
	}
	mods, _ := parseWith([]string{"desktop", "docker"})
	if !mods["nvidia"] {
		t.Error("desktop runs on the card, so it brings nvidia")
	}
	flake := "outputs = { rig, ... }: {\n  nixosConfigurations.guest = " + mkGuestLine + ";\n};"
	got, ok := addModules(flake, mods)
	if !ok || !strings.Contains(got, "rig.lib.mkGuest [ rig.nixosModules.docker rig.nixosModules.desktop ./guest.nix ]") {
		t.Errorf("modules: %s", got)
	}
}

// rig init points a new rig.yaml's editor comment at the schema matching the
// rig that wrote it, and leaves the rest of the file alone.
func TestPointAtSchemaRewritesOnlyTheComment(t *testing.T) {
	tmpl, err := os.ReadFile("../../project-template/rig.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got := pointAtSchema(string(tmpl), "/home/me/.cache/rig/rig.schema-abc.json")
	if !strings.HasPrefix(got, "# yaml-language-server: $schema=/home/me/.cache/rig/rig.schema-abc.json\n") {
		t.Errorf("first line: %q", strings.SplitN(got, "\n", 2)[0])
	}
	if strings.SplitN(got, "\n", 2)[1] != strings.SplitN(string(tmpl), "\n", 2)[1] {
		t.Error("more than the comment changed")
	}
}
