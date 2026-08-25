// Package pushguard stops `rig push` from destroying work done inside a guest.
//
// The failure it exists to prevent is a lost update, and it is silent: the
// operator edits a staging directory on the host, pushes it, and overwrites a
// file the guest changed in the meantime. Nothing errors. The guest's version
// is simply gone, and it is gone from the only copy — /work lives inside the
// instance.
//
// A blanket "refuse to overwrite" would be useless here, because re-pushing an
// edited staging directory is the normal workflow and overwrites are what it is
// for. The distinction that matters is not "does this file exist" but "did
// anyone but rig touch it since rig wrote it". So rig records what it wrote,
// and refuses only when the guest's copy no longer matches that record.
//
// The record lives in the guest, at ManifestPath, because it is derived state:
// it describes that instance, it is worthless without it, and it should die
// with it. Nothing is kept on the host.
package pushguard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestPath is in /var/lib rather than /run: it has to survive a reboot,
// because the hazard it guards against spans the whole life of the instance.
const ManifestPath = "/var/lib/rig/push.json"

// Client is the slice of the Incus client this package needs. An interface so
// the guard is testable without a daemon.
type Client interface {
	Pull(name, guestPath string) ([]byte, error)
	WriteFile(name, guestPath string, content []byte, mode fs.FileMode) error
	Mkdir(name, guestPath string, mode fs.FileMode) error
}

// Conflict is one file that push would have destroyed.
type Conflict struct {
	GuestPath string
	Why       string
}

// Manifest maps a guest path to the hash of the content rig last wrote there.
type Manifest map[string]string

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Load reads the manifest. A missing or unparseable manifest is an empty one,
// not an error: the first push to an instance has nothing to compare against,
// and a corrupt manifest should make the guard conservative rather than break
// the command. Both cases make every existing file look unrecorded, which
// Check reports as a conflict.
func Load(c Client, instance string) Manifest {
	b, err := c.Pull(instance, ManifestPath)
	if err != nil {
		return Manifest{}
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return Manifest{}
	}
	return m
}

// Files lists the regular files under hostDir, relative to it, in the same way
// PushDir selects them. Shared so the guard can never check a different set of
// files than the push writes.
func Files(hostDir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(hostDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(hostDir, p)
		if err != nil || rel == "." {
			return err
		}
		if d.Type().IsRegular() {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// ReadAll reads the files Push will write, keyed by their path relative to
// hostDir. Read once and reused for the manifest so the recorded hash is of the
// bytes that were actually sent, not of whatever the file says a moment later.
func ReadAll(hostDir string) (map[string][]byte, error) {
	rels, err := Files(hostDir)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(rels))
	for _, rel := range rels {
		b, err := os.ReadFile(filepath.Join(hostDir, rel))
		if err != nil {
			return nil, err
		}
		out[rel] = b
	}
	return out, nil
}

// Check reports every file the push would overwrite that rig did not itself
// last write. An empty result means the push destroys nothing.
func Check(c Client, instance, hostDir, guestDir string) ([]Conflict, error) {
	rels, err := Files(hostDir)
	if err != nil {
		return nil, err
	}
	m := Load(c, instance)

	var conflicts []Conflict
	for _, rel := range rels {
		gp := path.Join(guestDir, rel)
		existing, err := c.Pull(instance, gp)
		if err != nil {
			continue // not there; writing it destroys nothing
		}
		recorded, ok := m[gp]
		switch {
		case !ok:
			conflicts = append(conflicts, Conflict{gp,
				"exists in the guest but rig has no record of writing it"})
		case recorded != hash(existing):
			conflicts = append(conflicts, Conflict{gp,
				"changed inside the guest since rig last wrote it"})
		}
	}
	return conflicts, nil
}

// Record stores the hashes of what was just pushed, so a later push can tell
// its own writes from someone else's. Called only after a push succeeds.
func Record(c Client, instance, hostDir, guestDir string, contents map[string][]byte) error {
	m := Load(c, instance)
	for rel, b := range contents {
		m[path.Join(guestDir, rel)] = hash(b)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := c.Mkdir(instance, path.Dir(ManifestPath), 0o755); err != nil {
		return err
	}
	return c.WriteFile(instance, ManifestPath, b, 0o644)
}

// Error renders conflicts as the message the operator sees. It names every file
// rather than a count, because the decision to force is per-file judgement and
// a count cannot inform it.
func Error(conflicts []Conflict, instance, guestDir string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to overwrite %d file(s) in %s:%s that rig did not write:\n",
		len(conflicts), instance, guestDir)
	for _, c := range conflicts {
		fmt.Fprintf(&b, "  %s\n      %s\n", c.GuestPath, c.Why)
	}
	b.WriteString("\nRead them before you decide — /work exists only inside the instance,\n")
	b.WriteString("so the guest's copy may be the only one.\n")
	fmt.Fprintf(&b, "  rig pull %s %s\n      # look first\n", instance, conflicts[0].GuestPath)
	b.WriteString("  rig push --force <name> <src-dir>\n      # then overwrite deliberately")
	return fmt.Errorf("%s", b.String())
}
