package pushguard

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGuest is an in-memory stand-in for the guest filesystem.
type fakeGuest struct {
	files map[string][]byte
}

func newGuest() *fakeGuest { return &fakeGuest{files: map[string][]byte{}} }

func (g *fakeGuest) Pull(_, p string) ([]byte, error) {
	b, ok := g.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}
func (g *fakeGuest) WriteFile(_, p string, c []byte, _ fs.FileMode) error {
	g.files[p] = append([]byte(nil), c...)
	return nil
}
func (g *fakeGuest) Mkdir(_, _ string, _ fs.FileMode) error { return nil }

func stage(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// push simulates cmd/rig: check, then copy, then record.
func push(t *testing.T, g *fakeGuest, hostDir, guestDir string) []Conflict {
	t.Helper()
	conflicts, err := Check(g, "vm", hostDir, guestDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) > 0 {
		return conflicts
	}
	contents, err := ReadAll(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	for rel, b := range contents {
		g.files[filepath.Join(guestDir, rel)] = b
	}
	if err := Record(g, "vm", hostDir, guestDir, contents); err != nil {
		t.Fatal(err)
	}
	return nil
}

// The first push has nothing to destroy.
func TestFirstPushIsClean(t *testing.T) {
	g := newGuest()
	dir := stage(t, map[string]string{"MISSION.md": "v1", "run-agent": "#!/bin/sh"})
	if c := push(t, g, dir, "/seed"); c != nil {
		t.Fatalf("first push reported conflicts: %v", c)
	}
	if string(g.files["/seed/MISSION.md"]) != "v1" {
		t.Fatal("push did not land")
	}
}

// Re-pushing an edited staging directory is the normal workflow. The guard must
// not get in its way, or it will be forced off and stop protecting anything.
func TestRePushingOurOwnFilesIsAllowed(t *testing.T) {
	g := newGuest()
	dir := stage(t, map[string]string{"MISSION.md": "v1"})
	push(t, g, dir, "/seed")

	if err := os.WriteFile(filepath.Join(dir, "MISSION.md"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := push(t, g, dir, "/seed"); c != nil {
		t.Fatalf("re-pushing rig's own unmodified file was refused: %v", c)
	}
	if string(g.files["/seed/MISSION.md"]) != "v2" {
		t.Fatal("the edit did not land")
	}
}

// The regression this package exists for: the guest edited a file rig had
// written, and a later push silently destroyed it.
func TestGuestSideEditIsProtected(t *testing.T) {
	g := newGuest()
	dir := stage(t, map[string]string{"ESCALATION.md": "template"})
	push(t, g, dir, "/seed")

	// The agent appends its own entry inside the guest.
	g.files["/seed/ESCALATION.md"] = []byte("template\n\n## a hard-won decision")

	conflicts, err := Check(g, "vm", dir, "/seed")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("want 1 conflict, got %d: %v", len(conflicts), conflicts)
	}
	if conflicts[0].GuestPath != "/seed/ESCALATION.md" {
		t.Fatalf("wrong file: %s", conflicts[0].GuestPath)
	}
	if !strings.Contains(conflicts[0].Why, "changed inside the guest") {
		t.Fatalf("unhelpful reason: %q", conflicts[0].Why)
	}
	if string(g.files["/seed/ESCALATION.md"]) == "template" {
		t.Fatal("the guest's version was destroyed")
	}
}

// A file rig has no record of is not rig's to overwrite, even on a first push.
func TestUnrecordedGuestFileIsProtected(t *testing.T) {
	g := newGuest()
	g.files["/seed/NOTES.md"] = []byte("written by someone else")

	dir := stage(t, map[string]string{"NOTES.md": "mine"})
	conflicts, err := Check(g, "vm", dir, "/seed")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("want 1 conflict, got %d", len(conflicts))
	}
	if !strings.Contains(conflicts[0].Why, "no record") {
		t.Fatalf("reason should distinguish unrecorded from modified, got %q", conflicts[0].Why)
	}
}

// A corrupt manifest must make the guard conservative, not crash the command.
func TestCorruptManifestRefusesRatherThanBreaks(t *testing.T) {
	g := newGuest()
	dir := stage(t, map[string]string{"MISSION.md": "v1"})
	push(t, g, dir, "/seed")

	g.files[ManifestPath] = []byte("{not json")

	conflicts, err := Check(g, "vm", dir, "/seed")
	if err != nil {
		t.Fatalf("a corrupt manifest broke the command: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("a corrupt manifest must be treated as no record; got %d conflicts", len(conflicts))
	}
}

// Nested files must be covered — a guard that only sees the top level is worse
// than none, because it reads as protection.
func TestNestedFilesAreChecked(t *testing.T) {
	g := newGuest()
	dir := stage(t, map[string]string{"secrets/token": "v1"})
	push(t, g, dir, "/seed")

	g.files["/seed/secrets/token"] = []byte("rotated in the guest")
	conflicts, err := Check(g, "vm", dir, "/seed")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].GuestPath != "/seed/secrets/token" {
		t.Fatalf("nested file not protected: %v", conflicts)
	}
}

func TestManifestRecordsWhatWasSent(t *testing.T) {
	g := newGuest()
	dir := stage(t, map[string]string{"a.txt": "A", "sub/b.txt": "B"})
	push(t, g, dir, "/seed")

	var m Manifest
	if err := json.Unmarshal(g.files[ManifestPath], &m); err != nil {
		t.Fatalf("manifest is not valid json: %v", err)
	}
	for _, want := range []string{"/seed/a.txt", "/seed/sub/b.txt"} {
		if _, ok := m[want]; !ok {
			t.Errorf("manifest missing %s", want)
		}
	}
}

// The message has to name files, because deciding whether to force is a
// per-file judgement that a count cannot inform.
func TestErrorNamesEveryFileAndHowToLook(t *testing.T) {
	err := Error([]Conflict{
		{"/work/notes/DECISIONS.md", "changed inside the guest since rig last wrote it"},
	}, "myvm", "/work/notes")
	msg := err.Error()
	for _, want := range []string{"/work/notes/DECISIONS.md", "rig pull myvm", "--force"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message should contain %q, got:\n%s", want, msg)
		}
	}
}
