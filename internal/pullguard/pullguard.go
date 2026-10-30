// Package pullguard stops `rig pull` from destroying work done on the host.
//
// It is pushguard turned around. Pulling a guest directory again to see what
// an agent has done since is the normal workflow, so overwriting is what pull
// is for, and requiring --force for it would see --force passed every time.
// The loss to prevent is a host file someone edited after rig wrote it. So rig
// marks each file it writes with the hash of what it wrote, and refuses only
// when a file no longer matches its mark, or has none and differs from the
// guest's copy.
//
// The mark is an extended attribute on the file rather than a record kept
// elsewhere: it describes that file, follows it when the directory moves, and
// dies with it. An editor that saves by replacing the file drops the mark,
// which makes the guard conservative, never wrong.
package pullguard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// attr is in the user namespace, the one an unprivileged process may write.
const attr = "user.rig.pulled"

// Conflict is one host path that pull would have destroyed.
type Conflict struct {
	HostPath string
	Why      string
}

// Check reports whether writing a file to hostPath destroys anything. guest
// writes the guest's copy, and is called only when the host file has no valid
// mark, so the common cases cost no transfer.
func Check(hostPath string, guest func(io.Writer) error) (*Conflict, error) {
	st, err := os.Lstat(hostPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return &Conflict{hostPath, "is a " + kind(st.Mode()) + " on the host"}, nil
	}
	have, err := hashFile(hostPath)
	if err != nil {
		return nil, err
	}
	mark := readMark(hostPath)
	if mark == have {
		return nil, nil
	}
	h := sha256.New()
	if err := guest(h); err != nil {
		return nil, err
	}
	want := hex.EncodeToString(h.Sum(nil))
	switch {
	case want == have:
		return nil, nil // writing it changes nothing
	case mark == "":
		return &Conflict{hostPath, "exists on the host and rig did not write it"}, nil
	default:
		return &Conflict{hostPath, "changed on the host since rig wrote it"}, nil
	}
}

// Current reports whether hostPath is already the guest's file: rig wrote it,
// nothing changed it since, and it has the guest's size and modification time.
// Then there is nothing to transfer. rsync's quick check, made safe by the mark.
func Current(hostPath string, size int64, modified time.Time) bool {
	st, err := os.Lstat(hostPath)
	if err != nil || !st.Mode().IsRegular() || st.Size() != size || !st.ModTime().Equal(modified) {
		return false
	}
	have, err := hashFile(hostPath)
	return err == nil && have == readMark(hostPath)
}

// CheckDir reports whether a directory can be made at hostPath.
func CheckDir(hostPath string) *Conflict {
	st, err := os.Lstat(hostPath)
	if err != nil || st.IsDir() {
		return nil
	}
	return &Conflict{hostPath, "is a " + kind(st.Mode()) + " on the host, where the guest has a directory"}
}

// Write replaces hostPath with what fill writes, and marks it. The content goes
// to a temporary file beside it first, so an interrupted pull leaves the old
// file whole rather than half of the new one.
func Write(hostPath string, mode fs.FileMode, modified time.Time, fill func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(hostPath), "."+filepath.Base(hostPath)+".rig-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	h := sha256.New()
	if err := fill(io.MultiWriter(tmp, h)); err != nil {
		tmp.Close()
		return err
	}
	// Marked before the guest's mode applies: a user xattr needs write
	// permission, and git's objects, for one, are read-only. A filesystem
	// without user xattrs leaves the file unmarked, which the next pull treats
	// as a file rig did not write: stricter, not wrong.
	_ = syscall.Setxattr(tmp.Name(), attr, []byte(hex.EncodeToString(h.Sum(nil))), 0)
	if err := tmp.Chmod(mode.Perm()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !modified.IsZero() {
		_ = os.Chtimes(tmp.Name(), modified, modified)
	}
	return os.Rename(tmp.Name(), hostPath)
}

// Error renders conflicts as the message the operator sees, naming every file:
// whether to force is a per-file judgement a count cannot inform.
func Error(conflicts []Conflict, retry string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to overwrite %d file(s) on the host:\n", len(conflicts))
	for _, c := range conflicts {
		fmt.Fprintf(&b, "  %s\n      %s\n", c.HostPath, c.Why)
	}
	b.WriteString("\nLook at them first; then pull somewhere else, or overwrite deliberately:\n")
	fmt.Fprintf(&b, "  %s", retry)
	return errors.New(b.String())
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readMark(p string) string {
	buf := make([]byte, 128)
	n, err := syscall.Getxattr(p, attr, buf)
	if err != nil {
		return ""
	}
	return string(buf[:n])
}

func kind(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "directory"
	case m&fs.ModeSymlink != 0:
		return "symlink"
	}
	return "special file"
}
