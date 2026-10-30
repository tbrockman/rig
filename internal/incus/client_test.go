package incus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// --- the envelope --------------------------------------------------------

func TestGetUnwrapsMetadataAndReturnsTheETag(t *testing.T) {
	f := newFake(t)
	f.handle("GET /1.0/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc"`)
		writeSync(w, map[string]any{"name": r.PathValue("name"), "status": "Stopped"})
	})
	inst, etag, err := f.client().Instance("myvm")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Name != "myvm" || !inst.Stopped() {
		t.Errorf("decoded %+v", inst)
	}
	if etag != `"abc"` {
		t.Errorf("etag = %q, want the header", etag)
	}
}

// Incus answers a failed request with an error envelope, and not always with
// an error status: the files endpoint has returned one inside a 200. The
// client must read the envelope, not the status line.
func TestErrorEnvelopeBecomesAnAPIErrorWhateverTheStatus(t *testing.T) {
	f := newFake(t)
	f.handle("GET /1.0/instances/missing", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, 404, "Instance not found")
	})
	f.handle("GET /1.0/instances/quirky", func(w http.ResponseWriter, r *http.Request) {
		writeErrorAt200(w, 400, "bad but 200")
	})
	c := f.client()

	_, _, err := c.Instance("missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.NotFound() {
		t.Errorf("want a not-found APIError, got %v", err)
	}
	if c.Exists("missing") {
		t.Error("Exists must be false for a 404")
	}
	if _, _, err := c.Instance("quirky"); !errors.As(err, &apiErr) || apiErr.Code != 400 {
		t.Errorf("an error envelope at HTTP 200 must still be an error, got %v", err)
	}
}

func TestSocketHintNamesTheTwoFreshHostFailures(t *testing.T) {
	_, err := New("/nonexistent/incus.sock").Instances()
	if err == nil || !strings.Contains(err.Error(), "Is Incus installed") {
		t.Errorf("a missing socket must say the daemon may not be there, got: %v", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("root can open anything; the permission case cannot be set up")
	}
	dir, err := os.MkdirTemp("", "rig-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := os.Chmod(sock, 0); err != nil {
		t.Fatal(err)
	}
	_, err = New(sock).Instances()
	if err == nil || !strings.Contains(err.Error(), "incus-admin") {
		t.Errorf("a socket this user cannot open must name the group, got: %v", err)
	}
}

// --- async operations ----------------------------------------------------

// Incus reports an operation's failure inside a 200 response to /wait. Reading
// only the status line would report every failed start and delete as success.
func TestAsyncOperationFailureSurfaces(t *testing.T) {
	f := newFake(t)
	f.handle("DELETE /1.0/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("name") {
		case "stuck":
			writeAsync(w, f.operation("op-stuck", map[string]any{
				"err": "Instance is running", "status": "Failure", "status_code": 400,
			}), nil)
		default:
			writeAsync(w, f.operation("op-ok", map[string]any{
				"err": "", "status": "Success", "status_code": 200,
			}), nil)
		}
	})
	c := f.client()
	if err := c.DeleteInstance("stuck"); err == nil || !strings.Contains(err.Error(), "Instance is running") {
		t.Errorf("the operation's own error must come through, got %v", err)
	}
	if err := c.DeleteInstance("fine"); err != nil {
		t.Errorf("a successful operation is not an error: %v", err)
	}
}

// A start refused because host processes hold the card carries the holders
// only in its text, as Incus 6.0 writes it: PIDs one per line, tab-indented.
func TestDeviceInUseReadsTheRefusal(t *testing.T) {
	f := newFake(t)
	f.handle("PUT /1.0/instances/{name}/state", func(w http.ResponseWriter, r *http.Request) {
		writeAsync(w, f.operation("op-start", map[string]any{
			"err": "Failed to start device \"gpu\": Cannot use device \"/dev/nvidia0\", " +
				"2 processes are still attached to it:\n\t4242\n\t4343",
			"status": "Failure", "status_code": 400,
		}), nil)
	})
	err := f.client().SetState("myvm", "start", 30)
	got, ok := DeviceInUse(err)
	if !ok {
		t.Fatalf("not recognised: %v", err)
	}
	if got.Device != "gpu" || got.Node != "/dev/nvidia0" || len(got.PIDs) != 2 || got.PIDs[0] != 4242 || got.PIDs[1] != 4343 {
		t.Errorf("got %+v", got)
	}
	for _, other := range []error{nil, errors.New("operation failed: Instance is running")} {
		if _, ok := DeviceInUse(other); ok {
			t.Errorf("%v is not a device in use", other)
		}
	}
}

// --- events and operations -------------------------------------------------

// The handback watcher acts on an instance's stop event, so only lifecycle
// events about that instance itself may come through as its, and the stream
// has to end when Incus closes it, so the watcher knows to reconnect.
func TestLifecycleEventsNameTheirInstance(t *testing.T) {
	f := newFake(t)
	f.handle("GET /1.0/events", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "lifecycle" {
			t.Errorf("subscribed to %q", r.URL.RawQuery)
		}
		c := acceptWS(t, w, r)
		if c == nil {
			return
		}
		for _, ev := range []string{
			`{"type":"logging","metadata":{"message":"hi"}}`,
			`{"type":"lifecycle","metadata":{"action":"instance-shutdown","source":"/1.0/instances/other"}}`,
			`{"type":"lifecycle","metadata":{"action":"instance-snapshot-created","source":"/1.0/instances/myvm/snapshots/s0"}}`,
			`{"type":"lifecycle","metadata":{"action":"instance-shutdown","source":"/1.0/instances/myvm?project=default"}}`,
		} {
			c.Write(r.Context(), websocket.MessageText, []byte(ev))
		}
		c.Close(websocket.StatusNormalClosure, "")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := f.client().LifecycleEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for ev := range events {
		got = append(got, ev.Instance()+" "+ev.Action)
	}
	// A snapshot's event is about the snapshot, so it names no instance.
	want := []string{"other instance-shutdown", " instance-snapshot-created", "myvm instance-shutdown"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Between the stop and start halves of a restart Incus reports the instance
// Stopped; an operation still running on it is what says it is not done.
func TestBusyLooksForARunningOperationOnTheInstance(t *testing.T) {
	f := newFake(t)
	running := []any{map[string]any{"resources": map[string]any{"instances": []string{"/1.0/instances/other"}}}}
	f.handle("GET /1.0/operations", func(w http.ResponseWriter, r *http.Request) {
		writeSync(w, map[string]any{
			"running": running,
			"success": []any{map[string]any{"resources": map[string]any{"instances": []string{"/1.0/instances/myvm"}}}},
		})
	})
	c := f.client()
	if busy, err := c.Busy("myvm"); err != nil || busy {
		t.Errorf("a finished operation, or one on another instance, is not busy: %v %v", busy, err)
	}
	running = append(running, map[string]any{"resources": map[string]any{"instances": []string{"/1.0/instances/myvm?project=default"}}})
	if busy, err := c.Busy("myvm"); err != nil || !busy {
		t.Errorf("a running operation on it is busy: %v %v", busy, err)
	}
}

// --- instances -----------------------------------------------------------

func TestCreateVMSendsTheProfileSecurebootAndARootDiskOverride(t *testing.T) {
	f := newFake(t)
	f.handle("GET /1.0/profiles/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeSync(w, map[string]any{"name": r.PathValue("name"), "devices": map[string]any{
			"root": map[string]string{"type": "disk", "path": "/", "pool": "fast"},
			"eth0": map[string]string{"type": "nic", "network": "incusbr0"},
		}})
	})
	f.handle("POST /1.0/instances", func(w http.ResponseWriter, r *http.Request) {
		writeAsync(w, f.operation("create", map[string]any{"status_code": 200}), nil)
	})

	err := f.client().CreateVM(CreateOpts{
		Name: "myvm", Image: "rig-base", Profile: "rigprof", CPUs: 4, Memory: "8GiB",
		DiskSize: "40GiB", Config: map[string]string{"user.rig.managed": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, ok := f.find("POST", "/1.0/instances")
	if !ok {
		t.Fatal("no create request was sent")
	}
	var body struct {
		Type     string            `json:"type"`
		Profiles []string          `json:"profiles"`
		Config   map[string]string `json:"config"`
		Devices  map[string]Device `json:"devices"`
		Source   map[string]string `json:"source"`
	}
	decode(t, req.Body, &body)

	if body.Type != "virtual-machine" {
		t.Errorf("type = %q", body.Type)
	}
	if len(body.Profiles) != 1 || body.Profiles[0] != "rigprof" {
		t.Errorf("profiles = %v; the VM must be made from the profile it was asked for", body.Profiles)
	}
	if body.Config["security.secureboot"] != "false" {
		t.Error("the NixOS image is unsigned; secureboot must be off or the VM will not boot")
	}
	if body.Config["limits.cpu"] != "4" || body.Config["limits.memory"] != "8GiB" || body.Config["user.rig.managed"] != "true" {
		t.Errorf("config = %v", body.Config)
	}
	if body.Source["alias"] != "rig-base" {
		t.Errorf("source = %v", body.Source)
	}
	// The override must be the profile's whole root device with one key
	// changed: the API replaces a device, it does not merge.
	root := body.Devices["root"]
	if root["size"] != "40GiB" || root["pool"] != "fast" || root["path"] != "/" || root["type"] != "disk" {
		t.Errorf("root disk override = %v; must copy the profile's entry and set size", root)
	}
	if _, leaked := body.Devices["eth0"]; leaked {
		t.Error("only the root disk is overridden; the NIC must stay inherited so the ACL applies")
	}
	if p, _ := f.find("GET", "/1.0/profiles/rigprof"); p.Path == "" {
		t.Error("the root disk must be read from the profile the VM is made from, not from default")
	}
}

func TestCreateVMWithoutADiskSizeOverridesNothing(t *testing.T) {
	f := newFake(t)
	f.handle("POST /1.0/instances", func(w http.ResponseWriter, r *http.Request) {
		writeAsync(w, f.operation("create", map[string]any{}), nil)
	})
	if err := f.client().CreateVM(CreateOpts{Name: "v", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	req, _ := f.find("POST", "/1.0/instances")
	var body map[string]any
	decode(t, req.Body, &body)
	if _, has := body["devices"]; has {
		t.Error("no size asked for, so no device override should be sent")
	}
	if p, _ := body["profiles"].([]any); len(p) != 1 || p[0] != "default" {
		t.Errorf("an empty profile must mean default, got %v", body["profiles"])
	}
}

// Every instance write is a read-modify-write with the ETag, so an edit made
// out of band between the read and the write is refused by Incus rather than
// silently overwritten.
func TestSetDevicesRoundTripsTheInstanceWithItsETag(t *testing.T) {
	f := newFake(t)
	f.handle("GET /1.0/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v7"`)
		writeSync(w, map[string]any{
			"name": "myvm", "architecture": "x86_64", "profiles": []string{"default"},
			"config":  map[string]string{"user.rig.managed": "true"},
			"devices": map[string]Device{"gpu0": {"type": "gpu", "pci": "0000:01:00.0"}},
		})
	})
	f.handle("PUT /1.0/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeAsync(w, f.operation("put", map[string]any{}), nil)
	})
	c := f.client()

	if err := c.SetDevices("myvm", map[string]Device{}); err != nil {
		t.Fatal(err)
	}
	put, ok := f.find("PUT", "/1.0/instances/myvm")
	if !ok {
		t.Fatal("no PUT was sent")
	}
	if got := put.Header.Get("If-Match"); got != `"v7"` {
		t.Errorf("If-Match = %q; the write must carry the ETag it read", got)
	}
	var body struct {
		Architecture string            `json:"architecture"`
		Profiles     []string          `json:"profiles"`
		Config       map[string]string `json:"config"`
		Devices      map[string]Device `json:"devices"`
	}
	decode(t, put.Body, &body)
	if len(body.Devices) != 0 {
		t.Errorf("devices = %v, want the empty set that was asked for", body.Devices)
	}
	if body.Config["user.rig.managed"] != "true" || body.Architecture != "x86_64" || len(body.Profiles) != 1 {
		t.Errorf("the rest of the instance must be carried unchanged, got %+v", body)
	}

	if err := c.SetConfigKey("myvm", "user.rig.env", "/x"); err != nil {
		t.Fatal(err)
	}
	puts := f.all("PUT", "/1.0/instances/myvm")
	decode(t, puts[len(puts)-1].Body, &body)
	if body.Config["user.rig.env"] != "/x" || body.Config["user.rig.managed"] != "true" {
		t.Errorf("SetConfigKey must add one key and keep the others, got %v", body.Config)
	}
	if body.Devices["gpu0"]["pci"] != "0000:01:00.0" {
		t.Error("SetConfigKey must not touch devices")
	}

	if err := c.SetConfigKey("myvm", "user.rig.managed", ""); err != nil {
		t.Fatal(err)
	}
	puts = f.all("PUT", "/1.0/instances/myvm")
	body.Config = nil
	decode(t, puts[len(puts)-1].Body, &body)
	if _, ok := body.Config["user.rig.managed"]; ok {
		t.Errorf("an empty value must remove the key, got %v", body.Config)
	}
}

