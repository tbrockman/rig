package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	rig "github.com/tbrockman/rig"
	"github.com/tbrockman/rig/internal/devices"
	"github.com/tbrockman/rig/internal/hostdev"
	"github.com/tbrockman/rig/internal/manifest"
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
	var with []string
	var image string
	cmd := &cobra.Command{
		Use:     "init <dir>",
		GroupID: "vm",
		Short:   "Write a rig.yaml and a guest image flake into a directory",
		Long: "Writes a starting point into <dir>: a rig.yaml that grants nothing\n" +
			"yet, with this host's NVIDIA card as a commented example when there is\n" +
			"one, and under guest/ the flake the VM's image is built from.\n\n" +
			"--with adds rig's optional guest modules: nvidia (the driver; also grants\n" +
			"the card in rig.yaml), docker, desktop (an X11 session on the card; implies\n" +
			"nvidia, and lends the host's keyboard and mouse). --image names an image\n" +
			"already built instead, and writes no guest/.\n\n" +
			"guest/flake.nix takes its rig base from the build of rig that wrote it:\n" +
			"a release tag on GitHub for a release build, or a copy of the embedded\n" +
			"base under your cache directory for a development build.\n\n" +
			"Then:\n" +
			"  rig apply -f <dir>/rig.yaml --start",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			dir := args[0]
			mods, err := parseWith(with)
			if err != nil {
				return err
			}
			if image != "" && len(mods) > 0 {
				return errors.New("--with adds modules to guest/flake.nix, which --image leaves out; choose one")
			}
			if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 && !force {
				return fmt.Errorf("%s is not empty; refusing to write over it (--force writes anyway)", dir)
			}
			skip := func(p string) bool { return image != "" && (p == "guest" || p == ".gitignore") }
			if err := rig.WriteTree(rig.ProjectTemplate(), dir, skip); err != nil {
				return err
			}
			var base string
			if image == "" {
				if base, err = baseFlakeRef(); err != nil {
					return err
				}
				if err := writeGuest(dir, base, mods); err != nil {
					return err
				}
			}

			// The manifest names the project, and shows this host's card: found
			// the way `kind: gpu` without an address finds it, and granted only
			// when nvidia was asked for.
			name := projectName(dir)
			pci, err := devices.DiscoverPCI(a.c, a.cfg)
			id := hostdev.IDs(pci)
			if err != nil {
				pci, id = examplePCI, exampleID
			}
			mf := filepath.Join(dir, manifest.DefaultFile)
			b, err := os.ReadFile(mf)
			if err != nil {
				return err
			}
			text := fillManifest(string(b), name, pci, id)
			if ref, err := schemaRef(); err == nil {
				text = pointAtSchema(text, ref)
			} else {
				note("WARNING: could not write the schema for editors: %v", err)
			}
			if mods["nvidia"] {
				text = grantCard(text)
			}
			if mods["desktop"] {
				text = uncommentLine(text, "input: host")
			}
			if image != "" {
				text = useImage(text, image)
			}
			if _, err := manifest.Parse([]byte(text)); err != nil {
				return fmt.Errorf("the rig.yaml init wrote does not parse; this is a bug in rig: %w", err)
			}
			if err := os.WriteFile(mf, []byte(text), 0o644); err != nil {
				return err
			}

			note("wrote %s", mf)
			if image == "" {
				note("guest/flake.nix builds on %s%s", base, moduleNote(mods))
			} else {
				note("the VM is made from the image %s", image)
			}
			switch {
			case mods["nvidia"] && pci == examplePCI:
				note("WARNING: no NVIDIA card found; rig.yaml grants a made-up one. Put yours in (lspci -Dnn).")
			case mods["nvidia"]:
				note("rig.yaml grants the card at %s", pci)
			case pci != examplePCI:
				note("the card at %s is in rig.yaml, commented out", pci)
			}
			note("next:  rig apply -f %s --start", mf)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "write into a non-empty directory, overwriting files of the same name")
	cmd.Flags().StringSliceVar(&with, "with", nil, "optional guest modules: nvidia, docker, desktop")
	cmd.Flags().StringVar(&image, "image", "", "use this image alias instead of writing a guest flake")
	return cmd
}

// guestModules are rig's optional modules, as `rig init --with` names them.
var guestModules = []string{"nvidia", "docker", "desktop"}

// parseWith reads --with. desktop brings nvidia, since the session runs on
// the card.
func parseWith(with []string) (map[string]bool, error) {
	mods := map[string]bool{}
	for _, w := range with {
		w = strings.TrimSpace(w)
		if !slices.Contains(guestModules, w) {
			return nil, fmt.Errorf("--with %q: the modules are %s", w, strings.Join(guestModules, ", "))
		}
		mods[w] = true
	}
	if mods["desktop"] {
		mods["nvidia"] = true
	}
	return mods, nil
}

