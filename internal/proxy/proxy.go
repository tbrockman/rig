// Package proxy gives a guest credentials it never holds.
//
// The one thing in an agent VM worth taking is its API key, and until now the
// guest held it: a file on tmpfs, readable by the root the agent runs as, and
// sendable anywhere the public internet reaches. This package takes it out of
// the guest. The guest gets a stand-in, an opaque string worth nothing on its
// own; a witmproxy on this host, running rig's plugin, puts the real value
// into HTTPS requests to the endpoints the manifest names, and into nothing
// else. A guest that leaks everything it has leaks a string that works only
// through its proxy, only there.
//
// Each guest has a proxy of its own: a witmproxy instance in the operator's
// user scope (`witm service install --user --instance rig-<vm>`), with its
// own process, database, CA and policy, listening on a bridge of rig's own
// (internal/policy, the proxy network) at the guest's port. rig installs,
// starts and stops it with witmproxy's own CLI and configures it through its
// management API: the plugin with that guest's rules, and a policy that lets
// that guest reach the hosts its manifest names and nothing else. The guest
// is handed HTTPS_PROXY pointing at the bridge address and the proxy's
// certificate to trust; its traffic to the rest of the world keeps going out
// of its first NIC, under the isolation ACL, carrying stand-ins.
package proxy

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/tbrockman/rig/internal/manifest"
	"github.com/tbrockman/rig/internal/policy"
)

const (
	// The plugin, as witmproxy names it.
	PluginNamespace = "joinezco"
	PluginName      = "rig"
	PluginID        = "@" + PluginNamespace + "/" + PluginName
	// RulesInput is the plugin's one setting.
	RulesInput = "rules"
	// InstanceKey records guest.proxy.credentials on the instance, as JSON.
	InstanceKey = "user.rig.proxy"
	// EgressKey records guest.proxy.egress: "" for the credentials' hosts
	// only, manifest.EgressInternet for everything the isolated NIC may reach.
	EgressKey = "user.rig.proxy.egress"
	// GuestCADir holds the proxy's root certificate in the guest, where the
	// tools the agent runs can read it. Its own directory rather than under
	// /run/rig, which is 0700 for the credentials.
	GuestCADir  = "/run/rig-ca"
	GuestCA     = GuestCADir + "/ca.pem"
	GuestBundle = GuestCADir + "/bundle.crt"
)

// Plugin is the signed @joinezco/rig component, built from plugin/ at the
// repository root (`make -C plugin`). Embedded so a `go install`ed rig
// carries it.
//
//go:embed plugin/rig.signed.wasm
var plugin []byte

// Plugin returns the embedded component.
func Plugin() []byte { return plugin }

// Rule is what the plugin is told about one credential.
type Rule struct {
	Placeholder string
	Value       string
	Endpoints   []manifest.Endpoint
	// Var is the variable it came from: for reports, never for the proxy.
	Var string
}

// --- the record on the instance ------------------------------------------

// Encode renders a manifest's credentials for the instance record: each
// endpoint in its canonical form, sorted, keys sorted, so the same intent is
// the same string. Empty for none.
func Encode(creds map[string][]string) string {
	if len(creds) == 0 {
		return ""
	}
	sorted := make(map[string][]string, len(creds))
	for k, v := range creds {
		endpoints := make([]string, 0, len(v))
		for _, s := range v {
			if e, err := manifest.ParseEndpoint(s); err == nil {
				s = e.String()
			}
			endpoints = append(endpoints, s)
		}
		sort.Strings(endpoints)
		sorted[k] = endpoints
	}
	b, _ := json.Marshal(sorted)
	return string(b)
}

// Decode reads the record back.
func Decode(s string) (map[string][]string, error) {
	if s == "" {
		return nil, nil
	}
	var out map[string][]string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("%s is not a credential record: %w", InstanceKey, err)
	}
	return out, nil
}

