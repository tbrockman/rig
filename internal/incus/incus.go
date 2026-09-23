package incus

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Device is Incus's untyped string map. Keys are things like "type", "pci",
// "security.acls".
type Device map[string]string

func (d Device) Type() string { return d["type"] }

type Instance struct {
	Name            string            `json:"name"`
	Status          string            `json:"status"`
	Architecture    string            `json:"architecture"`
	Description     string            `json:"description"`
	Ephemeral       bool              `json:"ephemeral"`
	Config          map[string]string `json:"config"`
	Devices         map[string]Device `json:"devices"`
	ExpandedDevices map[string]Device `json:"expanded_devices"`
	Profiles        []string          `json:"profiles"`
}

func (i *Instance) Running() bool { return i.Status == "Running" }
func (i *Instance) Stopped() bool { return i.Status == "Stopped" }

// GPUDevices returns GPU devices defined on the instance itself, not inherited.
func (i *Instance) GPUDevices() map[string]Device {
	out := map[string]Device{}
	for name, d := range i.Devices {
		if d.Type() == "gpu" {
			out[name] = d
		}
	}
	return out
}

// PassthroughDevices returns the host devices configured on the instance
// itself: GPUs, other PCI functions, and USB devices. Not inherited ones — a
// passthrough device in a profile is a misconfiguration reported separately.
func (i *Instance) PassthroughDevices() map[string]Device {
	out := map[string]Device{}
	for name, d := range i.Devices {
		switch d.Type() {
		case "gpu", "pci", "usb":
			out[name] = d
		}
	}
	return out
}

// NICs returns the instance's effective NICs. Expanded, because the isolation
// ACL is meant to come from a profile.
func (i *Instance) NICs() map[string]Device {
	out := map[string]Device{}
	for name, d := range i.ExpandedDevices {
		if d.Type() == "nic" {
			out[name] = d
		}
	}
	return out
}

type Profile struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Config      map[string]string `json:"config"`
	Devices     map[string]Device `json:"devices"`
	UsedBy      []string          `json:"used_by"`
}

type ACLRule struct {
	Action          string `json:"action"`
	Destination     string `json:"destination,omitempty"`
	Source          string `json:"source,omitempty"`
	Protocol        string `json:"protocol,omitempty"`
	DestinationPort string `json:"destination_port,omitempty"` // "80,443,5000-5010"
	State           string `json:"state,omitempty"`
}

type ACL struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Ingress     []ACLRule         `json:"ingress"`
	Egress      []ACLRule         `json:"egress"`
	Config      map[string]string `json:"config"`
	UsedBy      []string          `json:"used_by"`
}

// --- instances -----------------------------------------------------------

func (c *Client) Instances() ([]Instance, error) {
	var out []Instance
	_, err := c.Get("/1.0/instances?recursion=1", &out)
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, err
}

func (c *Client) Instance(name string) (*Instance, string, error) {
	var inst Instance
	etag, err := c.Get("/1.0/instances/"+url.PathEscape(name), &inst)
	if err != nil {
		return nil, "", err
	}
	return &inst, etag, nil
}

func (c *Client) Exists(name string) bool {
	_, _, err := c.Instance(name)
	return err == nil
}

type CreateOpts struct {
	Name     string
	Image    string
	Profile  string // the profile the VM inherits from; empty means "default"
	CPUs     int
	CPUSet   string // host CPUs to pin to, e.g. "4-7,12-15"; wins over CPUs
	Memory   string
	DiskSize string
	Config   map[string]string
}

func (c *Client) CreateVM(o CreateOpts) error {
	config := map[string]string{
		// NixOS images built here are not signed for secure boot.
		"security.secureboot": "false",
	}
	if o.CPUSet != "" {
		config["limits.cpu"] = o.CPUSet
	} else if o.CPUs > 0 {
		config["limits.cpu"] = fmt.Sprint(o.CPUs)
	}
	if o.Memory != "" {
		config["limits.memory"] = o.Memory
	}
	for k, v := range o.Config {
		config[k] = v
	}

	profile := o.Profile
	if profile == "" {
		profile = "default"
	}
	body := map[string]any{
		"name":     o.Name,
		"type":     "virtual-machine",
		"config":   config,
		"profiles": []string{profile},
		"source":   map[string]string{"type": "image", "alias": o.Image},
	}
	// A device override replaces the whole device, so resizing the root disk
	// means copying the profile's entry and changing one key. (The CLI merges
	// for you; the API does not, and the resulting error names only "pool".)
	if o.DiskSize != "" {
		root, err := c.profileRootDisk(profile)
		if err != nil {
			return err
		}
		root["size"] = o.DiskSize
		body["devices"] = map[string]Device{"root": root}
	}
	return c.Post("/1.0/instances", body, nil)
}

