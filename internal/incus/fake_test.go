package incus

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// A fake Incus daemon on a unix socket.
//
// Not a mock of the client's methods — a server the real client talks to over
// the real transport, so what gets pinned is the wire: the request shapes, the
// headers, the envelope quirks (an error inside a 200, results inside an
// operation's metadata) and the exec websocket handshake. Those are the things
// that were got wrong while writing the client, and the only things a fake can
// honestly test; whether Incus then does the right thing is the integration
// suite's question.
type fake struct {
	t      *testing.T
	mux    *http.ServeMux
	socket string

	mu   sync.Mutex
	reqs []recorded
	ops  map[string]any // operation id -> what GET /wait returns as metadata
}

type recorded struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

func newFake(t *testing.T) *fake {
	t.Helper()
	// Not t.TempDir(): a unix socket path is limited to 108 bytes, and a test
	// name is easily half of that.
	dir, err := os.MkdirTemp("", "rig-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	f := &fake{t: t, mux: http.NewServeMux(), socket: filepath.Join(dir, "s"), ops: map[string]any{}}
	f.mux.HandleFunc("GET /1.0/operations/{id}/wait", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		meta, ok := f.ops[r.PathValue("id")]
		f.mu.Unlock()
		if !ok {
			writeError(w, 404, "no such operation")
			return
		}
		writeSync(w, meta)
	})

	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f.record(f.mux)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return f
}

func (f *fake) client() *Client { return New(f.socket) }

// record keeps every request so tests can assert on what was sent.
func (f *fake) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		r.Body = io.NopCloser(bytesReader(body))
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
		f.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// find returns the first recorded request matching method and path.
func (f *fake) find(method, path string) (recorded, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if r.Method == method && r.Path == path {
			return r, true
		}
	}
	return recorded{}, false
}

func (f *fake) all(method, path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.reqs {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fake) handle(pattern string, h http.HandlerFunc) { f.mux.HandleFunc(pattern, h) }

// operation registers what waiting on an operation returns, and gives back
// the path a response should name.
func (f *fake) operation(id string, meta any) string {
	f.mu.Lock()
	f.ops[id] = meta
	f.mu.Unlock()
	return "/1.0/operations/" + id
}

// --- envelopes, as Incus writes them -------------------------------------

func writeSync(w http.ResponseWriter, meta any) {
	json.NewEncoder(w).Encode(map[string]any{
		"type": "sync", "status": "Success", "status_code": 200, "metadata": meta,
	})
}

func writeAsync(w http.ResponseWriter, opPath string, opMeta any) {
	json.NewEncoder(w).Encode(map[string]any{
		"type": "async", "status": "Operation created", "status_code": 100,
		"operation": opPath, "metadata": opMeta,
	})
}

// writeError writes Incus's error envelope. status is both the HTTP status and
// error_code, as the daemon does it; writeErrorAt200 is the quirk.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": msg, "error_code": status})
}

func writeErrorAt200(w http.ResponseWriter, code int, msg string) {
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": msg, "error_code": code})
}

func decode(t *testing.T, b []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decoding request body: %v\n%s", err, b)
	}
}

// acceptWS upgrades an exec websocket. Origin checking is off because the
// client dials a made-up host ("ws://incus/...") over the unix socket.
func acceptWS(t *testing.T, w http.ResponseWriter, r *http.Request) *websocket.Conn {
	t.Helper()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		t.Errorf("websocket accept: %v", err)
		return nil
	}
	return c
}

// eof is how Incus ends an exec stream: an empty frame, then close.
func eof(ctx context.Context, c *websocket.Conn) {
	_ = c.Write(ctx, websocket.MessageBinary, []byte{})
	c.Close(websocket.StatusNormalClosure, "")
}