// GlobalIPv4 has to name the address on the NIC rig configured, not the first
// global address it happens upon. With docker in the guest there are two, and
// the state map iterates in random order, so this ran many times to catch a
// version that was right only by luck.
// The force flag is the difference between asking a guest to shut down and
// pulling its plug, so it has to reach the wire exactly when asked for.
func TestStopSendsTheForceFlagOnlyWhenAsked(t *testing.T) {
	f := newFake(t)
	f.handle("PUT /1.0/instances/{name}/state", func(w http.ResponseWriter, r *http.Request) {
		writeSync(w, nil)
	})
	c := f.client()
	if err := c.SetState("myvm", "stop", 30); err != nil {
		t.Fatal(err)
	}
	if err := c.ForceStop("myvm", 30); err != nil {
		t.Fatal(err)
	}
	var seen []bool
	for _, req := range f.all("PUT", "/1.0/instances/myvm/state") {
		var body struct {
			Action  string `json:"action"`
			Timeout int    `json:"timeout"`
			Force   bool   `json:"force"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body.Action != "stop" || body.Timeout != 30 {
			t.Errorf("unexpected body %+v", body)
		}
		seen = append(seen, body.Force)
	}
	if len(seen) != 2 || seen[0] || !seen[1] {
		t.Fatalf("force flags on the wire = %v, want [false true]", seen)
	}
}

// The console log is the one endpoint that answers in plain text. It used to
// be requested as type=console, which 6.0.5 rejects, and decoded as an
// envelope, which would have failed on the text anyway.
func TestConsoleLogIsRequestedAsLogAndReadRaw(t *testing.T) {
	f := newFake(t)
	f.handle("GET /1.0/instances/{name}/console", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "log" {
			writeError(w, 500, "Invalid value for type parameter: "+r.URL.Query().Get("type"))
			return
		}
		w.Write([]byte("BdsDxe: loading Boot0001\nWelcome to NixOS\n"))
	})
	got, err := f.client().ConsoleLog("myvm")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Welcome to NixOS") {
		t.Fatalf("log = %q", got)
	}
}

func TestGlobalIPv4MatchesTheNICByMAC(t *testing.T) {
	f := newFake(t)
	f.handle("GET /1.0/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeSync(w, map[string]any{
			"name":   "myvm",
			"config": map[string]string{"volatile.eth0.hwaddr": "00:16:3e:AA:bb:cc"},
			"expanded_devices": map[string]Device{
				"eth0": {"type": "nic", "network": "incusbr0"},
				"root": {"type": "disk", "path": "/"},
			},
		})
	})
	f.handle("GET /1.0/instances/{name}/state", func(w http.ResponseWriter, r *http.Request) {
		writeSync(w, map[string]any{"network": map[string]any{
			"docker0": map[string]any{"hwaddr": "02:42:0a:c9:00:01", "addresses": []map[string]string{
				{"family": "inet", "address": "10.201.0.1", "scope": "global"},
			}},
			"enp5s0": map[string]any{"hwaddr": "00:16:3e:aa:bb:cc", "addresses": []map[string]string{
				{"family": "inet6", "address": "fe80::1", "scope": "link"},
				{"family": "inet", "address": "10.187.156.10", "scope": "global"},
			}},
			"lo": map[string]any{"hwaddr": "", "addresses": []map[string]string{
				{"family": "inet", "address": "127.0.0.1", "scope": "local"},
			}},
		}})
	})
	c := f.client()
	for i := 0; i < 25; i++ {
		got, err := c.GlobalIPv4("myvm")
		if err != nil {
			t.Fatal(err)
		}
		if got != "10.187.156.10" {
			t.Fatalf("run %d: got %s; the NIC's address must win over the docker bridge", i, got)
		}
	}
}

func TestGlobalIPv4FallsBackDeterministically(t *testing.T) {
	f := newFake(t)
	f.handle("GET /1.0/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeSync(w, map[string]any{"name": "old", "expanded_devices": map[string]Device{
			"eth0": {"type": "nic"},
		}})
	})
	f.handle("GET /1.0/instances/{name}/state", func(w http.ResponseWriter, r *http.Request) {
		writeSync(w, map[string]any{"network": map[string]any{
			"b": map[string]any{"hwaddr": "x", "addresses": []map[string]string{{"family": "inet", "address": "10.9.0.1", "scope": "global"}}},
			"a": map[string]any{"hwaddr": "y", "addresses": []map[string]string{{"family": "inet", "address": "10.1.0.1", "scope": "global"}}},
		}})
	})
	c := f.client()
	for i := 0; i < 25; i++ {
		got, _ := c.GlobalIPv4("old")
		if got != "10.1.0.1" {
			t.Fatalf("run %d: got %s; with no MAC to match, the answer must at least be the same every time", i, got)
		}
	}
}

// --- files ---------------------------------------------------------------

func TestWriteFileSendsModeOwnerAndOverwrite(t *testing.T) {
	f := newFake(t)
	f.handle("POST /1.0/instances/{name}/files", func(w http.ResponseWriter, r *http.Request) {
		writeSync(w, nil)
	})
	if err := f.client().WriteFile("myvm", "/run/rig/env", []byte("KEY=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	req, ok := f.find("POST", "/1.0/instances/myvm/files")
	if !ok {
		t.Fatal("no file request was sent")
	}
	if req.Query != "path=%2Frun%2Frig%2Fenv" {
		t.Errorf("query = %q", req.Query)
	}
	for k, want := range map[string]string{
		"X-Incus-Type": "file", "X-Incus-Mode": "0600", "X-Incus-Uid": "0", "X-Incus-Gid": "0",
		"X-Incus-Write": "overwrite",
	} {
		if got := req.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if string(req.Body) != "KEY=v\n" {
		t.Errorf("body = %q", req.Body)
	}
}

// Mkdir is always "ensure": a directory that already exists is the normal
// case on every push after the first, and Incus answers it with 409.
func TestMkdirTreatsConflictAsSuccessAndNothingElse(t *testing.T) {
	f := newFake(t)
	f.handle("POST /1.0/instances/{name}/files", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("path") {
		case "/work/exists":
			writeError(w, 409, "file exists")
		case "/work/nope":
			writeError(w, 403, "permission denied")
		default:
			writeSync(w, nil)
		}
	})
	c := f.client()
	if err := c.Mkdir("myvm", "/work/exists", 0o755); err != nil {
		t.Errorf("an existing directory is not a failure: %v", err)
	}
	if err := c.Mkdir("myvm", "/work/new", 0o755); err != nil {
		t.Errorf("creating: %v", err)
	}
	if err := c.Mkdir("myvm", "/work/nope", 0o755); err == nil {
		t.Error("a 403 must not be swallowed with the 409")
	}
	req, _ := f.find("POST", "/1.0/instances/myvm/files")
	if req.Header.Get("X-Incus-Type") != "directory" {
		t.Errorf("type header = %q", req.Header.Get("X-Incus-Type"))
	}
}

// guestFiles serves a small guest tree the way Incus does: every answer
// carries the path's type, mode and mtime in headers, a directory's GET is its
// listing, and a HEAD has no body.
func guestFiles(f *fake) {
	tree := map[string]string{ // path -> content; "/" suffix marks a directory
		"/work/":           "",
		"/work/notes.md":   "# notes\n",
		"/work/sub/":       "",
		"/work/sub/run.sh": "#!/bin/sh\n",
		"/work/link":       "/nix/store/x",
		"/etc/hostname":    "myvm\n",
	}
	serve := func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		h := w.Header()
		h.Set("X-Incus-Modified", "2026-10-16 02:04:31 +0000 UTC")
		h.Set("X-Incus-Uid", "0")
		if _, ok := tree[p+"/"]; ok {
			h.Set("X-Incus-Type", "directory")
			h.Set("X-Incus-Mode", "0755")
			var names []string
			for k := range tree {
				rest, ok := strings.CutPrefix(strings.TrimSuffix(k, "/"), p+"/")
				if ok && !strings.Contains(rest, "/") {
					names = append(names, rest)
				}
			}
			if r.Method == http.MethodGet {
				writeSync(w, names)
			}
			return
		}
		content, ok := tree[p]
		if !ok {
			if r.Method == http.MethodHead {
				w.WriteHeader(404)
				return
			}
			writeError(w, 404, "no such file")
			return
		}
		h.Set("X-Incus-Type", "file")
		h.Set("X-Incus-Mode", "0644")
		if p == "/work/link" {
			h.Set("X-Incus-Type", "symlink")
			h.Set("X-Incus-Mode", "0777")
		}
		if p == "/work/sub/run.sh" {
			h.Set("X-Incus-Mode", "0755")
		}
		h.Set("Content-Length", strconv.Itoa(len(content)))
		if r.Method == http.MethodGet {
			w.Write([]byte(content))
		}
	}
	f.handle("GET /1.0/instances/{name}/files", serve)
	f.handle("HEAD /1.0/instances/{name}/files", serve)
}

func TestPullReturnsTheBytesAndDecodesErrors(t *testing.T) {
	f := newFake(t)
	guestFiles(f)
	c := f.client()
	b, err := c.Pull("myvm", "/etc/hostname")
	if err != nil || string(b) != "myvm\n" {
		t.Errorf("got %q, %v", b, err)
	}
	_, err = c.Pull("myvm", "/nope")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.NotFound() {
		t.Errorf("a missing file must be a not-found APIError, got %v", err)
	}
}

// The endpoint answers a directory with its listing and a symlink with its
// target, at 200. Read as content, either is a wrong answer that exits 0:
// `rig pull vm /work` once printed a JSON listing.
func TestPullRefusesWhatIsNotAFile(t *testing.T) {
	f := newFake(t)
	guestFiles(f)
	c := f.client()
	for p, typ := range map[string]string{"/work": "directory", "/work/link": "symlink"} {
		b, err := c.Pull("myvm", p)
		var nf *NotAFileError
		if !errors.As(err, &nf) || nf.Type != typ {
			t.Errorf("%s: got %q, %v; want a NotAFileError naming a %s", p, b, err, typ)
		}
	}
}

func TestStatReadsTheHeadersAndNotTheContent(t *testing.T) {
	f := newFake(t)
	guestFiles(f)
	c := f.client()
	info, err := c.Stat("myvm", "/work/sub/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 16, 2, 4, 31, 0, time.UTC)
	if info.Type != "file" || info.Mode != 0o755 || info.Size != 10 || !info.Modified.Equal(want) {
		t.Errorf("got %+v", info)
	}
	if len(f.all("GET", "/1.0/instances/myvm/files")) != 0 {
		t.Error("Stat must not GET; that transfers the file")
	}
	_, err = c.Stat("myvm", "/nope")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.NotFound() {
		t.Errorf("a missing path must be a not-found APIError, got %v", err)
	}
}

// Tree is what pull plans with, so it must name every entry with its type,
// put each directory before what it contains, and transfer no file.
func TestTreeListsDirectoriesFirstAndReadsNoFile(t *testing.T) {
	f := newFake(t)
	guestFiles(f)
	entries, err := f.client().Tree("myvm", "/work")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Rel+":"+e.Type)
	}
	want := []string{"link:symlink", "notes.md:file", "sub:directory", "sub/run.sh:file"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, r := range f.all("GET", "/1.0/instances/myvm/files") {
		q, _ := parseQuery(r.Query)
		if p := q.Get("path"); p != "/work" && p != "/work/sub" {
			t.Errorf("Tree read %s; it should only list directories", p)
		}
	}
}

// `incus file push -r` names the guest directory after the source; PushDir
// exists so the destination is exactly the destination.
func TestPushDirCopiesContentsNotTheDirectory(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "top.txt"), []byte("t"), 0o644)
	os.WriteFile(filepath.Join(src, "sub", "run.sh"), []byte("#!/bin/sh\n"), 0o755)

	f := newFake(t)
	f.handle("POST /1.0/instances/{name}/files", func(w http.ResponseWriter, r *http.Request) {
		writeSync(w, nil)
	})
	n, err := f.client().PushDir("myvm", src, "/work/proj")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("pushed %d files, want 2", n)
	}
	got := map[string]recorded{}
	for _, r := range f.all("POST", "/1.0/instances/myvm/files") {
		q, _ := parseQuery(r.Query)
		got[q.Get("path")] = r
	}
	for path, typ := range map[string]string{
		"/work/proj": "directory", "/work/proj/sub": "directory",
		"/work/proj/top.txt": "file", "/work/proj/sub/run.sh": "file",
	} {
		r, ok := got[path]
		if !ok {
			t.Errorf("%s was not written", path)
			continue
		}
		if r.Header.Get("X-Incus-Type") != typ {
			t.Errorf("%s: type %q, want %q", path, r.Header.Get("X-Incus-Type"), typ)
		}
	}
	if _, wrong := got["/work/proj/"+filepath.Base(src)]; wrong {
		t.Error("a directory named after the source appeared under the destination")
	}
	if got["/work/proj/sub/run.sh"].Header.Get("X-Incus-Mode") != "0755" {
		t.Error("file modes must be carried into the guest")
	}
}

// --- exec ----------------------------------------------------------------

// The exec contract: a login shell so the guest's PATH applies, the command
// carried in the environment so there is no quoting layer, stdin closed unless
// asked for, output collected from both streams, and the exit code read from
// the operation rather than inferred.
func TestExecRunsThroughALoginShellAndCarriesTheExitCodeOut(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	f.handle("POST /1.0/instances/{name}/exec", func(w http.ResponseWriter, r *http.Request) {
		writeAsync(w, f.operation("e1", map[string]any{
			"metadata": map[string]any{"return": 3},
		}), map[string]any{"metadata": map[string]any{
			"fds": map[string]string{"0": "s0", "1": "s1", "2": "s2"},
		}})
	})
	stdinClosed := make(chan struct{})
	f.handle("GET /1.0/operations/{id}/websocket", func(w http.ResponseWriter, r *http.Request) {
		c := acceptWS(t, w, r)
		if c == nil {
			return
		}
		switch r.URL.Query().Get("secret") {
		case "s0":
			_, _, err := c.Read(ctx) // the client closes it straight away
			if err == nil {
				t.Error("stdin was written to; it should be closed when not requested")
			}
			close(stdinClosed)
		case "s1":
			c.Write(ctx, websocket.MessageBinary, []byte("out\n"))
			eof(ctx, c)
		case "s2":
			c.Write(ctx, websocket.MessageBinary, []byte("err\n"))
			eof(ctx, c)
		}
	})

	out, err := f.client().Exec("myvm", "make check", ExecOpts{Dir: "/work/p", Timeout: 10 * time.Second})
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 3 {
		t.Fatalf("want ExitError code 3, got %v", err)
	}
	if !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Errorf("both streams must be collected, got %q", out)
	}
	select {
	case <-stdinClosed:
	case <-time.After(5 * time.Second):
		t.Error("stdin websocket was never closed")
	}

	req, _ := f.find("POST", "/1.0/instances/myvm/exec")
	var body struct {
		Command          []string          `json:"command"`
		Environment      map[string]string `json:"environment"`
		WaitForWebsocket bool              `json:"wait-for-websocket"`
		Interactive      bool              `json:"interactive"`
	}
	decode(t, req.Body, &body)
	if len(body.Command) != 3 || body.Command[0] != "bash" || body.Command[1] != "-lc" {
		t.Errorf("command = %v; must be a login shell", body.Command)
	}
	if body.Environment["RIG_COMMAND"] != "make check" || body.Environment["RIG_DIR"] != "/work/p" {
		t.Errorf("environment = %v; the command and dir travel in the environment", body.Environment)
	}
	if !strings.Contains(body.Command[2], `cd "$RIG_DIR" && exec bash -c "$RIG_COMMAND"`) {
		t.Errorf("script = %q; must cd then exec, so the login shell never exits on its own", body.Command[2])
	}
	if !body.WaitForWebsocket || body.Interactive {
		t.Errorf("wait-for-websocket=%v interactive=%v", body.WaitForWebsocket, body.Interactive)
	}
	wait, ok := f.find("GET", "/1.0/operations/e1/wait")
	if !ok || !strings.HasPrefix(wait.Query, "timeout=") {
		t.Error("the exit code must be read by waiting on the operation, with a timeout")
	}
}

func TestExecReturnsOutputAndNoErrorOnExitZero(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	f.handle("POST /1.0/instances/{name}/exec", func(w http.ResponseWriter, r *http.Request) {
		writeAsync(w, f.operation("e2", map[string]any{"metadata": map[string]any{"return": 0}}),
			map[string]any{"metadata": map[string]any{"fds": map[string]string{"0": "a", "1": "b", "2": "c"}}})
	})
	f.handle("GET /1.0/operations/{id}/websocket", func(w http.ResponseWriter, r *http.Request) {
		c := acceptWS(t, w, r)
		if c == nil {
			return
		}
		if r.URL.Query().Get("secret") == "b" {
			c.Write(ctx, websocket.MessageBinary, []byte("hello\n"))
		}
		eof(ctx, c)
	})
	out, err := f.client().Exec("myvm", "echo hello", ExecOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello" {
		t.Errorf("output = %q; trailing newlines are trimmed, nothing else", out)
	}
}
