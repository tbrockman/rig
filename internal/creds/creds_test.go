package creds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile is subject to umask; force the mode we asked for.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateAcceptsCommentsAndBlanks(t *testing.T) {
	path := write(t, "# a comment\n\nKEY=value\nOTHER_1=with spaces and = signs\n", 0o600)
	if err := Validate(path); err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
	if n := Count(path); n != 2 {
		t.Fatalf("Count = %d, want 2", n)
	}
}

func TestValidateRejectsGroupOrWorldReadable(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		path := write(t, "KEY=value\n", mode)
		err := Validate(path)
		if err == nil {
			t.Errorf("mode %04o accepted; credentials must not be readable by others", mode)
			continue
		}
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %04o: error should say how to fix it, got %v", mode, err)
		}
	}
}

// The file is sourced in the guest, so anything that is not an assignment is a
// command waiting to run.
func TestValidateRejectsNonAssignments(t *testing.T) {
	path := write(t, "KEY=value\nrm -rf /\n", 0o600)
	err := Validate(path)
	if err == nil {
		t.Fatal("a shell command in an env file was accepted")
	}
	if !strings.Contains(err.Error(), "2:rm -rf /") {
		t.Fatalf("error should point at the offending line, got %v", err)
	}
}

func TestValidateMissingFile(t *testing.T) {
	if err := Validate(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("missing file accepted")
	}
}
