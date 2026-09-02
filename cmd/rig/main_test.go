package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"rig/internal/creds"
	"rig/internal/incus"
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

// A VM created before --no-gpu existed has no marker, and must keep claiming
// the card. Defaulting the other way would silently strip the GPU from every
// existing project.
func TestWantsGPUDefaultsToYes(t *testing.T) {
	for name, cfg := range map[string]map[string]string{
		"no config at all": nil,
		"unrelated keys":   {"user.rig.env": "/x"},
		"explicit true":    {gpuKey: "true"},
	} {
		if !wantsGPU(&incus.Instance{Config: cfg}) {
			t.Errorf("%s: should claim the card", name)
		}
	}
}

// Only the exact marker opts out, so a typo cannot quietly disable the GPU.
func TestWantsGPUOptsOutOnlyOnTheExactMarker(t *testing.T) {
	if wantsGPU(&incus.Instance{Config: map[string]string{gpuKey: "false"}}) {
		t.Error("an explicit false must not claim the card")
	}
	for _, v := range []string{"False", "FALSE", "0", "no", ""} {
		if !wantsGPU(&incus.Instance{Config: map[string]string{gpuKey: v}}) {
			t.Errorf("%q is not the marker; it must not disable the GPU", v)
		}
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
	msg := gitSafeHint("/home/theo/dev/vm-live")
	if !strings.Contains(msg, "git config --global --add safe.directory /home/theo/dev/vm-live") {
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

// A project with its own guest image must not be reported as drifted from a
// base it was never built from. The recorded alias is the honest comparison;
// the default is only right while there is one image on the host.
func TestDriftAliasPrefersWhatTheVMWasBuiltFrom(t *testing.T) {
	if got := driftAlias("og-guest", defaultImage, false); got != "og-guest" {
		t.Errorf("compared against %q, want the recorded alias og-guest", got)
	}
	// Nothing recorded: instances created before the key existed still compare
	// against the default rather than against nothing.
	if got := driftAlias("", defaultImage, false); got != defaultImage {
		t.Errorf("compared against %q, want the default %q", got, defaultImage)
	}
	// An explicit --image is a different question, asked on purpose.
	if got := driftAlias("og-guest", "some-other", true); got != "some-other" {
		t.Errorf("compared against %q, want the explicit flag value", got)
	}
}