// --- the stand-ins ---------------------------------------------------------

// Placeholder returns a fresh stand-in: "rig-" and 32 hex digits. Opaque,
// long enough that it occurs in a request only when put there, and nothing a
// header or a URL objects to.
func Placeholder() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "rig-" + hex.EncodeToString(b), nil
}

var assignment = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// Split rewrites an env file for the guest: every variable in creds keeps its
// name and gets a stand-in for a value. The real values come back as rules
// for the proxy, with the endpoints the manifest allows each. A credential
// the file does not define is an error: the manifest promised it.
//
// keep holds stand-ins the guest already has, by variable (StandInsIn); one
// is reused rather than replaced, so a value swapped under a running agent
// (rig creds) reaches it through the proxy with nothing in the guest changed.
func Split(env []byte, creds map[string][]string, keep map[string]string) ([]byte, []Rule, error) {
	var out bytes.Buffer
	var rules []Rule
	seen := map[string]bool{}
	for _, line := range strings.SplitAfter(string(env), "\n") {
		m := assignment.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
		if m == nil {
			out.WriteString(line)
			continue
		}
		name, value := m[1], m[2]
		grants, proxied := creds[name]
		if !proxied {
			out.WriteString(line)
			continue
		}
		// A later line for the same name wins in a shell; the proxy must
		// then hold that one too, under the stand-in the guest keeps.
		if seen[name] {
			for i := range rules {
				if rules[i].Var == name {
					rules[i].Value = value
				}
			}
			continue
		}
		seen[name] = true
		if value == "" {
			return nil, nil, fmt.Errorf("%s is proxied but empty in the env file; a stand-in for nothing would hide that", name)
		}
		endpoints := make([]manifest.Endpoint, 0, len(grants))
		for _, g := range grants {
			e, err := manifest.ParseEndpoint(g)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", name, err)
			}
			endpoints = append(endpoints, e)
		}
		ph := keep[name]
		if ph == "" {
			var err error
			if ph, err = Placeholder(); err != nil {
				return nil, nil, err
			}
		}
		rules = append(rules, Rule{Placeholder: ph, Value: value, Endpoints: endpoints, Var: name})
		fmt.Fprintf(&out, "%s=%s\n", name, ph)
	}
	for _, name := range sortedKeys(creds) {
		if !seen[name] {
			return nil, nil, fmt.Errorf("guest.proxy.credentials names %s, but the env file does not define it", name)
		}
	}
	return out.Bytes(), rules, nil
}

