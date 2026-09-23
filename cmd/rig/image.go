package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/incus"
)

// Properties rig stamps on an image it built.
//
// sourceProp is the nix store path of the disk derivation — a content hash over
// every input that went into it. It is what makes an image traceable back to
// the tree that produced it, which a bare Incus fingerprint is not.
const (
	sourceProp = "rig.source"
	builtProp  = "rig.built"
	flakeProp  = "rig.flake"
)

func (a *app) imageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "image",
		GroupID: "vm",
		Short:   "Build and inspect the base image VMs are made from",
	}
	cmd.AddCommand(a.imageBuildCmd(), a.imageListCmd())
	return cmd
}

func (a *app) imageBuildCmd() *cobra.Command {
	var flake, attr, alias string
	var keepPrevious bool

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build the base image from its flake and import it",
		Long: "Builds the image declaratively and imports it. The image is a build\n" +
			"artifact, not a snapshot of a VM someone logged into, so rebuilding\n" +
			"from the same flake produces the same image — and rebuilding when\n" +
			"nothing changed does nothing.\n\n" +
			"The alias is retargeted only after the import succeeds, so a failed\n" +
			"build never leaves this host without a base image.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// RIG_FLAKE counts as naming it: the operator set it deliberately,
			// and envOr has already folded it into the default.
			named := cmd.Flags().Changed("flake") || os.Getenv("RIG_FLAKE") != ""
			if !named {
				if _, err := os.Stat(filepath.Join(flake, "flake.nix")); err != nil {
					// No checkout here — a `go install`ed rig, most likely.
					// Build the base this binary carries, and say so: the
					// operator should know which definition became the image.
					dir, err := materializeBase()
					if err != nil {
						return err
					}
					note("no %s here; building the base image built into this rig, from %s", flake, dir)
					flake, named = dir, true
				}
			}
			return a.buildImage(flake, named, attr, alias, keepPrevious)
		},
	}
	f := cmd.Flags()
	f.StringVar(&flake, "flake", envOr("RIG_FLAKE", "./base"), "directory or flake ref holding the image definition (a relative path resolves against your current directory)")
	f.StringVar(&attr, "attr", envOr("RIG_FLAKE_ATTR", "gpubase"), "nixosConfigurations attribute to build")
	f.StringVar(&alias, "alias", envOr("RIG_IMAGE", defaultImage), "alias to point at the result")
	f.BoolVar(&keepPrevious, "keep-previous", false, "keep the image the alias pointed at before (each is gigabytes)")
	return cmd
}

// buildImage builds nixosConfigurations.<attr> of a flake, imports the result
// and points alias at it. Shared by `rig image build` and a manifest's
// `build:`, so the two cannot drift apart in what they stamp on an image.
func (a *app) buildImage(flake string, named bool, attr, alias string, keepPrevious bool) error {
	ref, err := flakeRef(flake, named)
	if err != nil {
		return err
	}
	base := fmt.Sprintf("%s#nixosConfigurations.%s.config.system.build.", ref, attr)

	note("building the disk (slow the first time)")
	diskPath, err := nixBuild(base + "qemuImage")
	if err != nil {
		return err
	}
	note("building the metadata tarball")
	metaPath, err := nixBuild(base + "metadata")
	if err != nil {
		return err
	}

	disk, err := findUnder(diskPath, ".qcow2")
	if err != nil {
		return err
	}
	meta, err := findUnder(metaPath, ".tar.xz")
	if err != nil {
		return err
	}

	stamp := map[string]string{
		sourceProp: diskPath,
		builtProp:  time.Now().UTC().Format(time.RFC3339),
		flakeProp:  ref + "#" + attr,
	}

	// Incus fingerprints a split image as sha256(metadata || disk), so
	// the answer to "is this already here" is knowable without uploading
	// a gigabyte and asking. A build that changed nothing stops here.
	fingerprint, err := incus.SplitImageFingerprint(meta, disk)
	if err != nil {
		return err
	}
	previous, _ := a.c.AliasTarget(alias)

	if existing, err := a.c.Image(fingerprint); err == nil {
		if previous == fingerprint {
			note("%s is already this build (%s)", alias, existing.Short())
			return a.c.SetImageProperties(fingerprint, stamp)
		}
		note("this build is already imported as %s; pointing %s at it",
			existing.Short(), alias)
	} else {
		note("importing %s (%s)", filepath.Base(disk), byteSize(fileSize(disk)))
		fingerprint, err = a.c.ImportImage(incus.ImportOpts{
			MetadataPath: meta,
			RootfsPath:   disk,
			Filename:     filepath.Base(disk),
			Properties:   stamp,
		})
		if err != nil {
			return err
		}
		note("imported %s", fingerprint[:12])
	}
	if err := a.c.SetImageProperties(fingerprint, stamp); err != nil {
		return err
	}

	if err := a.c.SetAlias(alias, fingerprint, "rig base image"); err != nil {
		return fmt.Errorf("imported %s but could not point %q at it: %w",
			fingerprint[:12], alias, err)
	}
	note("%s -> %s", alias, fingerprint[:12])

	if previous != "" && previous != fingerprint {
		if keepPrevious {
			note("previous image %s kept", previous[:12])
		} else if err := a.c.DeleteImage(previous); err != nil {
			note("could not delete the previous image %s: %v", previous[:12], err)
		} else {
			note("deleted the previous image %s", previous[:12])
		}
	}

	note("VMs created from now on use this image; existing ones keep theirs")
	note("check with:  rig doctor <vm>")
	return nil
}

