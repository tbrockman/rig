package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	rig "github.com/tbrockman/rig"
)

// rig init writes the project template into a directory.
//
// The template lives in the rig checkout as project-template/, which a
// `go install`ed rig does not have. So the binary carries it and writes out
// the copy it was built with — the one whose guest flake calls the mkGuest
// this binary's `rig image build` expects. The same embedded copy of base/ is
// what `rig image build` falls back to when there is no checkout to read.

func (a *app) initCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "init <dir>",
		GroupID: "vm",
		Short:   "Write the project template into a directory",
		Long: "Writes the project template into <dir>: a CUDA devShell flake, a\n" +
			"correctness test for the passed-through card, run-agent, and under\n" +
			"guest/ an optional guest image flake for what the project needs the\n" +
			"machine itself to have.\n\n" +
			"guest/flake.nix takes its rig base from the build of rig that wrote it:\n" +
			"a release tag on GitHub for a release build, or a copy of the embedded\n" +
			"base under your cache directory for a development build. Change it if\n" +
			"the project should track a different rig.\n\n" +
			"Then:\n" +
			"  rig new <vm> --env secrets/<vm>.env --start\n" +
			"  rig push <vm> <dir>\n" +
			"  rig exec --dir /work/<dir> <vm> nix develop \"path:.\" -c make run",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			dir := args[0]
			if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 && !force {
				return fmt.Errorf("%s is not empty; refusing to write over it (--force writes anyway)", dir)
			}
			base, err := baseFlakeRef()
			if err != nil {
				return err
			}
			// The guest lock is regenerated on the first build against whatever
			// the rig input resolves to; the one in the checkout pins a relative
			// path that means nothing outside it.
			skip := func(p string) bool { return p == "guest/flake.lock" }
			if err := rig.WriteTree(rig.ProjectTemplate(), dir, skip); err != nil {
				return err
			}
			gf := filepath.Join(dir, "guest", "flake.nix")
			b, err := os.ReadFile(gf)
			if err != nil {
				return err
			}
			updated, ok := retargetRigInput(string(b), base)
			if !ok {
				return fmt.Errorf("no inputs.rig.url line in %s to point at the base", gf)
			}
			if err := os.WriteFile(gf, []byte(updated), 0o644); err != nil {
				return err
			}
			note("wrote the project template to %s", dir)
			note("guest/flake.nix takes its rig base from %s", base)
			note("next:  rig new <vm> --env secrets/<vm>.env --start && rig push <vm> %s", dir)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "write into a non-empty directory, overwriting files of the same name")
	return cmd
}

// rigInputRE matches the one line in the template's guest flake that names
// the rig base, whatever it currently points at.
var rigInputRE = regexp.MustCompile(`(?m)^(\s*inputs\.rig\.url\s*=\s*)"[^"]*"(\s*;)`)

// retargetRigInput points the guest flake's rig input at ref.
func retargetRigInput(flake, ref string) (string, bool) {
	if !rigInputRE.MatchString(flake) {
		return flake, false
	}
	return rigInputRE.ReplaceAllString(flake, `${1}"`+ref+`"${2}`), true
}

// baseFlakeRef names the base a project written by this binary should build
// against. A release build names its tag on GitHub, which is portable and
// pinned. Anything else — a `make` in a checkout, a pseudo-version — names a
// copy of the embedded base written to the cache directory, because a commit
// that may never have been pushed cannot be fetched.
func baseFlakeRef() (string, error) {
	if ref, ok := releaseBaseRef(readBuild()); ok {
		return ref, nil
	}
	dir, err := materializeBase()
	if err != nil {
		return "", err
	}
	return "path:" + dir, nil
}

var semverTagRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// releaseBaseRef is the GitHub flake ref for a release build, and false for
// anything that is not one: a pseudo-version is a commit, not a tag, and a
// module not hosted on GitHub has no such ref.
func releaseBaseRef(b build) (string, bool) {
	repo, ok := strings.CutPrefix(b.module, "github.com/")
	if !ok || !semverTagRE.MatchString(b.version) {
		return "", false
	}
	return "github:" + repo + "/" + b.version + "?dir=base", true
}

// materializeBase writes the embedded base to the cache directory and returns
// the path. Keyed by the build, so two rigs on one machine do not overwrite
// each other's, and rewritten every time, because it is derived from the
// binary and nothing else.
func materializeBase() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "rig", "base-"+buildKey(readBuild()))
	if err := rig.WriteTree(rig.Base(), dir, nil); err != nil {
		return "", err
	}
	return dir, nil
}

// buildKey is the build rendered as a directory name: the commit when there is
// one, else the version, with nothing a path would object to.
func buildKey(b build) string {
	key := b.version
	if b.rev != "" {
		key = b.rev
		if b.dirty {
			key += "-dirty"
		}
	}
	if key == "" {
		key = "unknown"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			return r
		}
		return '_'
	}, key)
}
