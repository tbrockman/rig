package pullguard

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// guest stands in for the guest's copy of a file, and counts transfers: the
// cases rig sees on every refresh must cost none.
type guest struct {
	content string
	reads   int
}

func (g *guest) write(w io.Writer) error {
	g.reads++
	_, err := io.WriteString(w, g.content)
	return err
}

func write(t *testing.T, p, content string) {
	t.Helper()
	err := Write(p, 0o644, time.Time{}, func(w io.Writer) error {
		_, err := io.WriteString(w, content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// needXattrs skips where the test filesystem cannot hold the mark, which
// Write tolerates but the refresh cases depend on.
func needXattrs(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "probe")
	os.WriteFile(p, nil, 0o644)
	if err := syscall.Setxattr(p, attr, []byte("x"), 0); err != nil {
		t.Skipf("no user xattrs on %s: %v", filepath.Dir(p), err)
	}
}

// The normal workflow: pull, the agent works on, pull again.
func TestARefreshOverwritesWhatRigWroteWithoutAsking(t *testing.T) {
	needXattrs(t)
	p := filepath.Join(t.TempDir(), "STATUS.md")
	write(t, p, "day one\n")

	g := &guest{content: "day two\n"}
	c, err := Check(p, g.write)
	if err != nil || c != nil {
		t.Fatalf("an unchanged file rig wrote must be overwritable: %v, %v", c, err)
	}
	if g.reads != 0 {
		t.Error("a file that matches its mark must not cost a transfer")
	}
}

func TestAHostEditIsRefused(t *testing.T) {
	needXattrs(t)
	p := filepath.Join(t.TempDir(), "STATUS.md")
	write(t, p, "day one\n")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("my annotation\n")
	f.Close()

	c, err := Check(p, (&guest{content: "day two\n"}).write)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || !strings.Contains(c.Why, "changed on the host") {
		t.Errorf("an edited file must be refused as changed, got %+v", c)
	}
}

// A file rig never wrote is the operator's, unless overwriting it would change
// nothing — the case of a tree first copied out some other way.
func TestAnUnmarkedFileIsRefusedUnlessItMatches(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notes.md")
	os.WriteFile(p, []byte("same\n"), 0o644)

	if c, err := Check(p, (&guest{content: "same\n"}).write); err != nil || c != nil {
		t.Errorf("an identical file is no loss: %+v, %v", c, err)
	}
	c, err := Check(p, (&guest{content: "different\n"}).write)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || !strings.Contains(c.Why, "did not write") {
		t.Errorf("a differing file rig did not write must be refused, got %+v", c)
	}
}

func TestNothingThereIsNothingToLose(t *testing.T) {
	g := &guest{}
	c, err := Check(filepath.Join(t.TempDir(), "new"), g.write)
	if err != nil || c != nil || g.reads != 0 {
		t.Errorf("got %+v, %v, %d reads", c, err, g.reads)
	}
}

func TestADirectoryInTheWayIsAConflict(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "x"), 0o755)
	os.WriteFile(filepath.Join(dir, "y"), nil, 0o644)

	if c, _ := Check(filepath.Join(dir, "x"), (&guest{}).write); c == nil {
		t.Error("a host directory where the guest has a file must be refused")
	}
	if c := CheckDir(filepath.Join(dir, "y")); c == nil {
		t.Error("a host file where the guest has a directory must be refused")
	}
	if c := CheckDir(filepath.Join(dir, "x")); c != nil {
		t.Errorf("an existing directory is fine: %+v", c)
	}
}

// Write carries the guest's mode and mtime, and a failed transfer leaves the
// old file whole with no temporary beside it.
func TestWriteIsAtomicAndKeepsModeAndTime(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "run.sh")
	when := time.Date(2026, 10, 16, 2, 4, 31, 0, time.UTC)
	err := Write(p, 0o755, when, func(w io.Writer) error {
		_, err := io.WriteString(w, "#!/bin/sh\n")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o755 || !st.ModTime().Equal(when) {
		t.Errorf("mode %v, mtime %v", st.Mode().Perm(), st.ModTime())
	}

	err = Write(p, 0o755, when, func(w io.Writer) error {
		io.WriteString(w, "half")
		return errors.New("connection reset")
	})
	if err == nil {
		t.Fatal("a failed transfer must be an error")
	}
	if b, _ := os.ReadFile(p); string(b) != "#!/bin/sh\n" {
		t.Errorf("the old file was damaged: %q", b)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("a temporary file was left behind: %v", ents)
	}
}

// A refresh of a large tree must not transfer what it already has, and must
// never take an edited file for current because its size and time match.
func TestCurrentNeedsTheMarkAsWellAsSizeAndTime(t *testing.T) {
	needXattrs(t)
	p := filepath.Join(t.TempDir(), "capture.pcap")
	when := time.Date(2026, 10, 16, 2, 4, 31, 0, time.UTC)
	Write(p, 0o644, when, func(w io.Writer) error { _, err := io.WriteString(w, "abcd"); return err })

	if !Current(p, 4, when) {
		t.Error("a marked, unchanged file with the guest's size and time is current")
	}
	if Current(p, 4, when.Add(time.Second)) || Current(p, 5, when) {
		t.Error("a different guest time or size is not current")
	}
	os.WriteFile(p, []byte("wxyz"), 0o644)
	os.Chtimes(p, when, when)
	if Current(p, 4, when) {
		t.Error("an edit that kept the size and time is still an edit")
	}
}

// git's objects are read-only, and a user xattr needs write permission: a mark
// set after the mode would be silently missing, and every refresh would fetch
// those files again.
func TestAReadOnlyFileIsMarkedToo(t *testing.T) {
	needXattrs(t)
	p := filepath.Join(t.TempDir(), "object")
	Write(p, 0o444, time.Time{}, func(w io.Writer) error { _, err := io.WriteString(w, "blob"); return err })
	if readMark(p) == "" {
		t.Error("a read-only file was left unmarked")
	}
}