func (a *app) imageListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Images on this host, and which flake built them",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			images, err := a.c.Images()
			if err != nil {
				return err
			}
			if len(images) == 0 {
				fmt.Println("no images. Build one with `rig image build`.")
				return nil
			}
			fmt.Printf("  %-14s %-24s %-10s %s\n", "FINGERPRINT", "ALIAS", "SIZE", "BUILT")
			for i := range images {
				img := &images[i]
				var aliases []string
				for _, al := range img.Aliases {
					aliases = append(aliases, al.Name)
				}
				built := img.Properties[builtProp]
				if built == "" {
					built = "not built by rig (" + img.UploadedAt.Format(time.DateOnly) + ")"
				}
				fmt.Printf("  %-14s %-24s %-10s %s\n", img.Short(),
					firstNonEmpty(strings.Join(aliases, ","), "-"), byteSize(img.Size), built)
			}
			return nil
		},
	}
}

// imageDrift reports whether an instance was created from something other than
// what its alias points at now. The instance records its origin itself —
// volatile.base_image — so nothing new has to be stored to answer this.
func (a *app) imageDrift(inst *incus.Instance, alias string) (drifted bool, detail string) {
	from := inst.Config["volatile.base_image"]
	if from == "" {
		return false, "unknown: instance records no base image"
	}
	current, err := a.c.AliasTarget(alias)
	if err != nil {
		return false, from[:12] + " (no " + alias + " alias to compare against)"
	}
	if from == current {
		return false, from[:12] + ", current"
	}
	detail = from[:12] + " but " + alias + " is now " + current[:12]
	if img, err := a.c.Image(current); err == nil && img.Properties[builtProp] != "" {
		detail += " (built " + img.Properties[builtProp] + ")"
	}
	return true, detail
}

// --- nix -----------------------------------------------------------------

