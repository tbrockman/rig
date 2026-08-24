package incus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// ExecOpts drives Exec.
//
// The defaults absorb two Incus behaviours that are easy to get wrong by hand:
//
//   - a bare `bash -c` in a guest runs with a stub PATH (/usr/bin:/bin and
//     friends), none of which exist on NixOS. Commands run through a *login*
//     shell so the system profile is on PATH.
//   - stdin is closed immediately unless asked for, so a command that reads it
//     gets EOF rather than hanging forever.
type ExecOpts struct {
	Dir       string            // cd here first
	Env       map[string]string // extra environment
	Stdin     bool              // forward our stdin
	Timeout   time.Duration     // 0 means no limit
	Streaming bool              // stream to our stdout/stderr instead of capturing
}

// ExitError reports a non-zero exit from the guest command. Separating it from
// transport failures lets callers tell "the command said no" from "the VM is
// unreachable".
type ExitError struct {
	Code   int
	Output string
}

func (e *ExitError) Error() string { return fmt.Sprintf("command exited %d", e.Code) }

// execFDs mirrors the async response, whose top-level metadata is the whole
// operation object; the fd secrets are inside the operation's own metadata.
type execFDs struct {
	Metadata struct {
		FDs map[string]string `json:"fds"`
	} `json:"metadata"`
}

// Exec runs a shell command in the guest over the exec websocket API and
// returns its output. With Streaming, output goes to our stdout/stderr as it
// arrives and the returned string is empty.
func (c *Client) Exec(name, command string, o ExecOpts) (string, error) {
	ctx := context.Background()
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}

	guestEnv := map[string]string{}
	for k, v := range o.Env {
		guestEnv[k] = v
	}
	// The command travels in the environment, so there is no quoting layer
	// between here and the guest. `exec` matters: a NixOS login shell writes a
	// stray terminal-title escape to stdout when it exits, which corrupts
	// captured output. Replacing it with the command means it never exits.
	guestEnv["RIG_COMMAND"] = command
	script := `exec bash -c "$RIG_COMMAND"`
	if o.Dir != "" {
		guestEnv["RIG_DIR"] = o.Dir
		script = `cd "$RIG_DIR" && ` + script
	}

	body := map[string]any{
		"command":            []string{"bash", "-lc", script},
		"wait-for-websocket": true,
		"interactive":        false,
		"environment":        guestEnv,
	}

	env, _, err := c.call("POST", "/1.0/instances/"+url.PathEscape(name)+"/exec", body, "")
	if err != nil {
		return "", err
	}
	var fds execFDs
	if err := json.Unmarshal(env.Metadata, &fds); err != nil {
		return "", fmt.Errorf("exec: cannot read fd secrets: %w", err)
	}

	opPath := env.Operation
	dial := func(fd string) (*websocket.Conn, error) {
		secret, ok := fds.Metadata.FDs[fd]
		if !ok {
			return nil, fmt.Errorf("exec: no websocket for fd %s", fd)
		}
		return c.dialOperation(ctx, opPath, secret)
	}

	// Incus holds the command until the sockets are connected (wait-for-websocket),
	// so connect all three before doing anything else.
	stdinConn, err := dial("0")
	if err != nil {
		return "", err
	}
	stdoutConn, err := dial("1")
	if err != nil {
		return "", err
	}
	stderrConn, err := dial("2")
	if err != nil {
		return "", err
	}

	var out bytes.Buffer
	var stdoutW, stderrW io.Writer = &out, &out
	if o.Streaming {
		stdoutW, stderrW = os.Stdout, os.Stderr
	}

	done := make(chan struct{}, 2)
	go func() { pump(ctx, stdoutConn, stdoutW); done <- struct{}{} }()
	go func() { pump(ctx, stderrConn, stderrW); done <- struct{}{} }()

	if o.Stdin {
		go func() {
			_, _ = io.Copy(wsWriter{ctx, stdinConn}, os.Stdin)
			stdinConn.Close(websocket.StatusNormalClosure, "")
		}()
	} else {
		// Closing fd 0 immediately is the equivalent of </dev/null.
		stdinConn.Close(websocket.StatusNormalClosure, "")
	}

	for range 2 {
		select {
		case <-done:
		case <-ctx.Done():
			return out.String(), fmt.Errorf("exec in %s timed out after %s", name, o.Timeout)
		}
	}

	code, err := c.waitExec(opPath, o.Timeout)
	if err != nil {
		return out.String(), err
	}
	text := strings.TrimRight(out.String(), "\n")
	if code != 0 {
		return text, &ExitError{Code: code, Output: text}
	}
	return text, nil
}

func (c *Client) dialOperation(ctx context.Context, opPath, secret string) (*websocket.Conn, error) {
	q := url.Values{"secret": {secret}}
	// Host is irrelevant: the transport dials the unix socket regardless.
	u := "ws://incus" + opPath + "/websocket?" + q.Encode()
	conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.ws})
	if err != nil {
		return nil, fmt.Errorf("exec: websocket dial: %w", err)
	}
	conn.SetReadLimit(-1)
	return conn, nil
}

// waitExec blocks on the exec operation and returns the command's exit code.
func (c *Client) waitExec(opPath string, timeout time.Duration) (int, error) {
	if timeout <= 0 {
		timeout = 24 * time.Hour
	}
	q := url.Values{"timeout": {fmt.Sprint(int(timeout.Seconds()))}}
	var res struct {
		Err        string `json:"err"`
		StatusCode int    `json:"status_code"`
		Metadata   struct {
			Return *int `json:"return"`
		} `json:"metadata"`
	}
	if _, err := c.Get(opPath+"/wait?"+q.Encode(), &res); err != nil {
		return -1, err
	}
	if res.Err != "" {
		return -1, fmt.Errorf("exec failed: %s", res.Err)
	}
	if res.Metadata.Return == nil {
		return -1, errors.New("exec finished without reporting an exit code")
	}
	return *res.Metadata.Return, nil
}

func pump(ctx context.Context, conn *websocket.Conn, w io.Writer) {
	defer conn.Close(websocket.StatusNormalClosure, "")
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return // close or context cancellation: both mean done
		}
		if len(data) == 0 {
			return // Incus signals EOF with an empty frame
		}
		if _, err := w.Write(data); err != nil {
			return
		}
	}
}

type wsWriter struct {
	ctx  context.Context
	conn *websocket.Conn
}

func (w wsWriter) Write(p []byte) (int, error) {
	if err := w.conn.Write(w.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// WaitAgent polls until the Incus guest agent answers. Polling, never a fixed
// sleep: boot time varies with what the guest does on start.
func (c *Client) WaitAgent(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := c.Exec(name, "true", ExecOpts{Timeout: 10 * time.Second}); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("guest agent in %s did not come up within %s", name, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// WaitAddress polls until the guest holds a global IPv4 address.
//
// Separate from WaitAgent on purpose: the agent answers several seconds before
// DHCP completes, so code that waits only for the agent and then touches the
// network sees a spurious DNS failure.
func (c *Client) WaitAddress(name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		addr, err := c.GlobalIPv4(name)
		if err == nil && addr != "" {
			return addr, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s had no IPv4 address within %s", name, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}
