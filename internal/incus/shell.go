package incus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"unsafe"

	"github.com/coder/websocket"
)

// Shell runs an interactive login shell in the guest, with a TTY.
//
// Terminal handling is done with stdlib ioctls rather than a helper library:
// this is the only place that needs raw mode, and it keeps the dependency list
// at the single websocket module.
func (c *Client) Shell(name string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rows, cols := windowSize(os.Stdout)
	body := map[string]any{
		"command":            []string{"bash", "-l"},
		"wait-for-websocket": true,
		"interactive":        true,
		"width":              cols,
		"height":             rows,
	}
	env, _, err := c.call("POST", "/1.0/instances/"+url.PathEscape(name)+"/exec", body, "")
	if err != nil {
		return err
	}
	var fds execFDs
	if err := json.Unmarshal(env.Metadata, &fds); err != nil {
		return fmt.Errorf("shell: cannot read fd secrets: %w", err)
	}

	// Interactive mode multiplexes stdio onto one socket; "control" carries
	// window resizes.
	conn, err := c.dialOperation(ctx, env.Operation, fds.Metadata.FDs["0"])
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var control *websocket.Conn
	if secret, ok := fds.Metadata.FDs["control"]; ok {
		if control, err = c.dialOperation(ctx, env.Operation, secret); err == nil {
			defer control.Close(websocket.StatusNormalClosure, "")
			winch := make(chan os.Signal, 1)
			signal.Notify(winch, syscall.SIGWINCH)
			defer signal.Stop(winch)
			go func() {
				for range winch {
					r, c2 := windowSize(os.Stdout)
					msg, _ := json.Marshal(map[string]any{
						"command": "window-resize",
						"args":    map[string]string{"width": fmt.Sprint(c2), "height": fmt.Sprint(r)},
					})
					_ = control.Write(ctx, websocket.MessageText, msg)
				}
			}()
		}
	}

	if restore, err := makeRaw(os.Stdin); err == nil {
		defer restore()
	}

	go func() {
		_, _ = io.Copy(wsWriter{ctx, conn}, os.Stdin)
		conn.Close(websocket.StatusNormalClosure, "")
	}()
	pump(ctx, conn, os.Stdout)

	code, err := c.waitExec(env.Operation, 0)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

func ioctl(f *os.File, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

func windowSize(f *os.File) (rows, cols int) {
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	if err := ioctl(f, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err != nil || ws.Row == 0 {
		return 24, 80
	}
	return int(ws.Row), int(ws.Col)
}

// makeRaw puts the terminal in raw mode and returns a restore func.
func makeRaw(f *os.File) (func(), error) {
	var orig syscall.Termios
	if err := ioctl(f, syscall.TCGETS, unsafe.Pointer(&orig)); err != nil {
		return nil, err
	}
	raw := orig
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctl(f, syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	return func() { _ = ioctl(f, syscall.TCSETS, unsafe.Pointer(&orig)) }, nil
}