// writeGuest points guest/flake.nix at base and adds the chosen modules; with
// desktop, guest.nix gets the session's user.
func writeGuest(dir, base string, mods map[string]bool) error {
	gf := filepath.Join(dir, "guest", "flake.nix")
	b, err := os.ReadFile(gf)
	if err != nil {
		return err
	}
	flake, ok := retargetRigInput(string(b), base)
	if !ok {
		return fmt.Errorf("no inputs.rig.url line in %s to point at the base", gf)
	}
	if flake, ok = addModules(flake, mods); !ok {
		return fmt.Errorf("no mkGuest line in %s to add modules to", gf)
	}
	if err := os.WriteFile(gf, []byte(flake), 0o644); err != nil {
		return err
	}
	if !mods["desktop"] {
		return nil
	}
	gn := filepath.Join(dir, "guest", "guest.nix")
	b, err = os.ReadFile(gn)
	if err != nil {
		return err
	}
	user := os.Getenv("USER")
	if user == "" || user == "root" {
		user = "me"
	}
	text := strings.Replace(string(b), `  # rig.desktop.user = "me";`, `  rig.desktop.user = "`+user+`";`, 1)
	return os.WriteFile(gn, []byte(text), 0o644)
}

const mkGuestLine = "rig.lib.mkGuest [ ./guest.nix ]"

// addModules puts the chosen modules into the template's mkGuest list.
// desktop already imports nvidia, so it is not listed twice.
func addModules(flake string, mods map[string]bool) (string, bool) {
	if !strings.Contains(flake, mkGuestLine) {
		return flake, false
	}
	var list []string
	for _, m := range guestModules {
		if mods[m] && !(m == "nvidia" && mods["desktop"]) {
			list = append(list, "rig.nixosModules."+m)
		}
	}
	list = append(list, "./guest.nix")
	return strings.Replace(flake, mkGuestLine, "rig.lib.mkGuest [ "+strings.Join(list, " ")+" ]", 1), true
}

func moduleNote(mods map[string]bool) string {
	var on []string
	for _, m := range guestModules {
		if mods[m] {
			on = append(on, m)
		}
	}
	if len(on) == 0 {
		return ""
	}
	return ", with " + strings.Join(on, ", ")
}

// grantCard un-comments the template's gpu device and gives it to the VM.
func grantCard(text string) string {
	lines := strings.Split(text, "\n")
	in := false
	for i, l := range lines {
		switch {
		case l == "    # gpu:":
			in = true
		case in && !strings.HasPrefix(l, "    #   "):
			in = false
		}
		if in {
			lines[i] = "    " + strings.TrimPrefix(l, "    # ")
		}
	}
	return uncommentLine(strings.Join(lines, "\n"), "devices: [gpu]")
}

// uncommentLine turns the template's "  # <prefix>..." into "  <prefix>...".
func uncommentLine(text, prefix string) string {
	return strings.Replace(text, "  # "+prefix, "  "+prefix, 1)
}

var flakeLineRE = regexp.MustCompile(`(?m)^  flake: .*$`)

// useImage replaces the template's flake: line with image: alias.
func useImage(text, alias string) string {
	return flakeLineRE.ReplaceAllString(text, "  image: "+alias)
}

// projectName is the instance name a directory suggests: its basename, with
// anything Incus would reject turned into a dash.
func projectName(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		}
		return '-'
	}, filepath.Base(abs))
	name = strings.Trim(name, "-")
	if name == "" || !nameRE.MatchString(name) {
		return "myproj"
	}
	return name
}

// The card rig.yaml shows when this host has none to show: made up, so
// nobody's hardware is in a file rig writes.
const (
	examplePCI = "0000:2b:00.0"
	exampleID  = "10de:abcd"
)

// fillManifest replaces the template's placeholders. A card whose identity
// could not be read gets no id line: the field is optional for a gpu, and a
// made-up one would only fail the first start.
func fillManifest(text, name, pci, id string) string {
	text = strings.ReplaceAll(text, "PROJECT", name)
	text = strings.ReplaceAll(text, "GPU_PCI", pci)
	if id == "" {
		var keep []string
		for _, line := range strings.Split(text, "\n") {
			if !strings.Contains(line, "GPU_ID") {
				keep = append(keep, line)
			}
		}
		text = strings.Join(keep, "\n")
	}
	return strings.ReplaceAll(text, "GPU_ID", id)
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

func (a *app) schemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "schema",
		GroupID: "vm",
		Short:   "Print the JSON Schema for rig.yaml",
		Long: "Prints the JSON Schema rig.yaml is checked against, for an editor or\n" +
			"another tool. rig init already points a new rig.yaml at it, with a\n" +
			"yaml-language-server comment that VS Code's YAML extension reads.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_, err := os.Stdout.Write(rig.Schema())
			return err
		},
	}
}

// schemaLineRE matches the template's yaml-language-server comment.
var schemaLineRE = regexp.MustCompile(`(?m)^# yaml-language-server: \$schema=\S+$`)

// pointAtSchema points a manifest's yaml-language-server comment at ref.
func pointAtSchema(text, ref string) string {
	return schemaLineRE.ReplaceAllString(text, "# yaml-language-server: $$schema="+ref)
}

// schemaRef is the schema a manifest written by this binary should name: the
// release's own on GitHub for a release build, else a copy of the embedded
// one in the cache directory, since a development build's schema may not be
// published anywhere.
func schemaRef() (string, error) {
	b := readBuild()
	if repo, ok := strings.CutPrefix(b.module, "github.com/"); ok && semverTagRE.MatchString(b.version) {
		return "https://raw.githubusercontent.com/" + repo + "/" + b.version + "/schema/rig.schema.json", nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(cache, "rig", "rig.schema-"+buildKey(b)+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, rig.Schema(), 0o644)
}
