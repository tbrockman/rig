package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
