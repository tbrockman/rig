package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const example = `
host:
  devices:
    gpu:
      kind: gpu
      pci: 0000:2b:00.0
      id: 10de:abcd
      return:
        modules: [nvidia_drm, nvidia_modeset, nvidia_uvm, nvidia]
        reset: true
        alive: drm/card*
    desk-usb:
      kind: pci
      pci: 0000:3c:00.3
      id: 1022:1111
      return: { reset: true, alive: "usb*" }
    mouse:
      kind: usb
      id: 1234:5678
guest:
  name: myproj
  flake: ./guest
  cpus: 8
  memory: 16GiB
  disk: 40GiB
  env_file: ~/.config/rig/myproj.env
  devices: [gpu, desk-usb, mouse]
  ports:
    - "47989:47989/tcp"
    - "47998-48000:47998-48000/udp"
    - "8080:80"
    - { published: 9000, target: 9001, protocol: udp }
  proxy:
    credentials:
      ANTHROPIC_API_KEY: [api.anthropic.com]
`

func TestParseReadsTheExample(t *testing.T) {
	m, err := Parse([]byte(example))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Guest.Name != "myproj" || m.Guest.CPUs.Count != 8 || m.Guest.EnvFile != "~/.config/rig/myproj.env" {
		t.Errorf("guest fields not read: %+v", m.Guest)
	}
	if got := m.ImageAlias(); got != "myproj-guest" {
		t.Errorf("alias for a flake: = %q, want myproj-guest", got)
	}
	wanted := m.Wanted()
	if len(wanted) != 3 || wanted[0].Name != "gpu" || wanted[1].PCI != "0000:3c:00.3" || wanted[2].ID != "1234:5678" {
		t.Errorf("wanted devices wrong: %+v", wanted)
	}
	if wanted[0].Return == nil || wanted[0].Return.Alive != "drm/card*" || len(wanted[0].Return.Modules) != 4 {
		t.Errorf("gpu return recipe not read: %+v", wanted[0].Return)
	}
	want := []string{"47989:47989/tcp", "47998-48000:47998-48000/udp", "8080:80/tcp", "9000:9001/udp"}
	for i, p := range m.Guest.Ports {
		if p.String() != want[i] {
			t.Errorf("port %d = %s, want %s", i, p, want[i])
		}
	}
	if !m.Proxied() || m.Guest.Proxy.Credentials["ANTHROPIC_API_KEY"][0] != "api.anthropic.com" {
		t.Errorf("proxied credentials not read: %+v", m.Guest.Proxy)
	}
}

// A proxied credential needs a file to come from, a network device to reach
// the proxy over, a variable an env file can define, and real host names.
func TestProxyCredentialsAreCheckedAgainstTheRest(t *testing.T) {
	plain := strings.Replace(example, "  proxy:\n    credentials:\n      ANTHROPIC_API_KEY: [api.anthropic.com]\n", "", 1)
	m, err := Parse([]byte(plain))
	if err != nil || m.Proxied() {
		t.Fatalf("without the block: %v %v", err, m.Proxied())
	}
	empty := strings.Replace(example, "ANTHROPIC_API_KEY: [api.anthropic.com]", "", 1)
	if m, err := Parse([]byte(empty)); err != nil || m.Proxied() {
		t.Fatalf("an empty credentials: must read as not proxied: %v", err)
	}
}

// A typo in a key must be an error, not a silently different VM.
func TestParseRejectsUnknownKeys(t *testing.T) {
	bad := strings.Replace(example, "env_file:", "envfile:", 1)
	if _, err := Parse([]byte(bad)); err == nil || !strings.Contains(err.Error(), "envfile") {
		t.Fatalf("unknown key accepted, err = %v", err)
	}
}

