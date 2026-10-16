package operator

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tbrockman/rig/internal/agent"
)

func TestTheStoreIsUnderTheUserConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/home/me/.config")
	s, err := For("myproj")
	if err != nil || s.Dir != "/home/me/.config/rig/agent/myproj" {
		t.Errorf("got %q, %v", s.Dir, err)
	}
}

// Made once, private, and loaded the same ever after; send never makes one.
func TestTheKeyIsMadeOnceAndKeptPrivate(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "myproj")}
	if _, err := s.Key(); !os.IsNotExist(err) {
		t.Errorf("Key must not make a key, got %v", err)
	}
	k1, made, err := s.KeyOrCreate()
	if err != nil || !made {
		t.Fatalf("%v, made=%v", err, made)
	}
	k2, made, err := s.KeyOrCreate()
	if err != nil || made || !k1.Equal(k2) {
		t.Fatalf("the second call must load the first key: %v, made=%v", err, made)
	}
	st, _ := os.Stat(s.KeyPath())
	dir, _ := os.Stat(s.Dir)
	if st.Mode().Perm() != 0o600 || dir.Mode().Perm() != 0o700 {
		t.Errorf("key %v, dir %v", st.Mode(), dir.Mode())
	}
	os.Chmod(s.KeyPath(), 0o644)
	if _, err := s.Key(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a key others can read must be refused, got %v", err)
	}
}

// Two sends at once must not take the same number: the guest would refuse the
// second as a replay.
func TestSequenceNumbersAreNeverReused(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	var mu sync.Mutex
	seen := map[uint64]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				n, err := s.NextSeq()
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if seen[n] {
					t.Errorf("#%d taken twice", n)
				}
				seen[n] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 160 {
		t.Errorf("took %d numbers", len(seen))
	}
}

func TestSealSignsWithTheNextNumber(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	key, _, _ := s.KeyOrCreate()
	data, e, err := s.Seal(key, "sess", agent.KindMessage, "", "hello", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err := agent.Open(key.Public().(ed25519.PublicKey), data)
	if err != nil || got != e || e.Seq != 1 || e.ID != "m1" {
		t.Errorf("got %+v, %v", got, err)
	}
	_, e2, _ := s.Seal(key, "sess", agent.KindAnswer, "q", "yes", time.Now())
	if e2.Seq != 2 || e2.ID != "q" {
		t.Errorf("got %+v", e2)
	}
}

func TestTheLastBriefIsKept(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "vm")}
	if _, ok, err := s.Brief(); ok || err != nil {
		t.Errorf("none yet: %v %v", ok, err)
	}
	s.SetBrief("# v1\n")
	if text, ok, _ := s.Brief(); !ok || text != "# v1\n" {
		t.Errorf("got %q", text)
	}
}