func (c *Client) profileRootDisk(profile string) (Device, error) {
	p, _, err := c.Profile(profile)
	if err != nil {
		return nil, err
	}
	for _, dev := range p.Devices {
		if dev.Type() == "disk" && dev["path"] == "/" {
			out := Device{}
			for k, v := range dev {
				out[k] = v
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("profile %q has no root disk to base the size on", profile)
}

func (c *Client) DeleteInstance(name string) error {
	return c.Delete("/1.0/instances/" + url.PathEscape(name))
}

// SetDevices round-trips the instance with its ETag. Callers hold a file lock
// against themselves; the ETag catches anyone editing out of band.
func (c *Client) SetDevices(name string, devices map[string]Device) error {
	inst, etag, err := c.Instance(name)
	if err != nil {
		return err
	}
	body := map[string]any{
		"architecture": inst.Architecture,
		"config":       inst.Config,
		"devices":      devices,
		"ephemeral":    inst.Ephemeral,
		"profiles":     inst.Profiles,
		"description":  inst.Description,
	}
	return c.PutWithETag("/1.0/instances/"+url.PathEscape(name), body, etag)
}

func (c *Client) SetConfigKey(name, key, value string) error {
	inst, etag, err := c.Instance(name)
	if err != nil {
		return err
	}
	config := map[string]string{}
	for k, v := range inst.Config {
		config[k] = v
	}
	config[key] = value
	body := map[string]any{
		"architecture": inst.Architecture,
		"config":       config,
		"devices":      inst.Devices,
		"ephemeral":    inst.Ephemeral,
		"profiles":     inst.Profiles,
		"description":  inst.Description,
	}
	return c.PutWithETag("/1.0/instances/"+url.PathEscape(name), body, etag)
}

func (c *Client) SetState(name, action string, timeoutSec int) error {
	return c.setState(name, action, timeoutSec, false)
}

// ForceStop pulls the plug: no ACPI request, no guest cooperation. For a guest
// that ignores the power button, which a desktop session will do.
func (c *Client) ForceStop(name string, timeoutSec int) error {
	return c.setState(name, "stop", timeoutSec, true)
}

func (c *Client) setState(name, action string, timeoutSec int, force bool) error {
	return c.Put("/1.0/instances/"+url.PathEscape(name)+"/state", map[string]any{
		"action":  action,
		"timeout": timeoutSec,
		"force":   force,
	})
}

type instanceState struct {
	Network map[string]struct {
		Hwaddr    string `json:"hwaddr"`
		Addresses []struct {
			Family  string `json:"family"`
			Address string `json:"address"`
			Scope   string `json:"scope"`
		} `json:"addresses"`
	} `json:"network"`
}

// GlobalIPv4 returns the address on the instance's own NIC.
//
// It matches by MAC rather than taking the first global address it finds.
// `Network` is a map, so Go iterates it in random order, and a guest has more
// global IPv4 addresses than its NIC as soon as it runs anything that makes a
// bridge — docker being the obvious one. The old version was deterministic only
// while there was exactly one candidate; the moment there were two it started
// reporting a docker bridge as the guest's address, intermittently, which is
// the worst way for an address to be wrong. Incus records the NIC's MAC in
// `volatile.<device>.hwaddr`, and that is the one unambiguous link between the
// device rig configured and the interface the guest brought up under whatever
// name the kernel chose.
func (c *Client) GlobalIPv4(name string) (string, error) {
	inst, _, err := c.Instance(name)
	if err != nil {
		return "", err
	}
	var st instanceState
	if _, err := c.Get("/1.0/instances/"+url.PathEscape(name)+"/state", &st); err != nil {
		return "", err
	}

	// The MACs of every NIC this instance is configured with.
	wanted := map[string]bool{}
	for dev := range inst.NICs() {
		if mac := inst.Config["volatile."+dev+".hwaddr"]; mac != "" {
			wanted[strings.ToLower(mac)] = true
		}
	}

	var fallback string
	for _, iface := range st.Network {
		for _, a := range iface.Addresses {
			if a.Family != "inet" || a.Scope != "global" {
				continue
			}
			if wanted[strings.ToLower(iface.Hwaddr)] {
				return a.Address, nil
			}
			// Deterministic, so a guest whose MACs cannot be matched at least
			// reports the same wrong answer twice rather than a different one
			// each call.
			if fallback == "" || a.Address < fallback {
				fallback = a.Address
			}
		}
	}
	return fallback, nil
}

// ConsoleLog returns the instance's console log.
//
// Incus 6.0 serves it with type=log and, unlike every other endpoint, as the
// raw text rather than inside the JSON envelope. Only a failure comes back
// as an envelope, so the body is tried as one first.
func (c *Client) ConsoleLog(name string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, "http://incus/1.0/instances/"+url.PathEscape(name)+"/console?type=log", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach incus at %s: %w%s", c.socket, err, socketHint(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var env envelope
	if json.Unmarshal(raw, &env) == nil && env.Type == "error" {
		return "", &APIError{Code: env.ErrorCode, Message: env.Error}
	}
	return string(raw), nil
}

// --- profiles ------------------------------------------------------------

func (c *Client) Profiles() ([]Profile, error) {
	var out []Profile
	_, err := c.Get("/1.0/profiles?recursion=1", &out)
	return out, err
}

func (c *Client) Profile(name string) (*Profile, string, error) {
	var p Profile
	etag, err := c.Get("/1.0/profiles/"+url.PathEscape(name), &p)
	if err != nil {
		return nil, "", err
	}
	return &p, etag, nil
}

func (c *Client) CreateProfile(name string, devices map[string]Device) error {
	return c.Post("/1.0/profiles", map[string]any{
		"name": name, "devices": devices, "config": map[string]string{},
	}, nil)
}

func (c *Client) DeleteProfile(name string) error {
	return c.Delete("/1.0/profiles/" + url.PathEscape(name))
}

// SetProfiles replaces the list of profiles an instance inherits from.
func (c *Client) SetProfiles(name string, profiles []string) error {
	inst, etag, err := c.Instance(name)
	if err != nil {
		return err
	}
	return c.PutWithETag("/1.0/instances/"+url.PathEscape(name), map[string]any{
		"architecture": inst.Architecture,
		"config":       inst.Config,
		"devices":      inst.Devices,
		"ephemeral":    inst.Ephemeral,
		"profiles":     profiles,
		"description":  inst.Description,
	}, etag)
}

func (c *Client) SetProfileDevices(name string, p *Profile, devices map[string]Device, etag string) error {
	return c.PutWithETag("/1.0/profiles/"+url.PathEscape(name), map[string]any{
		"config":      p.Config,
		"description": p.Description,
		"devices":     devices,
	}, etag)
}

// --- network ACLs --------------------------------------------------------

func (c *Client) ACLNames() ([]string, error) {
	var urls []string
	if _, err := c.Get("/1.0/network-acls", &urls); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(urls))
	for _, u := range urls {
		names = append(names, u[strings.LastIndex(u, "/")+1:])
	}
	sort.Strings(names)
	return names, nil
}

func (c *Client) ACL(name string) (*ACL, string, error) {
	var a ACL
	etag, err := c.Get("/1.0/network-acls/"+url.PathEscape(name), &a)
	if err != nil {
		return nil, "", err
	}
	return &a, etag, nil
}

func (c *Client) CreateACL(a *ACL) error {
	return c.Post("/1.0/network-acls", a, nil)
}

func (c *Client) PutACL(name string, a *ACL, etag string) error {
	return c.PutWithETag("/1.0/network-acls/"+url.PathEscape(name), a, etag)
}

func (c *Client) DeleteACL(name string) error {
	return c.Delete("/1.0/network-acls/" + url.PathEscape(name))
}

// --- networks ------------------------------------------------------------

type Network struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Config map[string]string `json:"config"`
}

func (c *Client) Network(name string) (*Network, error) {
	var n Network
	if _, err := c.Get("/1.0/networks/"+url.PathEscape(name), &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// --- custom storage volumes -------------------------------------------------

// CustomVolumeExists reports whether a custom volume is in a pool.
func (c *Client) CustomVolumeExists(pool, name string) bool {
	var v map[string]any
	_, err := c.Get("/1.0/storage-pools/"+url.PathEscape(pool)+"/volumes/custom/"+url.PathEscape(name), &v)
	return err == nil
}

// CreateCustomVolume creates a filesystem custom volume in a pool.
func (c *Client) CreateCustomVolume(pool, name string, config map[string]string) error {
	if config == nil {
		config = map[string]string{}
	}
	body := map[string]any{"name": name, "type": "custom", "content_type": "filesystem", "config": config}
	return c.Post("/1.0/storage-pools/"+url.PathEscape(pool)+"/volumes/custom", body, nil)
}

// DeleteCustomVolume deletes a custom volume and everything on it.
func (c *Client) DeleteCustomVolume(pool, name string) error {
	return c.Delete("/1.0/storage-pools/" + url.PathEscape(pool) + "/volumes/custom/" + url.PathEscape(name))
}
