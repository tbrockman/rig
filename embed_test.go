package rig

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// The embed patterns are explicit, so a file added to the template or the base
// is invisible to a `go install`ed rig until it is listed here. This test is
// what makes forgetting that a failure rather than a surprise months later.
func TestEmbeddedAssetsAreExactlyWhatGitTracks(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "base", "project-template").Output()
	if err != nil {
		t.Skip("not in a git checkout")
	}
	want := strings.Fields(string(out))
	sort.Strings(want)

	var got []string
	err = fs.WalkDir(assets, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			got = append(got, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("embedded files differ from what git tracks.\nembedded:\n  %s\ngit:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// A script written without its mode is a file nobody can run, and the
// failure shows up as "permission denied" from inside the guest.
func TestWriteTreeMakesScriptsExecutableAndHonoursSkip(t *testing.T) {
	src := fstest.MapFS{
		"run":           {Data: []byte("#!/bin/sh\necho hi\n")},
		"guest/a.nix":   {Data: []byte("{ }\n")},
		"guest/skipped": {Data: []byte("x")},
	}
	dir := t.TempDir()
	if err := WriteTree(src, dir, func(p string) bool { return p == "guest/skipped" }); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "run"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0o111 == 0 {
		t.Error("a script was written without an executable bit")
	}
	if st, err := os.Stat(filepath.Join(dir, "guest", "a.nix")); err != nil || st.Mode()&0o111 != 0 {
		t.Errorf("guest/a.nix: %v, mode %v", err, st)
	}
	if _, err := os.Stat(filepath.Join(dir, "guest", "skipped")); err == nil {
		t.Error("a skipped file was written")
	}
}

// rig init writes the whole template: the manifest and the guest flake.
func TestProjectTemplateIsTheManifestAndGuestFlake(t *testing.T) {
	dir := t.TempDir()
	if err := WriteTree(ProjectTemplate(), dir, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"rig.yaml", "guest/flake.nix", "guest/guest.nix", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s missing from the written template", p)
		}
	}
}
