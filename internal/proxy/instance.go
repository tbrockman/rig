package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tbrockman/rig/internal/manifest"
	"github.com/tbrockman/rig/internal/policy"
)

// --- one guest's witmproxy -----------------------------------------------------

// An Instance is one guest's witmproxy: a user-scope witmproxy instance named
// for the guest, installed and run by witmproxy's own CLI as a unit of the
// operator's systemd user manager, with its state under the operator's home.
// Its proxy listens on the bridge address at the guest's port and its
// management API on the host's loopback at the guest's API port; both
// follow from the guest's address, so an Instance is built from the record
// and nothing is stored for it.
//
// The instance's lifetime is the guest's: made at the first start that
// proxies credentials, stopped with the VM, removed with it.
type Instance struct {
	VM string
	IP string
}

// InstanceFor is the proxy of the guest vm at ip on the bridge.
func InstanceFor(vm, ip string) Instance { return Instance{VM: vm, IP: ip} }

// instancePrefix keeps rig's instances apart from the operator's own in
// `witm service list --user`.
const instancePrefix = "rig-"

// AdminEmail is the one account in a guest's proxy: rig's.
const AdminEmail = "rig@localhost"

// Name is the witmproxy instance name: rig-<vm>.
func (i Instance) Name() string { return instancePrefix + i.VM }

// VMOfInstance reads a guest's name back out of an instance name; false for
// an instance that is not rig's.
func VMOfInstance(name string) (string, bool) {
	return strings.CutPrefix(name, instancePrefix)
}

// Unit is the systemd user unit witmproxy installs for it.
func (i Instance) Unit() string { return "ez-witmproxy@" + i.Name() + ".service" }

// ProxyPort is where the guest reaches it, on the bridge address.
func (i Instance) ProxyPort() int { return policy.ProxyPortOf(i.IP) }

// APIPort is where rig reaches its management API, on loopback.
func (i Instance) APIPort() int { return policy.APIPortOf(i.IP) }

// ProxyURL is what the guest's HTTPS_PROXY names.
func (i Instance) ProxyURL() string { return policy.ProxyURLFor(i.IP) }

// ProxyBind and APIBind are the addresses witmproxy is told to listen on.
func (i Instance) ProxyBind() string { return fmt.Sprintf("%s:%d", policy.ProxyAddr, i.ProxyPort()) }
func (i Instance) APIBind() string   { return fmt.Sprintf("127.0.0.1:%d", i.APIPort()) }

// Server is the management API's URL.
func (i Instance) Server() string { return "https://" + i.APIBind() }

// StateHome is where a user's witmproxy keeps its instances: $XDG_STATE_HOME,
// else ~/.local/state. The same rule witmproxy applies, so rig reads the
// files witmproxy writes without asking it where they are.
func StateHome() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state"), nil
}

// Root is the instance's state directory: its config, policy, database, CA
// and log.
func (i Instance) Root() (string, error) {
	state, err := StateHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "witmproxy", "instances", i.Name()), nil
}

func (i Instance) under(parts ...string) (string, error) {
	root, err := i.Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{root}, parts...)...), nil
}

// CAFile is the instance's root certificate, which its API serves under and
// which it mints the guests' interception certificates with. Written by the
// daemon on its first start.
func (i Instance) CAFile() (string, error) { return i.under("certs", "ca.crt") }

// ConfigFile is witmproxy's own configuration for the instance, which holds
// the admin password rig gave it at install.
func (i Instance) ConfigFile() (string, error) { return i.under("config.toml") }

// PolicyFile is the instance's egress policy, written by rig.
func (i Instance) PolicyFile() (string, error) { return i.under("policy.toml") }

// UnitFile is where witmproxy writes the user unit.
func (i Instance) UnitFile() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", i.Unit()), nil
}

// Installed is whether witmproxy has installed the unit.
func (i Instance) Installed() bool {
	path, err := i.UnitFile()
	return err == nil && fileExists(path)
}

// InstalledVMs names every guest that has an instance directory, whether or
// not the guest still exists: rig rm takes the instance away with the VM, so
// one without a VM was deleted some other way.
func InstalledVMs() []string {
	state, err := StateHome()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(state, "witmproxy", "instances"))
	if err != nil {
		return nil
	}
	var vms []string
	for _, e := range entries {
		if vm, ok := VMOfInstance(e.Name()); ok && e.IsDir() {
			vms = append(vms, vm)
		}
	}
	sort.Strings(vms)
	return vms
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// --- witm, witmproxy's CLI -----------------------------------------------------

// ErrNoWitm is what every proxy verb reports when witmproxy is not installed.
var ErrNoWitm = errors.New("no `witm` on PATH: witmproxy is not installed on this host.\n  cargo install witmproxy   # or a release binary; docs/RUNBOOK.md")

