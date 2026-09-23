// Package input forwards this host's keyboards and mice into a guest, while
// input is toggled to it, over vsock.
//
// It exists so a guest someone sits at can have a keyboard and mouse without
// being given the hardware. Passed through by identity (kind: usb), the guest
// talks to the real devices, and both of the reference host's have vendor
// configuration interfaces: a guest holding the keyboard could store a macro
// in it that later types into the host. Forwarded as events, the guest gets
// a virtual keyboard and mouse from its own uinput (base/rig-input) and never
// the devices.
//
// Why vsock rather than QEMU's own evdev forwarding: Incus runs every VM's
// QEMU as nobody and QEMU opens the evdev path itself, so the node it reads
// would have to be readable by nobody — which is also Incus's dnsmasq, the
// daemon every networked guest talks to. It would also need raw.qemu, which a
// manifest cannot declare, and QEMU opens the path only at VM start, so a
// restarted helper would leave the guest with dead input until the VM
// restarted too. Over vsock none of that holds, and either side can restart.
//
// The stream is one way: this process never reads from the connection. It is
// the root half of `rig host input`, it holds the host's real input devices,
// and nothing the guest sends can reach it. It does read every key pressed on
// the host while running, host mode included, because that is how it sees the
// toggle; it forwards nothing until toggled.
package input

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Port and Magic are shared with base/rig-input: the vsock port the guest
// listens on (above the TCP range, so no clash with rig forward) and the
// bytes that open every stream.
const (
	Port  = 65537
	Magic = "RIG1"
)

const (
	evSyn  = 0x00
	evKey  = 0x01
	evRel  = 0x02
	evLed  = 0x11
	keyA   = 30
	keyZ   = 44
	relX   = 0x00
	relY   = 0x01
	btnL   = 0x110
	ledScr = 0x02

	KeyLeftCtrl  = 29
	KeyRightCtrl = 97

	evIOCGrab = 0x40044590 // EVIOCGRAB, _IOW('E', 0x90, int)

	// writeTimeout is how long a batch may take to reach the guest before
	// the connection is taken for dead and input drops back to the host.
	writeTimeout = 500 * time.Millisecond
)

// Config is what the root half is handed; everything was resolved before
// the elevation.
type Config struct {
	VM      string
	CID     uint32
	Devices []string // explicit event nodes; empty means every keyboard and mouse
	Sysfs   string   // /sys, overridable for tests
}

// --- finding devices --------------------------------------------------------

// bitSet reports whether bit n is set in a sysfs capability bitmap: words in
// hex, most significant first, each an unsigned long.
func bitSet(bitmap string, n int) bool {
	words := strings.Fields(bitmap)
	idx := len(words) - 1 - n/64
	if idx < 0 || idx >= len(words) {
		return false
	}
	w, err := strconv.ParseUint(words[idx], 16, 64)
	if err != nil {
		return false
	}
	return w&(1<<(uint(n)%64)) != 0
}

// Candidate is an event node worth forwarding.
type Candidate struct {
	Node     string // /dev/input/eventN
	Name     string
	Keyboard bool
	Pointer  bool
}

