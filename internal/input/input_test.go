package input

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestBitSetReadsSysfsBitmaps(t *testing.T) {
	// A keyboard's key bitmap ends in a word with KEY_A (30) and KEY_Z (44);
	// the earlier words are higher bits, most significant first.
	bitmap := "1000000 0 0 fffffffffffffffe"
	for _, n := range []int{1, 30, 44, 63} {
		if !bitSet(bitmap, n) {
			t.Errorf("bit %d should be set in %q", n, bitmap)
		}
	}
	if bitSet(bitmap, 0) {
		t.Error("bit 0 is clear")
	}
	if !bitSet(bitmap, 3*64+24) {
		t.Error("bit 216 is in the first (most significant) word")
	}
	if bitSet(bitmap, 4*64) || bitSet("", 1) || bitSet("zz", 1) {
		t.Error("out of range, empty or malformed bitmaps set nothing")
	}
}

func fakeInput(t *testing.T, devs map[string][4]string) string {
	t.Helper()
	root := t.TempDir()
	for ev, d := range devs { // name, ev, key, rel
		dir := filepath.Join(root, "class", "input", ev, "device")
		if err := os.MkdirAll(filepath.Join(dir, "capabilities"), 0o755); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{"name": d[0], "capabilities/ev": d[1], "capabilities/key": d[2], "capabilities/rel": d[3]}
		for f, v := range files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// Keyboards and pointers are forwarded; a power button is not, though it is
// also an EV_KEY device.
func TestCandidatesKeepKeyboardsAndPointersOnly(t *testing.T) {
	root := fakeInput(t, map[string][4]string{
		"event3": {"EPOMAKER TH80 Pro", "120013", "1000000 0 0 fffffffffffffffe", "0"},
		"event9": {"Razer DeathAdder Essential", "17", "1f0000 0 0 0 0", "903"},
		"event0": {"Power Button", "3", "10000000000000 0", "0"},
	})
	got, err := Candidates(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want the keyboard and the mouse, got %+v", got)
	}
	if got[0].Node != "/dev/input/event3" || !got[0].Keyboard || got[0].Pointer {
		t.Errorf("event3 should be a keyboard only: %+v", got[0])
	}
	if got[1].Node != "/dev/input/event9" || !got[1].Pointer || got[1].Keyboard {
		t.Errorf("event9 should be a pointer only: %+v", got[1])
	}
}

// Both Ctrl keys arm the toggle; it switches only once both are released, so
// no key is held across the switch. One Ctrl alone never toggles.
func TestToggleSwitchesOnReleaseOfBothCtrls(t *testing.T) {
	var tg Toggle
	step := func(code uint16, v int32, wantArm, wantSwitch bool) {
		t.Helper()
		arm, sw := tg.Key(code, v)
		if arm != wantArm || sw != wantSwitch {
			t.Fatalf("key %d=%d: armed=%v switch=%v, want %v %v", code, v, arm, sw, wantArm, wantSwitch)
		}
	}
	step(KeyLeftCtrl, 1, false, false)
	step(30, 1, false, false) // Ctrl+A is an ordinary shortcut
	step(30, 0, false, false)
	step(KeyLeftCtrl, 0, false, false)

	step(KeyLeftCtrl, 1, false, false)
	step(KeyRightCtrl, 1, true, false)
	step(KeyLeftCtrl, 2, false, false) // autorepeat changes nothing
	step(KeyLeftCtrl, 0, false, false)
	if !tg.Armed() {
		t.Fatal("one Ctrl still held: still armed")
	}
	step(KeyRightCtrl, 0, false, true)
	if tg.Armed() {
		t.Fatal("disarmed after switching")
	}
}

func TestRecordIsWhatTheGuestReads(t *testing.T) {
	b := Record(1, 30, -1)
	if len(b) != 8 || binary.LittleEndian.Uint16(b[0:]) != 1 || binary.LittleEndian.Uint16(b[2:]) != 30 ||
		int32(binary.LittleEndian.Uint32(b[4:])) != -1 {
		t.Fatalf("record %x", b)
	}
}