// Witm is the CLI's path.
func Witm() (string, error) {
	path, err := exec.LookPath("witm")
	if err != nil {
		return "", ErrNoWitm
	}
	return path, nil
}

// WitmHasUserScope is whether this witm knows `--user`, which the per-guest
// instances need: an older one installs system services only.
func WitmHasUserScope() error {
	witm, err := Witm()
	if err != nil {
		return err
	}
	out, _ := exec.Command(witm, "service", "install", "--help").CombinedOutput()
	if !bytes.Contains(out, []byte("--user")) {
		return errors.New("this witm has no --user scope; rig's per-guest proxies need a witmproxy that does (cargo install witmproxy, or a newer release)")
	}
	return nil
}

// witm is a `witm service <verb>` for this instance, in the user scope.
func (i Instance) witm(verb string, args ...string) (*exec.Cmd, error) {
	witm, err := Witm()
	if err != nil {
		return nil, err
	}
	all := append([]string{"service", verb, "--user", "--instance", i.Name()}, args...)
	return exec.Command(witm, all...), nil
}

// run runs a witm command quietly; its output is the error's when it fails.
func run(cmd *exec.Cmd) error {
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", strings.Join(cmd.Args[1:], " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// NewPassword is the admin password rig gives a new instance: 32 hex digits,
// kept by witmproxy in the instance's config (0600, the operator's) and read
// back from there, so there is one copy.
func NewPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Install makes the instance: witmproxy's installer, in the user scope,
// bound to the guest's ports, with rig's account. The password goes in the
// environment, never on the command line. Does not start it.
func (i Instance) Install(password string) error {
	root, err := i.Root()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	cmd, err := i.witm("install",
		"--proxy-bind-addr", i.ProxyBind(),
		"--web-bind-addr", i.APIBind(),
		"--auth-admin-email", AdminEmail,
		"-y")
	if err != nil {
		return err
	}
	cmd.Env = append(os.Environ(), "AUTH_ADMIN_PASSWORD="+password)
	return run(cmd)
}

// Start, Stop and Restart drive the unit through witm, which waits for the
// daemon to answer (or to exit).
func (i Instance) Start() error {
	cmd, err := i.witm("start")
	if err != nil {
		return err
	}
	return run(cmd)
}

func (i Instance) Stop() error {
	cmd, err := i.witm("stop")
	if err != nil {
		return err
	}
	return run(cmd)
}

func (i Instance) Restart() error {
	cmd, err := i.witm("restart")
	if err != nil {
		return err
	}
	return run(cmd)
}

// Uninstall removes the unit and then the instance's state: its database,
// with the credentials it held, its CA and its log. The guest it served is
// gone or going.
func (i Instance) Uninstall() error {
	if i.Installed() {
		cmd, err := i.witm("uninstall", "-y")
		if err != nil {
			return err
		}
		if err := run(cmd); err != nil {
			return err
		}
	}
	root, err := i.Root()
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}

// Logs attaches `witm service logs` for the instance to the terminal.
func (i Instance) Logs(follow bool, lines int) error {
	args := []string{"--lines", fmt.Sprint(lines)}
	if follow {
		args = append(args, "--follow")
	}
	cmd, err := i.witm("logs", args...)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

// Running asks the user manager whether the unit is active.
func (i Instance) Running() bool {
	out, _ := exec.Command("systemctl", "--user", "is-active", i.Unit()).Output()
	return strings.TrimSpace(string(out)) == "active"
}

// Linger reports whether the operator's user manager keeps running without a
// session, which is what lets the guests' proxies outlive a logout and start
// at boot. known is false when loginctl cannot say.
func Linger() (enabled, known bool) {
	out, err := exec.Command("loginctl", "show-user", fmt.Sprint(os.Getuid()), "--property=Linger", "--value").Output()
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(string(out)) {
	case "yes":
		return true, true
	case "no":
		return false, true
	}
	return false, false
}

// EnableLinger asks logind to keep the operator's user manager running
// without a session. On systemd 256 and later a user may linger themself
// (polkit's set-self-linger allows it without authentication), so this needs
// no root; an older logind asks an admin through polkit, and with no agent
// to ask that fails, which is when LingerHint is the way.
func EnableLinger() error {
	out, err := exec.Command("loginctl", "enable-linger", fmt.Sprint(os.Getuid())).CombinedOutput()
	if err != nil {
		return fmt.Errorf("loginctl enable-linger: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LingerHint is the root form of what EnableLinger does, for a logind that
// will not let a user linger themself.
func LingerHint() string { return "sudo loginctl enable-linger $USER" }

// --- the admin password, from witmproxy's own file -------------------------------

// AdminPassword reads rig's password back from the instance's config.toml,
// where witmproxy persisted it at install.
func (i Instance) AdminPassword() (string, error) {
	path, err := i.ConfigFile()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s's proxy configuration: %w", i.VM, err)
	}
	pw, err := adminPasswordIn(b)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return pw, nil
}

// adminPasswordIn finds `admin_password = "..."` in the `[config.auth]` table
// of witmproxy's config.toml. Not a TOML parser: the value is one rig wrote
// (hex), so a basic string with no escapes is all it has to read, and
// anything else is refused rather than misread.
func adminPasswordIn(toml []byte) (string, error) {
	inAuth := false
	for _, raw := range strings.Split(string(toml), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") {
			inAuth = line == "[config.auth]"
			continue
		}
		if !inAuth {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "admin_password" {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' || strings.Contains(value, "\\") {
			return "", errors.New("admin_password is not a plain quoted string")
		}
		return value[1 : len(value)-1], nil
	}
	return "", errors.New("no admin_password under [config.auth]; the instance was not installed by rig (rig rm, then rig start again)")
}

// --- the instance's policy -------------------------------------------------------

// PrivateRanges is what no guest's proxy ever dials for it: the isolation
// ACL's reject list plus what the proxy's own host must not be reached at. A
// guest's request through the proxy gets no further than the guest's own
// NIC would have.
func PrivateRanges() []string {
	return append(append([]string{}, policy.RejectRanges...), "127.0.0.0/8", "[::1]", "[fc00::]/7", "[fe80::]/10")
}

// PolicyTOML renders a guest's proxy policy: everything forbidden by
// default, the private networks forbidden to anyone, and the guest (by its
// address, which Incus pins to its NIC) permitted HTTPS to the hosts its
// manifest names and nothing else; with guest.proxy.egress internet, the
// guest is permitted everything, and the forbid still wins (Cedar: a forbid
// no permit undoes). With a check that says so, which witmproxy evaluates
// when it loads the file and refuses it if it fails.
func PolicyTOML(ip string, rules []Rule, egress string) string {
	hosts := map[string]bool{}
	for _, r := range rules {
		for _, e := range r.Endpoints {
			hosts[e.Host] = true
		}
	}
	var b strings.Builder
	b.WriteString("# written by rig: this guest's proxy reaches the hosts its manifest names, nothing else\n")
	b.WriteString("default = \"forbid\"\n\n[sets]\nprivate = [")
	for i, cidr := range PrivateRanges() {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", cidr)
	}
	b.WriteString("]\n\n[[rules]]\neffect = \"forbid\"\ndst = [\"@private:*\"]\ncomment = \"this host's own networks are never dialed for a guest\"\n")
	internet := egress == manifest.EgressInternet
	if len(hosts) == 0 && !internet {
		return b.String()
	}
	names := make([]string, 0, len(hosts))
	for h := range hosts {
		names = append(names, h)
	}
	sort.Strings(names)
	if len(names) > 0 {
		b.WriteString("\n[[rules]]\neffect = \"permit\"\n")
		fmt.Fprintf(&b, "src = [\"ip:%s\"]\n", ip)
		b.WriteString("dst = [")
		for i, h := range names {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", h+":443")
		}
		b.WriteString("]\ncomment = \"a credential is granted for these\"\n")
	}
	if internet {
		b.WriteString("\n[[rules]]\neffect = \"permit\"\n")
		fmt.Fprintf(&b, "src = [\"ip:%s\"]\n", ip)
		b.WriteString("dst = [\"*:*\"]\ncomment = \"guest.proxy.egress: internet; the forbid above still wins\"\n")
	}
	// The check names a concrete host: a wildcard permit is proven by one
	// name under it. With everything permitted, what is checked is the
	// forbid: a private address on any port is still refused, and a public
	// name on any port reached; example.com is nobody's credential host.
	probe := "example.com"
	if len(names) > 0 {
		probe = names[0]
		if rest, ok := strings.CutPrefix(probe, "*."); ok {
			probe = "proof." + rest
		}
	}
	fmt.Fprintf(&b, "\n[[checks]]\nsrc = \"ip:%s\"\n", ip)
	if internet {
		fmt.Fprintf(&b, "permit = [%q, %q, %q]\nforbid = [%q, %q]\n", probe+":443", "example.com:443", "example.com:80", "10.0.0.5:22", "192.168.1.1:443")
	} else {
		fmt.Fprintf(&b, "permit = [%q]\nforbid = [%q, %q]\n", probe+":443", probe+":80", "10.0.0.5:22")
	}
	return b.String()
}

// WritePolicy puts the guest's policy where its proxy reads it at start.
// Reports whether the file changed, since a running proxy reads it only at
// start.
func (i Instance) WritePolicy(rules []Rule, egress string) (changed bool, err error) {
	path, err := i.PolicyFile()
	if err != nil {
		return false, err
	}
	want := []byte(PolicyTOML(i.IP, rules, egress))
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, want, 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}