// badEdits turn the example into manifests that must be refused. The schema
// test holds the JSON Schema to the same list.
var badEdits = map[string]func(string) string{
	"no image and no flake": func(s string) string { return strings.Replace(s, "flake: ./guest", "", 1) },
	// Compose's build: is a Dockerfile's directory, so it is refused by
	// name rather than read as a flake.
	"compose's build": func(s string) string { return strings.Replace(s, "flake: ./guest", "build: ./guest", 1) },
	"wanted device not declared": func(s string) string {
		return strings.Replace(s, "devices: [gpu, desk-usb, mouse]", "devices: [gpu, webcam]", 1)
	},
	"bad pci address": func(s string) string { return strings.Replace(s, "0000:3c:00.3", "3c:00.3", 1) },
	"usb with a pci": func(s string) string {
		return strings.Replace(s, "id: 1234:5678", "id: 1234:5678\n      pci: 0000:01:00.0", 1)
	},
	"unknown kind":  func(s string) string { return strings.Replace(s, "kind: pci", "kind: xhci", 1) },
	"duplicate pci": func(s string) string { return strings.Replace(s, "0000:3c:00.3", "0000:2b:00.0", 1) },
	"bad port":      func(s string) string { return strings.Replace(s, `"8080:80"`, `"8080:80/sctp"`, 1) },
	"unequal ranges": func(s string) string {
		return strings.Replace(s, `"47998-48000:47998-48000/udp"`, `"47998-48000:47998/udp"`, 1)
	},
	"port out of range": func(s string) string { return strings.Replace(s, `"8080:80"`, `"80800:80"`, 1) },
	"wildcard host ip":  func(s string) string { return strings.Replace(s, `"8080:80"`, `"0.0.0.0:8080:80"`, 1) },
	"bad name":          func(s string) string { return strings.Replace(s, "name: myproj", "name: my proj", 1) },
	"alive is a path":   func(s string) string { return strings.Replace(s, `alive: "usb*"`, `alive: "/sys/usb*"`, 1) },
	// An address alone once handed a VM the host's SATA controller after a
	// BIOS change renumbered the bus; a pci device must say what it is.
	"pci without its identity": func(s string) string { return strings.Replace(s, "      id: 1022:1111\n", "", 1) },
	"malformed identity":       func(s string) string { return strings.Replace(s, "id: 10de:abcd", "id: RTX4080", 1) },
	// A proxied credential comes from the env file and goes over the network.
	"proxy without an env file": func(s string) string { return strings.Replace(s, "  env_file: ~/.config/rig/myproj.env\n", "", 1) },
	"proxy with no network": func(s string) string {
		s = strings.Replace(s, "  ports:\n    - \"47989:47989/tcp\"\n    - \"47998-48000:47998-48000/udp\"\n    - \"8080:80\"\n    - { published: 9000, target: 9001, protocol: udp }\n", "", 1)
		return strings.Replace(s, "  proxy:", "  network: none\n  proxy:", 1)
	},
	"proxied variable that is not one": func(s string) string { return strings.Replace(s, "ANTHROPIC_API_KEY:", "anthropic-key:", 1) },
	"proxied credential with no hosts": func(s string) string { return strings.Replace(s, "[api.anthropic.com]", "[]", 1) },
	"proxied host with a scheme": func(s string) string {
		return strings.Replace(s, "api.anthropic.com]", "https://api.anthropic.com]", 1)
	},
	"proxied endpoint with a query": func(s string) string {
		return strings.Replace(s, "[api.anthropic.com]", `["api.anthropic.com/v1?x=1"]`, 1)
	},
	"proxied endpoint with a lower-case method": func(s string) string {
		return strings.Replace(s, "[api.anthropic.com]", `["post api.anthropic.com"]`, 1)
	},
	"unknown agent": func(s string) string {
		return strings.Replace(s, "  name: myproj\n", "  name: myproj\n  agent: codex\n", 1)
	},
	"proxied host with a port": func(s string) string {
		return strings.Replace(s, "[api.anthropic.com]", "[\"api.anthropic.com:443\"]", 1)
	},
	// egress widens what the proxy dials; it is a word, and it is nothing
	// without a credential to have a proxy for.
	"proxy egress that is not internet": func(s string) string {
		return strings.Replace(s, "  proxy:\n", "  proxy:\n    egress: everywhere\n", 1)
	},
	"proxy egress without a credential": func(s string) string {
		return strings.Replace(s, "    credentials:\n      ANTHROPIC_API_KEY: [api.anthropic.com]\n", "    egress: internet\n", 1)
	},
}

