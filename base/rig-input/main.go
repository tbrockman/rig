// Command rig-input puts the host's keyboard and mouse into this guest.
//
// `rig host input <vm>` on the host grabs its keyboards and mice while input
// is toggled to the guest, and streams their events here over vsock. This
// daemon replays them through two uinput devices, so the guest sees a virtual
// keyboard and mouse and never the hardware. That matters because the real
// devices have vendor configuration interfaces: a guest holding them could
// program a macro into the keyboard that later types into the host.
//
// The stream is one way. Nothing is ever written back, so nothing the guest
// does reaches the process on the host that holds the real devices.
//
// No dependencies beyond the standard library, so the guest image builds it
// with no vendor hash to maintain; the vsock and uinput calls are raw syscalls.
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"syscall"
	"unsafe"
)

// Port is the vsock port the host dials: the first above the TCP range, so it
// can never collide with `rig forward`, which uses the TCP port number.
const Port = 65537

// Magic opens every stream, so a stray connection is refused rather than
// replayed as keystrokes.
const Magic = "RIG1"

const (
	afVsock    = 40
	cidAny     = 0xFFFFFFFF
	cidHost    = 2
	evSyn      = 0x00
	evKey      = 0x01
	evRel      = 0x02
	keyMax     = 0x2ff
	relMax     = 0x0f
	btnMouseLo = 0x110 // BTN_LEFT
	btnMouseHi = 0x11f // up to BTN_TASK and the vendor buttons beside it
	busVirtual = 0x06

	uiDevCreate  = 0x5501
	uiDevDestroy = 0x5502
	uiDevSetup   = 0x405c5503
	uiSetEvbit   = 0x40045564
	uiSetKeybit  = 0x40045565
	uiSetRelbit  = 0x40045566
)

type sockaddrVM struct {
	Family   uint16
	Reserved uint16
	Port     uint32
	CID      uint32
	Flags    uint8
	Zero     [3]uint8
}

// device is one uinput device and the keys it currently holds down.
type device struct {
	f       *os.File
	pressed map[uint16]bool
	touched bool
}

func ioctl(f *os.File, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, arg); e != 0 {
		return e
	}
	return nil
}

func newDevice(name string, product uint16, keys []uint16, rels []uint16) (*device, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/uinput: %w", err)
	}
	fail := func(err error) (*device, error) { f.Close(); return nil, err }
	if err := ioctl(f, uiSetEvbit, evKey); err != nil {
		return fail(err)
	}
	for _, k := range keys {
		if err := ioctl(f, uiSetKeybit, uintptr(k)); err != nil {
			return fail(err)
		}
	}
	if len(rels) > 0 {
		if err := ioctl(f, uiSetEvbit, evRel); err != nil {
			return fail(err)
		}
		for _, r := range rels {
			if err := ioctl(f, uiSetRelbit, uintptr(r)); err != nil {
				return fail(err)
			}
		}
	}
	// struct uinput_setup: input_id (4 x u16), name[80], ff_effects_max u32.
	var setup [92]byte
	binary.LittleEndian.PutUint16(setup[0:], busVirtual)
	binary.LittleEndian.PutUint16(setup[2:], 0x1209) // pid.codes test vendor
	binary.LittleEndian.PutUint16(setup[4:], product)
	binary.LittleEndian.PutUint16(setup[6:], 1)
	copy(setup[8:87], name)
	if err := ioctl(f, uiDevSetup, uintptr(unsafe.Pointer(&setup[0]))); err != nil {
		return fail(fmt.Errorf("UI_DEV_SETUP: %w", err))
	}
	if err := ioctl(f, uiDevCreate, 0); err != nil {
		return fail(fmt.Errorf("UI_DEV_CREATE: %w", err))
	}
	return &device{f: f, pressed: map[uint16]bool{}}, nil
}

// emit writes one struct input_event; the kernel stamps the time.
func (d *device) emit(typ, code uint16, value int32) error {
	var ev [24]byte
	binary.LittleEndian.PutUint16(ev[16:], typ)
	binary.LittleEndian.PutUint16(ev[18:], code)
	binary.LittleEndian.PutUint32(ev[20:], uint32(value))
	_, err := d.f.Write(ev[:])
	return err
}

