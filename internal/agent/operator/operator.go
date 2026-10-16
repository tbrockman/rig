// Package operator is the host's half of the channel to an agent: each VM's
// signing key, its sequence counter, and the last brief delivered to it. It
// lives in the user's config directory and never enters a guest; the guest
// build of rig does not import this package, and a test holds it to that.
package operator

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tbrockman/rig/internal/agent"
)

// Store is one VM's directory: $XDG_CONFIG_HOME/rig/agent/<vm>, else
// ~/.config/rig/agent/<vm>.
type Store struct{ Dir string }

// For is the store for vm.
func For(vm string) (Store, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return Store{}, err
	}
	return Store{Dir: filepath.Join(base, "rig", "agent", vm)}, nil
}

func (s Store) keyPath() string   { return filepath.Join(s.Dir, "operator.key") }
func (s Store) seqPath() string   { return filepath.Join(s.Dir, "seq") }
func (s Store) briefPath() string { return filepath.Join(s.Dir, "brief") }

// KeyPath is where the private key is, for messages that name it.
func (s Store) KeyPath() string { return s.keyPath() }

// Key loads the VM's private key. It does not make one: a key made by send
// would not be the one the guest was given at start, and everything it signed
// would be refused.
func (s Store) Key() (ed25519.PrivateKey, error) {
	path := s.keyPath()
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %04o, readable by other users; it signs for the operator:  chmod 600 %s",
			path, perm, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s: not a PEM private key", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ed, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ed25519 key", path)
	}
	return ed, nil
}

// KeyOrCreate loads the VM's key, making it on first use (PKCS#8 PEM, mode
// 0600, in a directory of mode 0700). made says it was new.
func (s Store) KeyOrCreate() (key ed25519.PrivateKey, made bool, err error) {
	if k, err := s.Key(); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return k, false, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, false, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, false, err
	}
	// O_EXCL: two starts racing must not each write a key and leave the
	// guest holding the loser's public half.
	f, err := os.OpenFile(s.keyPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		k, err := s.Key()
		return k, false, err
	}
	if err != nil {
		return nil, false, err
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		f.Close()
		return nil, false, err
	}
	return priv, true, f.Close()
}

// NextSeq takes the next sequence number for this VM. Under a lock, so two
// sends at once cannot take the same one: the guest would accept the first
// and refuse the second as a replay.
func (s Store) NextSeq() (uint64, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(s.seqPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	b := make([]byte, 32)
	n, _ := f.ReadAt(b, 0)
	var last uint64
	if t := strings.TrimSpace(string(b[:n])); t != "" {
		if last, err = strconv.ParseUint(t, 10, 64); err != nil {
			return 0, fmt.Errorf("%s: %w", s.seqPath(), err)
		}
	}
	next := last + 1
	if err := f.Truncate(0); err != nil {
		return 0, err
	}
	if _, err := f.WriteAt([]byte(strconv.FormatUint(next, 10)+"\n"), 0); err != nil {
		return 0, err
	}
	return next, f.Sync()
}

// Seal makes the next envelope for this VM: it takes a sequence number and
// signs with the VM's key.
func (s Store) Seal(key ed25519.PrivateKey, session, kind, id, body string, now time.Time) ([]byte, agent.Envelope, error) {
	seq, err := s.NextSeq()
	if err != nil {
		return nil, agent.Envelope{}, err
	}
	if id == "" {
		id = "m" + strconv.FormatUint(seq, 10)
	}
	e := agent.Envelope{
		V: agent.EnvelopeVersion, Session: session, Seq: seq,
		Time: agent.Stamp(now), Kind: kind, ID: id, Body: body,
	}
	b, err := agent.Seal(key, e)
	return b, e, err
}

// Brief is the last brief delivered to this VM, which the next update is a
// diff against; ok is false when none was.
func (s Store) Brief() (text string, ok bool, err error) {
	b, err := os.ReadFile(s.briefPath())
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	return string(b), err == nil, err
}

// SetBrief records text as the brief delivered.
func (s Store) SetBrief(text string) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	tmp := s.briefPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.briefPath())
}
