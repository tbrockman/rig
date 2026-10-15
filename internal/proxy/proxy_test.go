package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tbrockman/rig/internal/manifest"
)

func TestSplitReplacesOnlyProxiedValues(t *testing.T) {
	env := []byte("# keys\nANTHROPIC_API_KEY=sk-ant-real\nOTHER=plain\nexport X=1\n\nGH_TOKEN=ghp_real\n")
	creds := map[string][]string{
		"ANTHROPIC_API_KEY": {"POST api.anthropic.com/v1/messages", "api.anthropic.com"},
		"GH_TOKEN":          {"api.github.com"},
	}
	guest, rules, err := Split(env, creds, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(guest)
	if strings.Contains(s, "sk-ant-real") || strings.Contains(s, "ghp_real") {
		t.Fatalf("a real value reached the guest:\n%s", s)
	}
	for _, keep := range []string{"# keys\n", "OTHER=plain\n", "export X=1\n", "\n\n"} {
		if !strings.Contains(s, keep) {
			t.Errorf("lost %q:\n%s", keep, s)
		}
	}
	if len(rules) != 2 || rules[0].Var != "ANTHROPIC_API_KEY" || rules[0].Value != "sk-ant-real" {
		t.Fatalf("rules = %+v", rules)
	}
	if !strings.HasPrefix(rules[0].Placeholder, "rig-") || len(rules[0].Placeholder) != 36 {
		t.Errorf("stand-in = %q", rules[0].Placeholder)
	}
	if !strings.Contains(s, "ANTHROPIC_API_KEY="+rules[0].Placeholder+"\n") {
		t.Errorf("the guest did not get the stand-in:\n%s", s)
	}
	// The endpoints are parsed, in the manifest's order.
	if len(rules[0].Endpoints) != 2 || rules[0].Endpoints[0].Path != "/v1/messages" || rules[0].Endpoints[0].Methods[0] != "POST" || rules[0].Endpoints[1].Host != "api.anthropic.com" {
		t.Errorf("endpoints = %+v", rules[0].Endpoints)
	}
}

func TestSplitRefusesWhatTheManifestPromisedAndTheFileLacks(t *testing.T) {
	_, _, err := Split([]byte("OTHER=1\n"), map[string][]string{"ANTHROPIC_API_KEY": {"api.anthropic.com"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not define it") {
		t.Errorf("err = %v", err)
	}
	_, _, err = Split([]byte("ANTHROPIC_API_KEY=\n"), map[string][]string{"ANTHROPIC_API_KEY": {"api.anthropic.com"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v", err)
	}
	_, _, err = Split([]byte("K=v\n"), map[string][]string{"K": {"https://x.com"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "not an endpoint") {
		t.Errorf("err = %v", err)
	}
}

// A shell takes the last assignment; the proxy must hold that one, under
// the one stand-in the guest keeps.
func TestSplitKeepsTheLastValueOfARepeatedVariable(t *testing.T) {
	guest, rules, err := Split([]byte("K=first\nK=second\n"), map[string][]string{"K": {"a.example.com"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Value != "second" {
		t.Errorf("rules = %+v", rules)
	}
	if strings.Count(string(guest), "K=") != 1 {
		t.Errorf("guest env:\n%s", guest)
	}
}

func TestRulesJSONIsWhatThePluginReads(t *testing.T) {
	rules := []Rule{{
		Placeholder: "rig-0123456789abcdef0123456789abcdef", Value: "sk&<real>", Var: "K",
		Endpoints: []manifest.Endpoint{{Host: "api.anthropic.com", Methods: []string{"POST"}, Path: "/v1/messages"}, {Host: "*.example.com"}},
	}}
	got := RulesJSON(rules)
	want := `[{"placeholder":"rig-0123456789abcdef0123456789abcdef","value":"sk&<real>","endpoints":[{"host":"api.anthropic.com","methods":["POST"],"path":"/v1/messages"},{"host":"*.example.com"}]}]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if RulesJSON(nil) != "[]" {
		t.Errorf("no rules = %s", RulesJSON(nil))
	}
}

func TestEncodeIsCanonical(t *testing.T) {
	a := Encode(map[string][]string{"B": {"z.example.com", "POST a.example.com/x/"}, "A": {"a.example.com"}})
	b := Encode(map[string][]string{"A": {"a.example.com"}, "B": {"POST a.example.com/x", "z.example.com"}})
	if a != b {
		t.Errorf("%s != %s", a, b)
	}
	if a != `{"A":["a.example.com"],"B":["POST a.example.com/x","z.example.com"]}` {
		t.Errorf("encoded as %s", a)
	}
	if Encode(nil) != "" {
		t.Error("nothing should encode as empty")
	}
	back, err := Decode(a)
	if err != nil || len(back["B"]) != 2 {
		t.Errorf("decode: %v %v", back, err)
	}
	if _, err := Decode("not json"); err == nil {
		t.Error("garbage decoded")
	}
}

func TestGuestEnvNamesTheProxyAndTheBundle(t *testing.T) {
	s := string(GuestEnv("http://10.207.0.1:3105", []string{"NODE_EXTRA_CA_CERTS=" + GuestCA}))
	for _, want := range []string{"HTTPS_PROXY=http://10.207.0.1:3105\n", "SSL_CERT_FILE=" + GuestBundle + "\n", "CARGO_HTTP_CAINFO=" + GuestBundle + "\n", "NODE_EXTRA_CA_CERTS=" + GuestCA + "\n", "NO_PROXY=localhost"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestDescribeNeverPrintsAValue(t *testing.T) {
	rules := []Rule{{Placeholder: "rig-x", Value: "SECRET", Var: "K", Endpoints: []manifest.Endpoint{{Host: "a.example.com", Methods: []string{"POST"}, Path: "/v1"}}}}
	d := Describe(rules)
	if strings.Contains(d, "SECRET") || strings.Contains(d, "rig-x") || d != "K -> POST a.example.com/v1" {
		t.Errorf("describe = %q", d)
	}
}

// A guest's proxy follows from its name and its address: nothing is stored.
func TestInstanceFollowsFromTheGuest(t *testing.T) {
	i := InstanceFor("agent", "10.207.0.5")
	if i.Name() != "rig-agent" || i.Unit() != "ez-witmproxy@rig-agent.service" {
		t.Errorf("%q %q", i.Name(), i.Unit())
	}
	if i.ProxyPort() != 3105 || i.APIPort() != 3405 {
		t.Errorf("ports %d %d", i.ProxyPort(), i.APIPort())
	}
	if i.ProxyBind() != "10.207.0.1:3105" || i.APIBind() != "127.0.0.1:3405" || i.Server() != "https://127.0.0.1:3405" || i.ProxyURL() != "http://10.207.0.1:3105" {
		t.Errorf("%q %q %q %q", i.ProxyBind(), i.APIBind(), i.Server(), i.ProxyURL())
	}
	vm, ok := VMOfInstance(i.Name())
	if !ok || vm != "agent" {
		t.Errorf("%q %v", vm, ok)
	}
	if _, ok := VMOfInstance("agent-1"); ok {
		t.Error("someone else's instance read as rig's")
	}
	t.Setenv("XDG_STATE_HOME", "/tmp/state")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/config")
	root, _ := i.Root()
	ca, _ := i.CAFile()
	unit, _ := i.UnitFile()
	if root != "/tmp/state/witmproxy/instances/rig-agent" || ca != root+"/certs/ca.crt" || unit != "/tmp/config/systemd/user/ez-witmproxy@rig-agent.service" {
		t.Errorf("%q %q %q", root, ca, unit)
	}
}

func TestInstalledVMsReadsTheStateDirectory(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if got := InstalledVMs(); len(got) != 0 {
		t.Errorf("nothing installed, got %v", got)
	}
	for _, d := range []string{"rig-b", "rig-a", "agent-1"} {
		mustMkdir(t, state+"/witmproxy/instances/"+d)
	}
	if got := InstalledVMs(); strings.Join(got, ",") != "a,b" {
		t.Errorf("got %v", got)
	}
}

// The policy a guest's proxy loads: everything forbidden by default, the
// private networks forbidden to anyone, the guest permitted HTTPS to the
// hosts its manifest names, and a check that says so.
func TestPolicyTOMLPermitsOnlyTheManifestsHostsFromTheGuest(t *testing.T) {
	rules := []Rule{
		{Var: "K", Endpoints: []manifest.Endpoint{{Host: "api.anthropic.com", Methods: []string{"POST"}, Path: "/v1/messages"}, {Host: "api.anthropic.com"}}},
		{Var: "G", Endpoints: []manifest.Endpoint{{Host: "*.github.com"}}},
	}
	p := PolicyTOML("10.207.0.5", rules, "")
	for _, want := range []string{
		"default = \"forbid\"\n",
		"private = [\"10.0.0.0/8\", ",
		"\"127.0.0.0/8\", \"[::1]\", \"[fc00::]/7\", \"[fe80::]/10\"]\n",
		"effect = \"forbid\"\ndst = [\"@private:*\"]\n",
		"effect = \"permit\"\nsrc = [\"ip:10.207.0.5\"]\ndst = [\"*.github.com:443\", \"api.anthropic.com:443\"]\n",
		"[[checks]]\nsrc = \"ip:10.207.0.5\"\npermit = [\"proof.github.com:443\"]\nforbid = [\"proof.github.com:80\", \"10.0.0.5:22\"]\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in:\n%s", want, p)
		}
	}
	if strings.Contains(p, "SECRET") || strings.Count(p, "api.anthropic.com") != 1 {
		t.Errorf("hosts are listed once and values never:\n%s", p)
	}
	// No credentials: nothing is permitted, and there is nothing to check.
	none := PolicyTOML("10.207.0.5", nil, "")
	if strings.Contains(none, "permit") || strings.Contains(none, "checks") {
		t.Errorf("an empty grant permits something:\n%s", none)
	}
	if strings.Contains(p, "*:*") {
		t.Errorf("egress left out must not permit everything:\n%s", p)
	}
}

// guest.proxy.egress: internet permits the guest everything, and the forbid
// of the private networks still wins; the check says both.
func TestPolicyTOMLWithInternetEgressPermitsEverythingButTheFloor(t *testing.T) {
	rules := []Rule{{Var: "K", Endpoints: []manifest.Endpoint{{Host: "api.anthropic.com"}}}}
	p := PolicyTOML("10.207.0.5", rules, manifest.EgressInternet)
	for _, want := range []string{
		"default = \"forbid\"\n",
		"effect = \"forbid\"\ndst = [\"@private:*\"]\n",
		"effect = \"permit\"\nsrc = [\"ip:10.207.0.5\"]\ndst = [\"api.anthropic.com:443\"]\n",
		"effect = \"permit\"\nsrc = [\"ip:10.207.0.5\"]\ndst = [\"*:*\"]\n",
		"[[checks]]\nsrc = \"ip:10.207.0.5\"\npermit = [\"api.anthropic.com:443\", \"example.com:443\", \"example.com:80\"]\nforbid = [\"10.0.0.5:22\", \"192.168.1.1:443\"]\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in:\n%s", want, p)
		}
	}
	// Never "src = [\"*\"]": the permit is the guest's, by its pinned address.
	if strings.Contains(p, "src = [\"*\"]") {
		t.Errorf("the permit must be the guest's alone:\n%s", p)
	}
}

// What doctor runs in the guest for a host the manifest does not name.
func TestGuestEgressProbeExpectsARefusalOrTheFile(t *testing.T) {
	refuse := GuestEgressProbe("http://10.207.0.1:3105", false)
	reach := GuestEgressProbe("http://10.207.0.1:3105", true)
	for _, s := range []string{refuse, reach} {
		for _, want := range []string{"--proxy http://10.207.0.1:3105", "--cacert " + GuestBundle, ProbeURL, "exit 2"} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %q in:\n%s", want, s)
			}
		}
	}
	if !strings.Contains(refuse, "response 403") || strings.Contains(refuse, "= 200") {
		t.Errorf("without egress the probe must expect the CONNECT refused:\n%s", refuse)
	}
	if !strings.Contains(reach, "= 200") || strings.Contains(reach, "response 403") {
		t.Errorf("with egress the probe must expect the file:\n%s", reach)
	}
}

func TestAdminPasswordIsReadFromWitmproxysOwnConfig(t *testing.T) {
	toml := "[config]\n\n[config.auth]\nenabled = true\nadmin_email = \"rig@localhost\"\nadmin_password = \"0123456789abcdef0123456789abcdef\"\n\n[config.db]\ndb_password = \"other\"\n"
	pw, err := adminPasswordIn([]byte(toml))
	if err != nil || pw != "0123456789abcdef0123456789abcdef" {
		t.Errorf("%q %v", pw, err)
	}
	if _, err := adminPasswordIn([]byte("[config.db]\nadmin_password = \"x\"\n")); err == nil {
		t.Error("a password under another table was accepted")
	}
	if _, err := adminPasswordIn([]byte("[config.auth]\nadmin_password = \"a\\\"b\"\n")); err == nil {
		t.Error("an escaped string was read as plain")
	}
	if _, err := adminPasswordIn([]byte("[config.auth]\nadmin_password = 'x'\n")); err == nil {
		t.Error("a literal string was accepted")
	}
}

// A fake witmproxy instance's management API: enough of it to see what rig
// sends. Login takes rig's account; everything else wants the token it gave.
type fakeAPI struct {
	t        *testing.T
	calls    []string
	uploaded []byte
	enabled  *bool
	config   map[string]string
	password string
}

func newFakeAPI(t *testing.T, password string) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{t: t, config: map[string]string{}, password: password}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "OK") })
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["email"] != AdminEmail || body["password"] != f.password {
			http.Error(w, "Invalid credentials", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "tok-" + f.password})
	})
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.calls = append(f.calls, r.Method+" "+r.URL.Path)
			if r.Header.Get("Authorization") != "Bearer tok-"+f.password {
				http.Error(w, "no token", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/api/plugins", authed(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			var list []PluginInfo
			if f.uploaded != nil {
				list = append(list, PluginInfo{Namespace: PluginNamespace, Name: PluginName, Version: "0.2.0", Enabled: f.enabled == nil || *f.enabled})
			}
			json.NewEncoder(w).Encode(list)
		case http.MethodPost:
			file, _, err := r.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			f.uploaded, _ = io.ReadAll(file)
			on := true
			f.enabled = &on // an upload enables it, as witmproxy does
			io.WriteString(w, "Plugin added/updated successfully")
		}
	}))
	mux.HandleFunc("/api/plugins/"+PluginNamespace+"/"+PluginName+"/enabled", authed(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]bool
		json.NewDecoder(r.Body).Decode(&body)
		on := body["enabled"]
		f.enabled = &on
	}))
	mux.HandleFunc("/api/plugins/"+PluginNamespace+"/"+PluginName+"/config", authed(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Config map[string]string `json:"config"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		for k, v := range body.Config {
			f.config[k] = v
		}
	}))
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

// serverCA is the fake's certificate as PEM, standing in for ca.crt.
func serverCA(t *testing.T, srv *httptest.Server) []byte {
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("no server certificate")
	}
	return pemCert(cert)
}

func TestLoginThenPluginAndRules(t *testing.T) {
	f, srv := newFakeAPI(t, "pw")
	ca := serverCA(t, srv)
	if _, err := Login(srv.URL, ca, AdminEmail, "wrong"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a wrong password logged in: %v", err)
	}
	token, err := Login(srv.URL, ca, AdminEmail, "pw")
	if err != nil || token != "tok-pw" {
		t.Fatalf("%q %v", token, err)
	}
	c, err := NewClient(srv.URL, token, ca)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Health(); err != nil {
		t.Fatal(err)
	}
	if err := c.InstallPlugin([]byte("wasm")); err != nil {
		t.Fatal(err)
	}
	if string(f.uploaded) != "wasm" || f.enabled == nil || !*f.enabled {
		t.Errorf("uploaded %q enabled %v", f.uploaded, f.enabled)
	}
	rules := []Rule{{Placeholder: "rig-0123456789abcdef0123456789abcdef", Value: "real", Var: "K", Endpoints: []manifest.Endpoint{{Host: "a.example.com"}}}}
	if err := c.SetRules(RulesJSON(rules)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.config[RulesInput], `"value":"real"`) {
		t.Errorf("the proxy did not get the rules: %v", f.config)
	}
	p, err := c.RigPlugin()
	if err != nil || p == nil || !p.Enabled {
		t.Errorf("%+v %v", p, err)
	}
	for _, call := range f.calls {
		if !strings.HasPrefix(call, "GET ") && !strings.HasPrefix(call, "POST ") && !strings.HasPrefix(call, "PUT ") {
			t.Errorf("odd call %q", call)
		}
	}
}

func TestClientTrustsOnlyTheInstancesCA(t *testing.T) {
	_, srv := newFakeAPI(t, "pw")
	// Another CA entirely (every httptest server shares one certificate, so
	// a second server would not do): a server under it must not be trusted.
	c, err := NewClient(srv.URL, "tok-pw", selfSignedCA(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Health(); err == nil {
		t.Fatal("a server under another CA was trusted")
	}
	if _, err := NewClient(srv.URL, "t", []byte("not a cert")); err == nil {
		t.Fatal("garbage accepted as a CA")
	}
	// The server's own CA is fine.
	c, _ = NewClient(srv.URL, "tok-pw", serverCA(t, srv))
	if err := c.Health(); err != nil {
		t.Fatal(err)
	}
}

func TestCountBlockedReadsOnlyTheProxyPortsOnTheBridge(t *testing.T) {
	lines := []string{
		"kernel: [UFW BLOCK] IN=rigproxy0 OUT= MAC=... SRC=10.207.0.2 DST=10.207.0.1 LEN=60 PROTO=TCP SPT=40000 DPT=3102 WINDOW=64240",
		"kernel: [UFW BLOCK] IN=rigproxy0 OUT= MAC=... SRC=10.207.0.9 DST=10.207.0.1 LEN=60 PROTO=TCP SPT=40000 DPT=3109 WINDOW=64240",
		"kernel: [UFW BLOCK] IN=rigproxy0 OUT= MAC=... SRC=10.207.0.2 DST=10.207.0.1 LEN=60 PROTO=TCP SPT=40000 DPT=22 WINDOW=64240",
		"kernel: [UFW BLOCK] IN=rigproxy0 OUT= MAC=... SRC=10.207.0.2 DST=10.207.0.1 LEN=60 PROTO=TCP SPT=40000 DPT=3128 WINDOW=64240",
		"kernel: [UFW BLOCK] IN=incusbr0 OUT= MAC=... SRC=10.1.1.2 DST=10.1.1.1 LEN=60 PROTO=TCP SPT=40000 DPT=3105 WINDOW=64240",
		"kernel: [UFW BLOCK] IN=rigproxy0 OUT= MAC=... SRC=10.207.0.2 DST=10.207.0.1 LEN=60 PROTO=UDP SPT=68 DPT=67",
	}
	if n := countBlocked(lines); n != 3 {
		t.Errorf("counted %d", n)
	}
}

// --- helpers ------------------------------------------------------------------

func pemCert(cert *x509.Certificate) []byte {
	return []byte("-----BEGIN CERTIFICATE-----\n" + base64Lines(cert.Raw) + "-----END CERTIFICATE-----\n")
}

func base64Lines(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	var line int
	for i := 0; i < len(b); i += 3 {
		var n uint32
		rem := len(b) - i
		for j := 0; j < 3; j++ {
			n <<= 8
			if j < rem {
				n |= uint32(b[i+j])
			}
		}
		for j := 0; j < 4; j++ {
			if j <= rem {
				out.WriteByte(alphabet[(n>>(18-6*uint(j)))&63])
			} else {
				out.WriteByte('=')
			}
			line++
			if line == 64 {
				out.WriteByte('\n')
				line = 0
			}
		}
	}
	if line != 0 {
		out.WriteByte('\n')
	}
	return out.String()
}

// selfSignedCA is a certificate nothing here is signed by.
func selfSignedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "not the instance"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pemCert(cert)
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestUFWRuleAdmitsOnlyTheProxyRangeOnTheBridge(t *testing.T) {
	want := "sudo ufw allow in on rigproxy0 to 10.207.0.1 port 3102:3354 proto tcp"
	if got := UFWRule(); got != want {
		t.Fatalf("UFWRule() = %q, want %q", got, want)
	}
	if got := "sudo " + strings.Join(ufwArgs(), " "); got != want {
		t.Fatalf("what AllowUFW runs, %q, is not what UFWRule prints", got)
	}
}

// A credential swapped under a running agent must reach it without the agent
// noticing: the stand-in it holds stays, and the proxy gets the new value for
// it. A stand-in the guest has for a variable no longer proxied is not kept.
func TestSplitKeepsTheStandInsTheGuestAlreadyHolds(t *testing.T) {
	old := "rig-0123456789abcdef0123456789abcdef"
	keep := StandInsIn("CLAUDE_CODE_OAUTH_TOKEN=" + old + "\nGONE=rig-ffffffffffffffffffffffffffffffff\nnot a line\nOTHER=plain\n")
	if len(keep) != 2 || keep["CLAUDE_CODE_OAUTH_TOKEN"] != old || keep["OTHER"] != "" {
		t.Fatalf("StandInsIn = %v", keep)
	}
	creds := map[string][]string{"CLAUDE_CODE_OAUTH_TOKEN": {"api.anthropic.com"}, "GH_TOKEN": {"api.github.com"}}
	guest, rules, err := Split([]byte("CLAUDE_CODE_OAUTH_TOKEN=new-value\nGH_TOKEN=ghp_x\n"), creds, keep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(guest), "CLAUDE_CODE_OAUTH_TOKEN="+old+"\n") {
		t.Errorf("the stand-in the guest held was replaced:\n%s", guest)
	}
	if strings.Contains(string(guest), "ffffffff") {
		t.Errorf("a stand-in for a variable no longer proxied was reused:\n%s", guest)
	}
	for _, r := range rules {
		switch r.Var {
		case "CLAUDE_CODE_OAUTH_TOKEN":
			if r.Placeholder != old || r.Value != "new-value" {
				t.Errorf("the proxy must hold the new value under the old stand-in, got %+v", r)
			}
		case "GH_TOKEN":
			if r.Placeholder == old || !strings.HasPrefix(r.Placeholder, "rig-") {
				t.Errorf("a variable without a stand-in gets a fresh one, got %+v", r)
			}
		}
	}
	// The script reads only stand-ins back, never a value.
	if s := GuestStandIns("/run/rig/env"); !strings.Contains(s, "rig-[0-9a-f]{32}") || !strings.Contains(s, "/run/rig/env") {
		t.Errorf("GuestStandIns = %q", s)
	}
}
