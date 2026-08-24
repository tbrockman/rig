package incus

import (
	"fmt"
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
	Action      string `json:"action"`
	Destination string `json:"destination,omitempty"`
	Source      string `json:"source,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	State       string `json:"state,omitempty"`
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
	CPUs     int
	Memory   string
	DiskSize string
	Config   map[string]string
}

func (c *Client) CreateVM(o CreateOpts) error {
	config := map[string]string{
		// NixOS images built here are not signed for secure boot.
		"security.secureboot": "false",
	}
	if o.CPUs > 0 {
		config["limits.cpu"] = fmt.Sprint(o.CPUs)
	}
	if o.Memory != "" {
		config["limits.memory"] = o.Memory
	}
	for k, v := range o.Config {
		config[k] = v
	}

	body := map[string]any{
		"name":   o.Name,
		"type":   "virtual-machine",
		"config": config,
		"source": map[string]string{"type": "image", "alias": o.Image},
	}
	// A device override replaces the whole device, so resizing the root disk
	// means copying the profile's entry and changing one key. (The CLI merges
	// for you; the API does not, and the resulting error names only "pool".)
	if o.DiskSize != "" {
		root, err := c.profileRootDisk("default")
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
	return c.Put("/1.0/instances/"+url.PathEscape(name)+"/state", map[string]any{
		"action":  action,
		"timeout": timeoutSec,
	})
}

type instanceState struct {
	Network map[string]struct {
		Addresses []struct {
			Family  string `json:"family"`
			Address string `json:"address"`
			Scope   string `json:"scope"`
		} `json:"addresses"`
	} `json:"network"`
}

// GlobalIPv4 returns the first global IPv4 address on any interface, or "".
func (c *Client) GlobalIPv4(name string) (string, error) {
	var st instanceState
	if _, err := c.Get("/1.0/instances/"+url.PathEscape(name)+"/state", &st); err != nil {
		return "", err
	}
	for _, iface := range st.Network {
		for _, a := range iface.Addresses {
			if a.Family == "inet" && a.Scope == "global" {
				return a.Address, nil
			}
		}
	}
	return "", nil
}

func (c *Client) ConsoleLog(name string) (string, error) {
	env, _, err := c.call("GET", "/1.0/instances/"+url.PathEscape(name)+"/console?type=console", nil, "")
	if err != nil {
		return "", err
	}
	return string(env.Metadata), nil
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

// --- images --------------------------------------------------------------

func (c *Client) ImageExists(alias string) bool {
	_, err := c.Get("/1.0/images/aliases/"+url.PathEscape(alias), nil)
	return err == nil
}
