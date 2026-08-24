package incus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
)

// PushDir copies the *contents* of hostDir into guestDir, creating it.
//
// This exists because `incus file push -r` names the guest directory after the
// source and silently ignores a trailing "/.", so `push -r src/. vm/work/foo/`
// lands in /work/foo/src/. Here the destination is exactly the destination.
func (c *Client) PushDir(name, hostDir, guestDir string) (int, error) {
	info, err := os.Stat(hostDir)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", hostDir)
	}
	if err := c.Mkdir(name, guestDir, 0o755); err != nil {
		return 0, err
	}

	// Directories before the files inside them, so parents always exist.
	var dirs, files []string
	err = filepath.WalkDir(hostDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(hostDir, p)
		if err != nil || rel == "." {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, rel)
		} else if d.Type().IsRegular() {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	sort.Strings(dirs)

	for _, rel := range dirs {
		if err := c.Mkdir(name, path.Join(guestDir, rel), 0o755); err != nil {
			return 0, err
		}
	}
	for _, rel := range files {
		src := filepath.Join(hostDir, rel)
		st, err := os.Stat(src)
		if err != nil {
			return 0, err
		}
		if err := c.PushFile(name, src, path.Join(guestDir, rel), st.Mode().Perm()); err != nil {
			return 0, fmt.Errorf("pushing %s: %w", rel, err)
		}
	}
	return len(files), nil
}

func (c *Client) PushFile(name, hostPath, guestPath string, mode fs.FileMode) error {
	body, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}
	return c.WriteFile(name, guestPath, body, mode)
}

func (c *Client) WriteFile(name, guestPath string, content []byte, mode fs.FileMode) error {
	return c.fileRequest(http.MethodPost, name, guestPath, content, map[string]string{
		"X-Incus-type":  "file",
		"X-Incus-mode":  fmt.Sprintf("%04o", mode.Perm()),
		"X-Incus-uid":   "0",
		"X-Incus-gid":   "0",
		"X-Incus-write": "overwrite",
	})
}

func (c *Client) Mkdir(name, guestPath string, mode fs.FileMode) error {
	err := c.fileRequest(http.MethodPost, name, guestPath, nil, map[string]string{
		"X-Incus-type": "directory",
		"X-Incus-mode": fmt.Sprintf("%04o", mode.Perm()),
		"X-Incus-uid":  "0",
		"X-Incus-gid":  "0",
	})
	// Already there is not a failure; mkdir here is always "ensure".
	var apiErr *APIError
	if err != nil && asAPIError(err, &apiErr) && apiErr.Code == http.StatusConflict {
		return nil
	}
	return err
}

func (c *Client) Pull(name, guestPath string) ([]byte, error) {
	q := url.Values{"path": {guestPath}}
	req, err := http.NewRequest(http.MethodGet,
		"http://incus/1.0/instances/"+url.PathEscape(name)+"/files?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, decodeFileError(raw, resp.StatusCode)
	}
	return raw, nil
}

func (c *Client) fileRequest(method, name, guestPath string, content []byte, headers map[string]string) error {
	q := url.Values{"path": {guestPath}}
	req, err := http.NewRequest(method,
		"http://incus/1.0/instances/"+url.PathEscape(name)+"/files?"+q.Encode(),
		bytes.NewReader(content))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return decodeFileError(raw, resp.StatusCode)
	}
	// The files endpoint answers with the usual envelope, which can still carry
	// an error at HTTP 200.
	var env envelope
	if json.Unmarshal(raw, &env) == nil && env.Type == "error" {
		return &APIError{Code: env.ErrorCode, Message: env.Error}
	}
	return nil
}

func decodeFileError(raw []byte, status int) error {
	var env envelope
	if json.Unmarshal(raw, &env) == nil && env.Error != "" {
		code := env.ErrorCode
		if code == 0 {
			code = status
		}
		return &APIError{Code: code, Message: env.Error}
	}
	return &APIError{Code: status, Message: fmt.Sprintf("%.200s", raw)}
}

func asAPIError(err error, target **APIError) bool {
	e, ok := err.(*APIError)
	if ok {
		*target = e
	}
	return ok
}
