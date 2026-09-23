// Package rig carries the files a rig binary needs that are not code: the base
// image definition and the project template. They are embedded so a rig that
// was `go install`ed — no checkout on disk — can still build the image and
// start a project, and so the template a binary writes out is the one that
// binary's `rig image build` understands.
package rig

import (
	"bytes"
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

// assets holds base/ and project-template/. The patterns are explicit rather
// than `all:` so a build product left in a checkout — project-template's
// compiled vectoradd, a nix `result` link — can never be embedded by accident.
// A test holds this list to what git tracks.
//
//go:embed base/flake.nix base/flake.lock base/gpu-dev.nix base/desktop.nix
//go:embed base/rig-input/main.go
//go:embed project-template/flake.nix project-template/flake.lock
//go:embed project-template/Makefile project-template/README.md
//go:embed project-template/run-agent project-template/vectoradd.cu
//go:embed project-template/.gitignore project-template/rig.yaml
//go:embed project-template/guest/flake.nix project-template/guest/flake.lock
//go:embed project-template/guest/guest.nix
var assets embed.FS

// Base is the base image definition, rooted at its flake.
func Base() fs.FS { return sub("base") }

// ProjectTemplate is the per-project template, rooted at its flake.
func ProjectTemplate() fs.FS { return sub("project-template") }

func sub(dir string) fs.FS {
	f, err := fs.Sub(assets, dir)
	if err != nil {
		panic(err) // both directories are embedded above; this cannot fail
	}
	return f
}

// WriteTree copies an embedded tree to dst, creating it. skip, if given, is
// asked about every path and may drop a file or a whole directory. A file that
// begins with "#!" is written executable, since an embed.FS keeps no modes.
func WriteTree(src fs.FS, dst string, skip func(path string) bool) error {
	return fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if skip != nil && skip(p) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if bytes.HasPrefix(b, []byte("#!")) {
			mode = 0o755
		}
		return os.WriteFile(target, b, mode)
	})
}