func TestParseRejectsWhatWouldFailLater(t *testing.T) {
	for name, edit := range badEdits {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(edit(example))); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestParsePortShortForms(t *testing.T) {
	for in, want := range map[string]string{
		"80":                  "80:80/tcp",
		"8080:80":             "8080:80/tcp",
		"53:53/udp":           "53:53/udp",
		"5000-5002:6000-6002": "5000-5002:6000-6002/tcp",
		"192.168.1.2:8080:80": "192.168.1.2:8080:80/tcp",
	} {
		p, err := ParsePort(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if p.String() != want {
			t.Errorf("%q -> %s, want %s", in, p, want)
		}
	}
	lo, hi := mustPort(t, "5000-5002:6000-6002").TargetRange()
	if lo != 6000 || hi != 6002 {
		t.Errorf("target range = %d-%d", lo, hi)
	}
}

func mustPort(t *testing.T, s string) Port {
	t.Helper()
	p, err := ParsePort(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Relative paths in the file mean "next to the file", whatever directory rig
// was run from — the same rule Compose applies.
func TestLoadResolvesPathsAgainstTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rig.yaml")
	if err := os.WriteFile(path, []byte(example), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if ref, attr := m.FlakeRef(); ref != filepath.Join(dir, "guest") || attr != "guest" {
		t.Errorf("flake resolved to %q #%s", ref, attr)
	}
	if got := m.Resolve("/abs/path"); got != "/abs/path" {
		t.Errorf("absolute path was rewritten to %q", got)
	}
}

func TestExclusiveKinds(t *testing.T) {
	if !Exclusive(KindGPU) || !Exclusive(KindPCI) || Exclusive(KindUSB) {
		t.Error("gpu and pci are exclusive, usb is not")
	}
}

func TestNetworkIsTheDefaultOrNone(t *testing.T) {
	base := "guest:\n  name: daw\n  image: x\n"
	if _, err := Parse([]byte(base + "  network: none\n")); err != nil {
		t.Fatalf("network: none must parse: %v", err)
	}
	if _, err := Parse([]byte(base + "  network: bridged\n")); err == nil {
		t.Fatal("an unknown network value must be refused, not treated as the default")
	}
	if _, err := Parse([]byte(base + "  network: none\n  ports: [\"8080:80/tcp\"]\n")); err == nil {
		t.Fatal("ports on a VM with no network device must be refused")
	}
}

// A card may leave its identity out: the start-time check then falls back to
// "an NVIDIA display controller", which is all rig supports anyway.
func TestGPUIdentityIsOptional(t *testing.T) {
	m, err := Parse([]byte(strings.Replace(example, "      id: 10de:abcd\n", "", 1)))
	if err != nil {
		t.Fatalf("a gpu without id must parse: %v", err)
	}
	if got := m.Host.Devices["gpu"].ID; got != "" {
		t.Fatalf("id = %q, want empty", got)
	}
}

func TestCPUsIsACountOrASet(t *testing.T) {
	parse := func(v string) (*Manifest, error) {
		return Parse([]byte("guest:\n  name: daw\n  image: x\n  cpus: " + v + "\n"))
	}
	if m, err := parse("8"); err != nil || m.Guest.CPUs.Limit() != "8" {
		t.Fatalf("count: %v %v", m, err)
	}
	if m, err := parse(`"4-7,12-15"`); err != nil || m.Guest.CPUs.Limit() != "4-7,12-15" || m.Guest.CPUs.Count != 0 {
		t.Fatalf("set: %+v %v", m, err)
	}
	for _, bad := range []string{"0", "-2", `"4-7;12"`, `"7-4"`, `"all"`} {
		if _, err := parse(bad); err == nil {
			t.Errorf("cpus %s was accepted", bad)
		}
	}
}

// Named volumes only: a host path on the left would put a directory of this
// host inside the guest.
func TestVolumesAreNamedAndNeverHostPaths(t *testing.T) {
	parse := func(v string) (*Manifest, error) {
		return Parse([]byte("guest:\n  name: daw\n  image: x\n  volumes:\n" + v))
	}
	m, err := parse("    - daw-docs:/home/me/Documents\n    - { source: samples, target: /srv/samples, size: 50GiB, owner: \"1000:100\" }\n")
	if err != nil {
		t.Fatal(err)
	}
	if v := m.Guest.Volumes; len(v) != 2 || v[0].Name != "daw-docs" || v[0].Path != "/home/me/Documents" || v[1].Size != "50GiB" || v[1].Owner != "1000:100" {
		t.Fatalf("volumes %+v", v)
	}
	for name, bad := range map[string]string{
		"host path":       "    - /home/me:/mnt\n",
		"relative path":   "    - ./data:/mnt\n",
		"home path":       "    - ~/data:/mnt\n",
		"relative target": "    - data:mnt\n",
		"root target":     "    - data:/\n",
		"bad owner":       "    - { source: data, target: /mnt, owner: theo }\n",
		"duplicate":       "    - data:/a\n    - data:/b\n",
		"no target":       "    - data\n",
	} {
		if _, err := parse(bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// env_file: ~/... names a file outside the project, where credentials belong;
// a relative path stays relative to the manifest.
func TestResolveExpandsHome(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	m := &Manifest{Dir: "/work/proj"}
	if got := m.Resolve("~/.config/rig/proj.env"); got != "/home/me/.config/rig/proj.env" {
		t.Errorf("~/ path: got %s", got)
	}
	if got := m.Resolve("guest"); got != "/work/proj/guest" {
		t.Errorf("relative path: got %s", got)
	}
}

func TestInputIsHostOrNothing(t *testing.T) {
	base := "guest:\n  name: daw\n  image: x\n"
	if m, err := Parse([]byte(base + "  input: host\n")); err != nil || m.Guest.Input != InputHost {
		t.Fatalf("input: host must parse: %v", err)
	}
	if _, err := Parse([]byte(base + "  input: passthrough\n")); err == nil {
		t.Fatal("an unknown input value must be refused, not ignored")
	}
}

// flake: follows nixos-rebuild's dir#name, and a reference with a scheme is
// nix's to resolve, not the manifest's.
func TestFlakeRefSplitsTheName(t *testing.T) {
	for in, want := range map[string][2]string{
		"./guest":                     {"/work/proj/guest", "guest"},
		"./guest#daw":                 {"/work/proj/guest", "daw"},
		"#daw":                        {"/work/proj", "daw"},
		"github:me/vms?dir=guest#daw": {"github:me/vms?dir=guest", "daw"},
		"path:/abs/guest":             {"path:/abs/guest", "guest"},
	} {
		m := &Manifest{Dir: "/work/proj", Guest: Guest{Flake: in}}
		if ref, attr := m.FlakeRef(); ref != want[0] || attr != want[1] {
			t.Errorf("%s: got %s #%s, want %s #%s", in, ref, attr, want[0], want[1])
		}
	}
}

// The endpoint grammar: a host alone, methods and a path to narrow it, a
// wildcard for a site; rendered back canonically.
func TestEndpointGrammar(t *testing.T) {
	good := map[string]Endpoint{
		"api.anthropic.com":                        {Host: "api.anthropic.com"},
		"POST api.anthropic.com/v1/messages":       {Host: "api.anthropic.com", Methods: []string{"POST"}, Path: "/v1/messages"},
		"GET,HEAD *.example.com/":                  {Host: "*.example.com", Methods: []string{"GET", "HEAD"}},
		"  api.example.com/v1/messages/batches/  ": {Host: "api.example.com", Path: "/v1/messages/batches"},
	}
	for in, want := range good {
		got, err := ParseEndpoint(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got.Host != want.Host || got.Path != want.Path || strings.Join(got.Methods, ",") != strings.Join(want.Methods, ",") {
			t.Errorf("%q parsed as %+v, want %+v", in, got, want)
		}
		if again, _ := ParseEndpoint(got.String()); again.String() != got.String() {
			t.Errorf("%q does not round-trip: %q -> %q", in, got.String(), again.String())
		}
	}
	for _, bad := range []string{"", "localhost", "https://api.example.com", "api.example.com:443", "post api.example.com",
		"api.example.com/v1?x=1", "*.example.com*", "api.example.com v1", "POST,", "POST  api.example.com"} {
		if _, err := ParseEndpoint(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// An agent the manifest names must be one rig runs; leaving it out is the
// first harness.
func TestAgentIsAKnownHarness(t *testing.T) {
	m, err := Parse([]byte("guest:\n  name: a\n  image: rig-base\n  agent: claude-code\n"))
	if err != nil || m.Guest.Agent != "claude-code" {
		t.Fatalf("agent: %v %+v", err, m)
	}
	if Harnesses[0] != "claude-code" {
		t.Errorf("the default harness is %q", Harnesses[0])
	}
}

// guest.proxy.egress is internet or left out, and only beside a credential:
// the proxy exists for one.
func TestProxyEgressIsInternetOrNothing(t *testing.T) {
	m, err := Parse([]byte(example))
	if err != nil || m.ProxyEgress() != "" {
		t.Fatalf("left out: %v %q", err, m.ProxyEgress())
	}
	with := strings.Replace(example, "  proxy:\n", "  proxy:\n    egress: internet\n", 1)
	m, err = Parse([]byte(with))
	if err != nil || m.ProxyEgress() != EgressInternet {
		t.Fatalf("egress: internet: %v %q", err, m.ProxyEgress())
	}
}
