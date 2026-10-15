package proxy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

// --- the management API client ----------------------------------------------

// Client talks to one instance's management API, under that instance's own
// CA and with a token from rig's login.
type Client struct {
	server string
	token  string
	http   *http.Client
}

// httpClient trusts caPEM and nothing else for the server: the API is HTTPS
// under the instance's own CA.
func httpClient(caPEM []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the instance's ca.crt is not a certificate")
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    pool,
		}},
	}, nil
}

// NewClient is a client for server with token, trusting caPEM.
func NewClient(server, token string, caPEM []byte) (*Client, error) {
	hc, err := httpClient(caPEM)
	if err != nil {
		return nil, err
	}
	return &Client{server: strings.TrimRight(server, "/"), token: token, http: hc}, nil
}

// Login gets a token from the instance's own login endpoint, for the account
// rig made at install.
func Login(server string, caPEM []byte, email, password string) (string, error) {
	hc, err := httpClient(caPEM)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	resp, err := hc.Post(strings.TrimRight(server, "/")+"/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("the proxy's API at %s is not answering: %w", server, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", &Error{Method: http.MethodPost, Path: "/api/auth/login", Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Token == "" {
		return "", errors.New("the proxy's login answered without a token")
	}
	return out.Token, nil
}

// Error is a failed API call, with what the server said.
type Error struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("witmproxy: %s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		msg += "\n  The proxy does not accept rig's login. Its account was made at install; rig rm <vm> and rig start again makes a new instance"
	}
	return msg
}

func (c *Client) do(method, path string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequest(method, c.server+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("the proxy's API at %s is not answering: %w\n  Is its unit running? (rig proxy status)", c.server, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &Error{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("witmproxy: %s %s: unreadable answer: %w", method, path, err)
		}
	}
	return nil
}

func (c *Client) get(path string, out any) error { return c.do(http.MethodGet, path, nil, "", out) }

func (c *Client) sendJSON(method, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	return c.do(method, path, bytes.NewReader(b), "application/json", out)
}

// Server is where the client talks to.
func (c *Client) Server() string { return c.server }

// Health is whether the daemon answers at all.
func (c *Client) Health() error {
	return c.do(http.MethodGet, "/api/health", nil, "", nil)
}

// PluginInfo is a plugin as witmproxy lists it.
type PluginInfo struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Enabled   bool   `json:"enabled"`
}

// Plugins lists what is installed.
func (c *Client) Plugins() ([]PluginInfo, error) {
	var out []PluginInfo
	if err := c.get("/api/plugins", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RigPlugin is rig's plugin as installed, or nil.
func (c *Client) RigPlugin() (*PluginInfo, error) {
	plugins, err := c.Plugins()
	if err != nil {
		return nil, err
	}
	for i := range plugins {
		if plugins[i].Namespace == PluginNamespace && plugins[i].Name == PluginName {
			return &plugins[i], nil
		}
	}
	return nil, nil
}

// InstallPlugin uploads the component, replacing any version installed, and
// makes sure it is on: this instance serves one guest, and the plugin is
// for that guest.
func (c *Client) InstallPlugin(wasm []byte) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "rig.signed.wasm")
	if err != nil {
		return err
	}
	if _, err := part.Write(wasm); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/plugins", &body, w.FormDataContentType(), nil); err != nil {
		return err
	}
	return c.SetPluginEnabled(true)
}

// SetPluginEnabled toggles the plugin for the instance.
func (c *Client) SetPluginEnabled(enabled bool) error {
	return c.sendJSON(http.MethodPut, "/api/plugins/"+PluginNamespace+"/"+PluginName+"/enabled",
		map[string]bool{"enabled": enabled}, nil)
}

// SetRules hands the plugin the guest's rules: its configuration for the
// whole instance, which is this guest's.
func (c *Client) SetRules(rulesJSON string) error {
	return c.sendJSON(http.MethodPut, "/api/plugins/"+PluginNamespace+"/"+PluginName+"/config",
		map[string]any{"config": map[string]string{RulesInput: rulesJSON}}, nil)
}

// ReadCA reads an instance's root certificate from its state directory,
// where the daemon wrote it on its first start.
func (i Instance) ReadCA() ([]byte, error) {
	path, err := i.CAFile()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the proxy's certificate is not at %s: has it started? (%w)", path, err)
	}
	return b, nil
}

// Connect logs rig in to the instance and returns a client for it.
func (i Instance) Connect() (*Client, error) {
	ca, err := i.ReadCA()
	if err != nil {
		return nil, err
	}
	password, err := i.AdminPassword()
	if err != nil {
		return nil, err
	}
	token, err := Login(i.Server(), ca, AdminEmail, password)
	if err != nil {
		return nil, err
	}
	return NewClient(i.Server(), token, ca)
}
