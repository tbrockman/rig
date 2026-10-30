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
	"strconv"
	"time"
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

// FileInfo is what the files endpoint says about a guest path, in the headers
// of any request for it.
type FileInfo struct {
	Type     string // "file", "directory" or "symlink"
	Mode     fs.FileMode
	Size     int64 // a file's length; 0 for anything else
	Modified time.Time
}

// NotAFileError is a guest path that exists but is not a regular file. The
// endpoint answers a directory with its listing and a symlink with its target,
// and reading either as a file's content is a wrong answer that exits 0.
type NotAFileError struct {
	Path string
	Type string
}

func (e *NotAFileError) Error() string {
	return fmt.Sprintf("%s is a %s in the guest, not a file", e.Path, e.Type)
}

// Stat asks for guestPath's headers alone, so learning what a path is never
// transfers a file's content.
func (c *Client) Stat(name, guestPath string) (FileInfo, error) {
	resp, err := c.fileGet(http.MethodHead, name, guestPath)
	if err != nil {
		return FileInfo{}, err
	}
	resp.Body.Close()
	return fileInfo(resp.Header), nil
}

// ReadDir lists a guest directory's names, sorted.
func (c *Client) ReadDir(name, guestDir string) ([]string, error) {
	resp, err := c.fileGet(http.MethodGet, name, guestDir)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if info := fileInfo(resp.Header); info.Type != "directory" {
		return nil, fmt.Errorf("%s is a %s in the guest, not a directory", guestDir, info.Type)
	}
	var env envelope
	var names []string
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("listing %s: %w", guestDir, err)
	}
	if err := json.Unmarshal(env.Metadata, &names); err != nil {
		return nil, fmt.Errorf("listing %s: %w", guestDir, err)
	}
	sort.Strings(names)
	return names, nil
}

// PullTo streams one guest file into w, so a large file never sits in memory.
func (c *Client) PullTo(name, guestPath string, w io.Writer) (FileInfo, error) {
	resp, err := c.fileGet(http.MethodGet, name, guestPath)
	if err != nil {
		return FileInfo{}, err
	}
	defer resp.Body.Close()
	info := fileInfo(resp.Header)
	if info.Type != "file" {
		return info, &NotAFileError{guestPath, info.Type}
	}
	_, err = io.Copy(w, resp.Body)
	return info, err
}

// Pull reads one guest file. A directory or a symlink is a NotAFileError.
func (c *Client) Pull(name, guestPath string) ([]byte, error) {
	var b bytes.Buffer
	if _, err := c.PullTo(name, guestPath, &b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// GuestEntry is one path under a directory Tree walked, relative to it.
type GuestEntry struct {
	Rel string
	FileInfo
}

// Tree lists everything under guestDir, each directory before what it
// contains. It asks for each entry's headers and each directory's listing, and
// reads no file's content.
func (c *Client) Tree(name, guestDir string) ([]GuestEntry, error) {
	var out []GuestEntry
	var walk func(rel string) error
	walk = func(rel string) error {
		names, err := c.ReadDir(name, path.Join(guestDir, rel))
		if err != nil {
			return err
		}
		for _, n := range names {
			r := path.Join(rel, n)
			info, err := c.Stat(name, path.Join(guestDir, r))
			if err != nil {
				return err
			}
			out = append(out, GuestEntry{r, info})
			if info.Type == "directory" {
				if err := walk(r); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return out, walk("")
}

// fileGet sends a GET or HEAD for guestPath and returns the response with its
// body open, or the decoded error.
func (c *Client) fileGet(method, name, guestPath string) (*http.Response, error) {
	q := url.Values{"path": {guestPath}}
	req, err := http.NewRequest(method,
		"http://incus/1.0/instances/"+url.PathEscape(name)+"/files?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if method == http.MethodHead {
			// A HEAD carries no envelope to decode; the status is all there is.
			return nil, &APIError{Code: resp.StatusCode, Message: fmt.Sprintf("%s: %s", guestPath, http.StatusText(resp.StatusCode))}
		}
		return nil, decodeFileError(raw, resp.StatusCode)
	}
	return resp, nil
}

func fileInfo(h http.Header) FileInfo {
	info := FileInfo{Type: h.Get("X-Incus-Type")}
	if m, err := strconv.ParseUint(h.Get("X-Incus-Mode"), 8, 32); err == nil {
		info.Mode = fs.FileMode(m).Perm()
	}
	if info.Type == "file" {
		info.Size, _ = strconv.ParseInt(h.Get("Content-Length"), 10, 64)
	}
	// Go's time.Time.String(), which is what Incus sends.
	info.Modified, _ = time.Parse("2006-01-02 15:04:05 -0700 MST", h.Get("X-Incus-Modified"))
	return info
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
