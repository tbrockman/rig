// Package incus is a small typed client for the Incus REST API over its unix
// socket. Stdlib only.
//
// Everything that reads or changes state goes through here rather than shelling
// out to the CLI, so results are typed instead of parsed. The one exception is
// Exec, which needs streaming stdio and therefore a websocket client that the
// stdlib does not have; see exec.go.
package incus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

const DefaultSocket = "/var/lib/incus/unix.socket"

type Client struct {
	socket string
	http   *http.Client
	// ws has no client-level timeout: exec sockets are long-lived, and the
	// websocket library rejects a client that carries one. Cancellation for
	// those comes from the context instead.
	ws *http.Client
}

func New(socket string) *Client {
	if socket == "" {
		socket = os.Getenv("INCUS_SOCKET")
	}
	if socket == "" {
		socket = DefaultSocket
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &Client{
		socket: socket,
		http:   &http.Client{Timeout: 15 * time.Minute, Transport: transport},
		ws:     &http.Client{Transport: transport},
	}
}

// socketHint names the two ways a fresh host fails to reach the daemon, which
// the raw dial error leaves to the reader: the socket is not there, or this
// user may not open it.
func socketHint(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "\n  Is Incus installed and running?  systemctl status incus\n" +
			"  INCUS_SOCKET names the socket if it lives somewhere else."
	case errors.Is(err, os.ErrPermission):
		return "\n  This user cannot open the socket. Join the group Incus grants it to, then\n" +
			"  log in again:  sudo usermod -aG incus-admin $USER"
	}
	return ""
}

// APIError is returned for a well-formed Incus error response, so callers can
// react to a status code rather than matching on message text.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("incus: %s (code %d)", e.Message, e.Code) }

func (e *APIError) NotFound() bool { return e.Code == http.StatusNotFound }

type envelope struct {
	Type       string          `json:"type"`
	Status     string          `json:"status"`
	StatusCode int             `json:"status_code"`
	Operation  string          `json:"operation"`
	ErrorCode  int             `json:"error_code"`
	Error      string          `json:"error"`
	Metadata   json.RawMessage `json:"metadata"`
}

func (c *Client) call(method, path string, body any, etag string) (*envelope, string, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, "", err
		}
		rdr = bytes.NewReader(buf)
	}

	req, err := http.NewRequest(method, "http://incus"+path, rdr)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	return c.do(req)
}

// do sends a prepared request and parses Incus's response envelope. Separate
// from call because an image upload builds its own streaming multipart request.
func (c *Client) do(req *http.Request) (*envelope, string, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("cannot reach incus at %s: %w%s", c.socket, err, socketHint(err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, "", fmt.Errorf("non-JSON response from %s: %.200s", req.URL.Path, raw)
	}
	if env.Type == "error" {
		return nil, "", &APIError{Code: env.ErrorCode, Message: env.Error}
	}
	return &env, resp.Header.Get("ETag"), nil
}

// Get decodes a synchronous GET into out and returns the ETag, which
// PutWithETag needs for the optimistic-concurrency round-trip Incus expects.
func (c *Client) Get(path string, out any) (string, error) {
	env, etag, err := c.call(http.MethodGet, path, nil, "")
	if err != nil {
		return "", err
	}
	if out != nil && len(env.Metadata) > 0 {
		if err := json.Unmarshal(env.Metadata, out); err != nil {
			return "", fmt.Errorf("decoding %s: %w", path, err)
		}
	}
	return etag, nil
}

func (c *Client) Post(path string, body, out any) error {
	return c.mutate(http.MethodPost, path, body, "", out)
}

func (c *Client) Put(path string, body any) error {
	return c.mutate(http.MethodPut, path, body, "", nil)
}

func (c *Client) PutWithETag(path string, body any, etag string) error {
	return c.mutate(http.MethodPut, path, body, etag, nil)
}

func (c *Client) Delete(path string) error {
	return c.mutate(http.MethodDelete, path, nil, "", nil)
}

func (c *Client) mutate(method, path string, body any, etag string, out any) error {
	env, _, err := c.call(method, path, body, etag)
	if err != nil {
		return err
	}
	if env.Type == "async" {
		return c.wait(env.Operation, 10*time.Minute)
	}
	if out != nil && len(env.Metadata) > 0 {
		return json.Unmarshal(env.Metadata, out)
	}
	return nil
}

// wait blocks on an async operation. Incus reports operation failure inside a
// 200 response, so the metadata has to be inspected rather than the status code.
func (c *Client) wait(op string, timeout time.Duration) error {
	return c.WaitOperation(op, timeout, nil)
}

// WaitOperation blocks on an async operation and decodes its final metadata
// into out. An operation carries results there and nowhere else — an image
// upload reports the new fingerprint this way — so waiting without reading it
// throws them away.
func (c *Client) WaitOperation(op string, timeout time.Duration, out any) error {
	if op == "" {
		if out == nil {
			return nil
		}
		return errors.New("expected an async operation, got none")
	}
	q := url.Values{"timeout": {fmt.Sprint(int(timeout.Seconds()))}}
	var res struct {
		Err        string          `json:"err"`
		Status     string          `json:"status"`
		StatusCode int             `json:"status_code"`
		Metadata   json.RawMessage `json:"metadata"`
	}
	if _, err := c.Get(op+"/wait?"+q.Encode(), &res); err != nil {
		return err
	}
	if res.Err != "" {
		return fmt.Errorf("operation failed: %s", res.Err)
	}
	if res.StatusCode >= 400 {
		return fmt.Errorf("operation failed: %s", res.Status)
	}
	if out != nil && len(res.Metadata) > 0 {
		return json.Unmarshal(res.Metadata, out)
	}
	return nil
}
