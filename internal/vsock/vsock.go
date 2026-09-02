// Package vsock dials and listens on AF_VSOCK, the VM socket family.
//
// It exists because a rig guest has no IP route to this host and must not get
// one: the isolation ACL rejects every private range, which is the whole point
// of the project. vsock is not IP. It is the same out-of-band channel the Incus
// guest agent already uses, so carrying a socket over it opens nothing that was
// not already open.
//
// Incus proxy devices would be the obvious answer and cannot do this: 6.0.5
// supports only NAT mode for proxies on VM instances, which is host-to-guest
// through nftables — the wrong direction, and through the NIC where the ACL
// lives. vsock works in both directions, verified.
//
// The raw file descriptors are deliberate. Go's net package cannot wrap an
// AF_VSOCK socket — net.FileConn switches on the address family and returns
// EINVAL for anything that is not AF_UNIX, AF_INET or AF_INET6 — so these are
// os.File values rather than net.Conn. For a byte-for-byte forwarder that is
// all that is needed.
package vsock

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// CIDHost is the well-known context ID a guest uses to reach its hypervisor.
const CIDHost = 2

// Dial connects to a listener inside the guest with the given context ID.
func Dial(cid, port uint32) (*os.File, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock connect to cid %d port %d: %w\n"+
			"  Is anything listening on that port inside the guest?", cid, port, err)
	}
	return os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d:%d", cid, port)), nil
}

// Listener accepts vsock connections from any guest on this host.
type Listener struct {
	fd   int
	port uint32
}

// Listen binds a vsock port for connections from any guest.
//
// VMADDR_CID_ANY, so every VM on this host can reach it. With one project VM at
// a time that is not a distinction worth drawing, but it is worth knowing: a
// second guest could connect to the same port, and nothing here identifies
// which one did.
func Listen(port uint32) (*Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock bind port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock listen port %d: %w", port, err)
	}
	return &Listener{fd: fd, port: port}, nil
}

// Accept blocks for the next connection. Close makes it return an error, which
// is how a forwarder shuts down.
func (l *Listener) Accept() (*os.File, error) {
	nfd, _, err := unix.Accept(l.fd)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(nfd), fmt.Sprintf("vsock:accepted:%d", l.port)), nil
}

func (l *Listener) Close() error { return unix.Close(l.fd) }