// releaseAll lets go of every key this device holds, so a dropped connection
// cannot leave the guest with a key stuck down.
func (d *device) releaseAll() {
	if len(d.pressed) == 0 {
		return
	}
	for code := range d.pressed {
		_ = d.emit(evKey, code, 0)
	}
	_ = d.emit(evSyn, 0, 0)
	d.pressed = map[uint16]bool{}
}

func keyboardKeys() []uint16 {
	var out []uint16
	for k := uint16(1); k <= keyMax; k++ {
		if k >= 0x100 && k < 0x160 { // buttons: the mouse's, and joystick/gamepad ones nothing sends
			continue
		}
		out = append(out, k)
	}
	return out
}

func mouseButtons() []uint16 {
	var out []uint16
	for k := uint16(btnMouseLo); k <= btnMouseHi; k++ {
		out = append(out, k)
	}
	return out
}

// serve replays one connection until it ends.
func serve(conn *os.File, kbd, mouse *device) error {
	r := bufio.NewReader(conn)
	magic := make([]byte, len(Magic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != Magic {
		return errors.New("stream did not open with the rig-input magic; dropped")
	}
	defer kbd.releaseAll()
	defer mouse.releaseAll()
	var rec [8]byte
	for {
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		typ := binary.LittleEndian.Uint16(rec[0:])
		code := binary.LittleEndian.Uint16(rec[2:])
		value := int32(binary.LittleEndian.Uint32(rec[4:]))
		switch {
		case typ == evSyn && code == 0:
			for _, d := range []*device{kbd, mouse} {
				if d.touched {
					if err := d.emit(evSyn, 0, 0); err != nil {
						return err
					}
					d.touched = false
				}
			}
		case typ == evKey && code <= keyMax && value >= 0 && value <= 2:
			d := kbd
			if code >= btnMouseLo && code <= btnMouseHi {
				d = mouse
			}
			if value == 0 {
				delete(d.pressed, code)
			} else {
				d.pressed[code] = true
			}
			if err := d.emit(typ, code, value); err != nil {
				return err
			}
			d.touched = true
		case typ == evRel && code <= relMax:
			if err := mouse.emit(typ, code, value); err != nil {
				return err
			}
			mouse.touched = true
		}
		// Anything else is dropped: the host sends nothing else, and this
		// side replays only what a keyboard and a mouse can say.
	}
}

func main() {
	log.SetFlags(0)
	kbd, err := newDevice("rig keyboard", 0x0001, keyboardKeys(), nil)
	if err != nil {
		log.Fatal(err)
	}
	mouse, err := newDevice("rig mouse", 0x0002, mouseButtons(),
		[]uint16{0x00, 0x01, 0x06, 0x08, 0x0b, 0x0c}) // X, Y, HWHEEL, WHEEL, and their hi-res forms
	if err != nil {
		log.Fatal(err)
	}
	defer ioctl(kbd.f, uiDevDestroy, 0)
	defer ioctl(mouse.f, uiDevDestroy, 0)

	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		log.Fatalf("vsock socket: %v", err)
	}
	sa := sockaddrVM{Family: afVsock, Port: Port, CID: cidAny}
	if _, _, e := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); e != 0 {
		log.Fatalf("vsock bind port %d: %v", Port, e)
	}
	if err := syscall.Listen(fd, 1); err != nil {
		log.Fatalf("vsock listen: %v", err)
	}
	log.Printf("rig-input: listening on vsock port %d", Port)
	for {
		var peer sockaddrVM
		plen := uint32(unsafe.Sizeof(peer))
		nfd, _, e := syscall.Syscall6(syscall.SYS_ACCEPT4, uintptr(fd), uintptr(unsafe.Pointer(&peer)),
			uintptr(unsafe.Pointer(&plen)), syscall.SOCK_CLOEXEC, 0, 0)
		if e != 0 {
			log.Printf("rig-input: accept: %v", e)
			continue
		}
		conn := os.NewFile(nfd, "vsock")
		// Only the host. vhost-vsock carries nothing between guests, but the
		// check costs nothing and says what this socket is for.
		if peer.CID != cidHost {
			log.Printf("rig-input: refused a connection from cid %d; only the host (cid %d) may send input", peer.CID, cidHost)
			conn.Close()
			continue
		}
		log.Printf("rig-input: host connected")
		if err := serve(conn, kbd, mouse); err != nil {
			log.Printf("rig-input: %v", err)
		}
		conn.Close()
		log.Printf("rig-input: host disconnected; every key released")
	}
}