// Candidates lists this host's keyboards and pointers from sysfs: a device
// with letter keys, or with X/Y motion and a left button. Power buttons, lid
// switches and the like have neither and are left alone.
func Candidates(sysfs string) ([]Candidate, error) {
	dirs, err := filepath.Glob(filepath.Join(sysfs, "class", "input", "event*"))
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, dir := range dirs {
		caps := filepath.Join(dir, "device", "capabilities")
		ev := read(filepath.Join(caps, "ev"))
		key := read(filepath.Join(caps, "key"))
		rel := read(filepath.Join(caps, "rel"))
		c := Candidate{
			Node: "/dev/input/" + filepath.Base(dir),
			Name: read(filepath.Join(dir, "device", "name")),
		}
		c.Keyboard = bitSet(ev, evKey) && bitSet(key, keyA) && bitSet(key, keyZ)
		c.Pointer = bitSet(ev, evRel) && bitSet(rel, relX) && bitSet(rel, relY) && bitSet(key, btnL)
		if c.Keyboard || c.Pointer {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out, nil
}

func read(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

// --- the toggle -------------------------------------------------------------

// Toggle watches both Ctrl keys. Pressing both arms it; releasing both then
// switches. Switching on the release means no key is held across the switch,
// so neither side is left with a Ctrl stuck down.
type Toggle struct {
	left, right, armed bool
}

// Key feeds one Ctrl event. armedNow is true on the event that armed the
// chord; switchNow on the event that completes it.
func (t *Toggle) Key(code uint16, value int32) (armedNow, switchNow bool) {
	down := value != 0
	switch code {
	case KeyLeftCtrl:
		t.left = down
	case KeyRightCtrl:
		t.right = down
	default:
		return false, false
	}
	if t.left && t.right && !t.armed {
		t.armed = true
		return true, false
	}
	if t.armed && !t.left && !t.right {
		t.armed = false
		return false, true
	}
	return false, false
}

// Armed reports whether the chord is held.
func (t *Toggle) Armed() bool { return t.armed }

// --- the wire ---------------------------------------------------------------

// Record encodes one event as base/rig-input reads it: type, code and value,
// little-endian, eight bytes.
func Record(typ, code uint16, value int32) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint16(b[0:], typ)
	binary.LittleEndian.PutUint16(b[2:], code)
	binary.LittleEndian.PutUint32(b[4:], uint32(value))
	return b[:]
}

// dial connects to the guest's listener, non-blocking so writes can carry a
// deadline: a guest that stops reading must not hold the host's keyboard.
func dial(cid uint32) (*os.File, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: Port}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d:%d", cid, Port))
	if err := send(f, []byte(Magic)); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func send(f *os.File, b []byte) error {
	if err := f.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	_, err := f.Write(b)
	return err
}

// --- the loop ---------------------------------------------------------------

type device struct {
	Candidate
	f       *os.File
	pending []byte
}

type event struct {
	dev       *device
	typ, code uint16
	value     int32
	gone      bool
}

func (d *device) grab(on bool) error {
	v := 0
	if on {
		v = 1
	}
	return unix.IoctlSetInt(int(d.f.Fd()), evIOCGrab, v)
}

// led lights Scroll Lock while input is with the guest: a cue on the keyboard
// itself, where there is one. Best effort; many keyboards have no such LED.
func (d *device) led(on bool) {
	if !d.Keyboard {
		return
	}
	v := int32(0)
	if on {
		v = 1
	}
	var ev [48]byte
	binary.LittleEndian.PutUint16(ev[16:], evLed)
	binary.LittleEndian.PutUint16(ev[18:], ledScr)
	binary.LittleEndian.PutUint32(ev[20:], uint32(v))
	// ev[24:48] is the zero SYN_REPORT.
	_, _ = d.f.Write(ev[:])
}

func (d *device) readLoop(out chan<- event) {
	var buf [24]byte
	for {
		if _, err := d.f.Read(buf[:]); err != nil {
			out <- event{dev: d, gone: true}
			return
		}
		out <- event{
			dev:   d,
			typ:   binary.LittleEndian.Uint16(buf[16:]),
			code:  binary.LittleEndian.Uint16(buf[18:]),
			value: int32(binary.LittleEndian.Uint32(buf[20:])),
		}
	}
}

// Run forwards input until ctx ends. It starts with input on the host; both
// Ctrl keys, pressed together and released, move it to the guest and back.
func Run(ctx context.Context, cfg Config, logf func(string, ...any)) error {
	if cfg.Sysfs == "" {
		cfg.Sysfs = "/sys"
	}
	events := make(chan event, 256)
	devs := map[string]*device{}
	var conn *os.File
	toGuest := false
	var tog Toggle
	pressed := map[uint16]bool{}

	wanted := func() []Candidate {
		cands, err := Candidates(cfg.Sysfs)
		if err != nil {
			return nil
		}
		if len(cfg.Devices) == 0 {
			return cands
		}
		var out []Candidate
		for _, c := range cands {
			for _, want := range cfg.Devices {
				if real, err := filepath.EvalSymlinks(want); err == nil && real == c.Node {
					out = append(out, c)
				}
			}
		}
		return out
	}
	rescan := func() {
		for _, c := range wanted() {
			if _, ok := devs[c.Node]; ok {
				continue
			}
			f, err := os.OpenFile(c.Node, os.O_RDWR, 0)
			if err != nil {
				logf("cannot open %s (%s): %v", c.Node, c.Name, err)
				continue
			}
			d := &device{Candidate: c, f: f}
			devs[c.Node] = d
			if toGuest {
				if err := d.grab(true); err != nil {
					logf("cannot grab %s: %v", c.Name, err)
				}
				d.led(true)
			}
			logf("using %s (%s)", c.Name, c.Node)
			go d.readLoop(events)
		}
	}
	flush := func(d *device) error {
		if conn == nil || len(d.pending) == 0 {
			d.pending = d.pending[:0]
			return nil
		}
		err := send(conn, d.pending)
		d.pending = d.pending[:0]
		return err
	}
	releaseAll := func() {
		if conn == nil {
			return
		}
		var b []byte
		for code := range pressed {
			b = append(b, Record(evKey, code, 0)...)
		}
		if len(b) > 0 {
			b = append(b, Record(evSyn, 0, 0)...)
			_ = send(conn, b)
		}
		pressed = map[uint16]bool{}
	}
	toHost := func(why string) {
		if !toGuest {
			return
		}
		releaseAll()
		for _, d := range devs {
			_ = d.grab(false)
			d.led(false)
			d.pending = d.pending[:0]
		}
		toGuest = false
		logf("input -> host (%s)", why)
	}
	drop := func(err error) {
		toHost("connection lost: " + err.Error())
		if conn != nil {
			conn.Close()
			conn = nil
		}
	}
	toGuestNow := func() {
		if conn == nil {
			logf("not connected to %s; input stays with the host", cfg.VM)
			return
		}
		for _, d := range devs {
			if err := d.grab(true); err != nil {
				logf("cannot grab %s: %v", d.Name, err)
			}
			d.led(true)
		}
		toGuest = true
		logf("input -> %s   (both Ctrl keys, pressed and released, to come back)", cfg.VM)
	}

	defer func() {
		toHost("stopping")
		for _, d := range devs {
			d.f.Close()
		}
		if conn != nil {
			conn.Close()
		}
	}()

	rescan()
	if len(devs) == 0 {
		return errors.New("no keyboard or mouse found on this host")
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	connect := func() {
		if conn != nil {
			return
		}
		c, err := dial(cfg.CID)
		if err != nil {
			return
		}
		conn = c
		logf("connected to %s; both Ctrl keys, pressed and released, move input to it", cfg.VM)
	}
	connect()
	if conn == nil {
		logf("waiting for %s's rig-input (vsock cid %d, port %d)...", cfg.VM, cfg.CID, Port)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			rescan()
			connect()
		case ev := <-events:
			d := ev.dev
			if ev.gone {
				logf("%s went away", d.Name)
				d.f.Close()
				delete(devs, d.Node)
				// A monitor's KVM switching away, very likely from a key
				// combination on this same keyboard, takes devices with keys
				// still down. The guest would never see those released, so
				// let go of everything now; the chord state goes with them.
				if toGuest {
					releaseAll()
				}
				tog = Toggle{}
				continue
			}
			if ev.typ == evKey {
				armedNow, switchNow := tog.Key(ev.code, ev.value)
				if armedNow && toGuest {
					// The chord is for rig, not the guest: let go of the
					// Ctrl presses it has already been sent.
					var b []byte
					for _, code := range []uint16{KeyLeftCtrl, KeyRightCtrl} {
						if pressed[code] {
							b = append(b, Record(evKey, code, 0)...)
							delete(pressed, code)
						}
					}
					if len(b) > 0 && conn != nil {
						if err := send(conn, append(b, Record(evSyn, 0, 0)...)); err != nil {
							drop(err)
						}
					}
				}
				if switchNow {
					if toGuest {
						toHost("toggled")
					} else {
						toGuestNow()
					}
					continue
				}
				if tog.Armed() && (ev.code == KeyLeftCtrl || ev.code == KeyRightCtrl) {
					continue
				}
			}
			if !toGuest {
				continue
			}
			switch ev.typ {
			case evKey:
				if ev.value == 0 {
					delete(pressed, ev.code)
				} else {
					pressed[ev.code] = true
				}
				d.pending = append(d.pending, Record(ev.typ, ev.code, ev.value)...)
			case evRel:
				d.pending = append(d.pending, Record(ev.typ, ev.code, ev.value)...)
			case evSyn:
				if ev.code == 0 {
					d.pending = append(d.pending, Record(evSyn, 0, 0)...)
					if err := flush(d); err != nil {
						drop(err)
					}
				}
			}
		}
	}
}