// RulesJSON renders rules as the plugin reads them. Var stays out: it is
// rig's.
func RulesJSON(rules []Rule) string {
	type endpoint struct {
		Host    string   `json:"host"`
		Methods []string `json:"methods,omitempty"`
		Path    string   `json:"path,omitempty"`
	}
	type rule struct {
		Placeholder string     `json:"placeholder"`
		Value       string     `json:"value"`
		Endpoints   []endpoint `json:"endpoints"`
	}
	plain := make([]rule, 0, len(rules))
	for _, r := range rules {
		pr := rule{Placeholder: r.Placeholder, Value: r.Value, Endpoints: []endpoint{}}
		for _, e := range r.Endpoints {
			pr.Endpoints = append(pr.Endpoints, endpoint{Host: e.Host, Methods: e.Methods, Path: e.Path})
		}
		plain = append(plain, pr)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(plain)
	return strings.TrimSuffix(b.String(), "\n")
}

// Describe names a guest's proxied variables and their endpoints, for a
// report. The values never print.
func Describe(rules []Rule) string {
	var out []string
	for _, r := range rules {
		eps := make([]string, 0, len(r.Endpoints))
		for _, e := range r.Endpoints {
			eps = append(eps, e.String())
		}
		out = append(out, r.Var+" -> "+strings.Join(eps, ", "))
	}
	return strings.Join(out, "; ")
}

// DescribeRecord does the same from the instance record.
func DescribeRecord(creds map[string][]string) string {
	var out []string
	for _, v := range sortedKeys(creds) {
		out = append(out, v+" -> "+strings.Join(creds[v], ", "))
	}
	return strings.Join(out, "; ")
}

// --- the guest's environment ------------------------------------------------

// GuestEnv is what the guest's environment gains: its proxy, and the
// certificate bundle its interception needs trusting, named for every tool
// that reads its own variable rather than the system store, plus whatever
// lines the agent's harness needs for the same (trust). Appended to the env
// file the agent sources, so what uses the credential is what uses the proxy.
func GuestEnv(proxyURL string, trust []string) []byte {
	proxy := proxyURL
	lines := []string{
		"# added by rig: the credentials above are stand-ins; the proxy puts the real values in",
		"HTTPS_PROXY=" + proxy, "https_proxy=" + proxy,
		"HTTP_PROXY=" + proxy, "http_proxy=" + proxy,
		"NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1",
		"SSL_CERT_FILE=" + GuestBundle,
		"CURL_CA_BUNDLE=" + GuestBundle,
		"REQUESTS_CA_BUNDLE=" + GuestBundle,
		"NIX_SSL_CERT_FILE=" + GuestBundle,
		"GIT_SSL_CAINFO=" + GuestBundle,
		"CARGO_HTTP_CAINFO=" + GuestBundle,
	}
	lines = append(lines, trust...)
	return []byte(strings.Join(lines, "\n") + "\n")
}

// GuestStandIns is run in the guest to read the stand-ins its env file holds
// back out, and nothing else of it: only a line whose value has the shape
// Placeholder makes is printed. What it prints is for StandInsIn.
func GuestStandIns(envPath string) string {
	return `grep -oE '^[A-Za-z_][A-Za-z0-9_]*=rig-[0-9a-f]{32}$' ` + envPath + `; true`
}

var standInLine = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(rig-[0-9a-f]{32})$`)

// StandInsIn reads GuestStandIns's output: variable to stand-in. Anything
// else in the output is ignored, so a guest that printed an error gives none.
func StandInsIn(out string) map[string]string {
	keep := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if m := standInLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			keep[m[1]] = m[2]
		}
	}
	return keep
}

// --- the guest's half -------------------------------------------------------

// GuestSetup is run in the guest once the CA is at GuestCA: it gives the
// proxy NIC the address the host pinned to it (and nothing else: no route,
// no resolver), and builds the bundle the env names. Nothing listens in the
// guest; the proxy is reached over that NIC directly.
func GuestSetup(ip string) string {
	return fmt.Sprintf(`set -e
ip link show %[4]s >/dev/null 2>&1 || { echo "no %[4]s in this guest: the image predates the proxy NIC (rig image build, then recreate the VM)"; exit 1; }
ip link set %[4]s up
ip -4 addr flush dev %[4]s
ip addr replace %[5]s/%[6]d dev %[4]s
install -d -m 755 %[1]s
cat "$(readlink -f /etc/ssl/certs/ca-bundle.crt)" %[2]s > %[3]s.tmp && mv %[3]s.tmp %[3]s
chmod 644 %[2]s %[3]s
`, GuestCADir, GuestCA, GuestBundle, policy.ProxyGuestNIC, ip, policy.ProxyPrefix)
}

// GuestTeardown ends the guest's half: the certificate goes, so nothing in
// the guest trusts the proxy's interception any more.
func GuestTeardown() string {
	return fmt.Sprintf("rm -rf %s; true", GuestCADir)
}

// GuestNICProbe proves, from inside the guest, that the proxy NIC is what rig
// made it: it has the address rig gave it, it carries no default route (the
// bridge leads only to the proxy), and its proxy's port answers through it.
func GuestNICProbe(ip string) string {
	return fmt.Sprintf(`set -e
ip -4 -o addr show dev %[1]s 2>/dev/null | grep -q 'inet %[2]s/' || { echo "%[1]s does not have %[2]s:"; ip -4 -o addr show dev %[1]s 2>&1 | head -2; exit 1; }
if ip -4 route show default 2>/dev/null | grep -q 'dev %[1]s'; then echo "%[1]s carries a default route; the base image's dhcpcd settings for it are missing (rebuild the image)"; exit 1; fi
timeout 5 bash -c 'exec 3<>/dev/tcp/%[3]s/%[4]d' 2>/dev/null || { echo "nothing answers at %[3]s:%[4]d from %[1]s: the guest's witmproxy is not running there, or the host firewall drops the bridge (rig proxy status; docs/RUNBOOK.md)"; exit 1; }
echo "%[2]s on %[1]s; no default route; %[3]s:%[4]d answers"
`, policy.ProxyGuestNIC, ip, policy.ProxyAddr, policy.ProxyPortOf(ip))
}

// GuestProbe proves, from inside the guest, that a request to host goes
// through the proxy: the certificate it sees is the proxy's, not the host's.
// Prints the issuer on success. Needs curl in the guest, which the base
// image carries.
func GuestProbe(proxyURL, host string) string {
	return fmt.Sprintf(`command -v curl >/dev/null || { echo "no curl in this guest, so this cannot be proven"; exit 2; }
test -r %[1]s || { echo "no %[1]s: the proxy was not set up in this guest"; exit 1; }
out=$(curl --proxy %[2]s --cacert %[1]s -sS -o /dev/null -v --max-time 20 https://%[3]s/ 2>&1) || { echo "$out" | tail -3; exit 1; }
echo "$out" | grep -i '^\* *issuer:' | grep -qi witmproxy || { echo "the certificate for %[3]s was not the proxy's:"; echo "$out" | grep -i 'issuer:'; exit 1; }
echo "$out" | grep -i '^\* *issuer:' | head -1 | sed 's/^\* *//'`, GuestBundle, proxyURL, host)
}

// ProbeURL is a public file the proxy is asked for from inside the guest:
// the one `rig verify` fetches to prove the guest's own way out.
const ProbeURL = "https://cache.nixos.org/nix-cache-info"

// GuestEgressProbe proves, from inside the guest, what the proxy does with a
// host the manifest does not name: refuses it at the CONNECT, or carries it
// when guest.proxy.egress is internet. Exit 2 when it cannot be proven.
func GuestEgressProbe(proxyURL string, internet bool) string {
	host := strings.TrimPrefix(ProbeURL, "https://")
	host = host[:strings.Index(host, "/")]
	head := fmt.Sprintf(`command -v curl >/dev/null || { echo "no curl in this guest, so this cannot be proven"; exit 2; }
test -r %[1]s || { echo "no %[1]s: the proxy was not set up in this guest"; exit 1; }
err=$(mktemp); code=$(curl --proxy %[2]s --cacert %[1]s -sS -o /dev/null --max-time 20 -w '%%{http_code}' %[3]s 2>"$err"); rc=$?
`, GuestBundle, proxyURL, ProbeURL)
	if internet {
		return head + fmt.Sprintf(`[ "$rc" -eq 0 ] && [ "$code" = 200 ] || { echo "the proxy did not carry %[1]s: $(tail -1 "$err")"; rm -f "$err"; exit 1; }
rm -f "$err"; echo "%[1]s reached through the proxy, as guest.proxy.egress asks"`, host)
	}
	return head + fmt.Sprintf(`grep -q 'response 403' "$err" || { echo "the proxy carried %[1]s (HTTP $code), which its policy should refuse: $(tail -1 "$err")"; rm -f "$err"; exit 1; }
rm -f "$err"; echo "%[1]s refused at the CONNECT (403): the proxy dials the credentials' hosts only"`, host)
}

// --- helpers ----------------------------------------------------------------

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