// flakeRef turns a directory into a flake ref, and passes anything that
// already looks like a ref straight through.
//
// named says the operator chose this directory, by --flake or RIG_FLAKE. When
// they did not, the value is the built-in "./base", which resolves against
// whatever directory rig happens to be run from — so `rig image build` from
// anywhere but the rig checkout failed with "no flake.nix in /somewhere/base",
// naming a path the operator never typed. The default is the assumption worth
// reporting, not the path it produced.
//
// Inside a git working tree the bare path is the ref, not `path:`. Nix then
// reads the directory through git, which honours .gitignore and — the reason
// this matters — resolves a relative input such as `path:../../base` against
// the tree. A `path:` ref copies the directory into the store first, and a
// relative input is then resolved against the store copy, where `../../base`
// is nothing. The cost is that untracked files are invisible to nix, so they
// are named here rather than discovered as a missing-file error from inside
// the build.
func flakeRef(in string, named bool) (string, error) {
	if strings.Contains(in, ":") {
		return in, nil
	}
	abs, err := filepath.Abs(in)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(abs, "flake.nix")); err != nil {
		if !named {
			cwd, _ := os.Getwd()
			return "", fmt.Errorf("no flake.nix in %s.\n"+
				"  --flake was not given, so it defaulted to %q, resolved against\n"+
				"  your current directory (%s).\n"+
				"  Name the image definition:  rig image build --flake /path/to/base\n"+
				"  or set RIG_FLAKE.", abs, in, cwd)
		}
		return "", fmt.Errorf("no flake.nix in %s\n  Point --flake at the directory holding the image definition.", abs)
	}
	if !inGitTree(abs) {
		return "path:" + abs, nil
	}
	if untracked := untrackedFiles(abs); len(untracked) > 0 {
		note("WARNING: nix reads a git tree through git, so it will not see these untracked files:")
		for _, f := range untracked {
			note("         %s", f)
		}
		note("         git add them first if the flake needs them.")
	}
	return abs, nil
}

func inGitTree(dir string) bool {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func untrackedFiles(dir string) []string {
	out, err := exec.Command("git", "-C", dir, "ls-files", "--others", "--exclude-standard", ".").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// nixBuild builds one installable and returns its store path. Nix's progress
// goes to stderr, where it belongs — this can take an hour on a cold cache.
// It is also kept, because one failure in it has a known cause and a known
// fix that nix's own message does not name.
func nixBuild(installable string) (string, error) {
	cmd := exec.Command("nix", "build", "--no-link", "--print-out-paths", installable)
	var stderr strings.Builder
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	out, err := cmd.Output()
	if err != nil {
		if input, ok := staleLock(stderr.String()); ok {
			return "", fmt.Errorf("nix build %s: %w\n\n%s", installable, err, staleLockHint(installable, input))
		}
		return "", fmt.Errorf("nix build %s: %w", installable, err)
	}
	paths := strings.Fields(string(out))
	if len(paths) == 0 {
		return "", fmt.Errorf("nix build %s produced no output path", installable)
	}
	return paths[0], nil
}

// staleLockRE matches nix refusing a flake input whose contents no longer
// hash to what the lock file recorded.
var staleLockRE = regexp.MustCompile(`NAR hash mismatch in input '([^']+)'`)

// staleLock reports whether a build failed because a flake.lock pins an input
// that has since changed, and which input.
func staleLock(stderr string) (input string, ok bool) {
	m := staleLockRE.FindStringSubmatch(stderr)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// staleLockHint says what a NAR hash mismatch means here and what to do.
//
// A project's guest flake takes its rig base from a path input, and for a
// development build that path is a copy of the embedded base under the cache
// directory, rewritten whenever a differently built rig runs. The guest's
// flake.lock pinned the old contents by hash, so the next image build fails
// in nix's words rather than these. The fix is to let the lock regenerate.
func staleLockHint(installable, input string) string {
	dir := installable
	if i := strings.Index(dir, "#"); i >= 0 {
		dir = dir[:i]
	}
	dir = strings.TrimPrefix(dir, "path:")
	lock := filepath.Join(dir, "flake.lock")
	return fmt.Sprintf("The flake's lock file pins %s\n"+
		"to contents that have since changed — for a development build of rig, that is\n"+
		"the copy of the base it wrote under your cache directory, rewritten by a rebuild.\n"+
		"Let the lock regenerate against what is there now:\n"+
		"  rm %s\n"+
		"and run this build again. Nothing else in the flake is affected.",
		shortInput(input), lock)
}

// shortInput drops the query string nix appends to a path input, which is the
// very hash being complained about and says nothing to a reader.
func shortInput(input string) string {
	if i := strings.Index(input, "?"); i >= 0 {
		return input[:i]
	}
	return input
}

// findUnder locates the one file with a given suffix in a store path, so a
// change in the image module's output layout does not need a code change here.
func findUnder(root, suffix string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return err
		}
		if strings.HasSuffix(p, suffix) {
			found = p
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("no %s file under %s", suffix, root)
	}
	return found, nil
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
