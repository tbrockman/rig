package incus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// Lifecycle is one of Incus's lifecycle events: an instance started, shut
// down, was deleted.
type Lifecycle struct {
	Action string // e.g. instance-shutdown
	Source string // e.g. /1.0/instances/daw
}

// Instance names the instance the event is about, or "" when it is about
// something else, a snapshot of one included.
func (e Lifecycle) Instance() string {
	path, _, _ := strings.Cut(e.Source, "?")
	name, ok := strings.CutPrefix(path, "/1.0/instances/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return ""
	}
	name, err := url.PathUnescape(name)
	if err != nil {
		return ""
	}
	return name
}

// LifecycleEvents streams lifecycle events until ctx ends or the connection
// drops, when the channel closes. An event is sent only while someone is
// listening, so a caller that reconnects has to read the state again for
// whatever it missed.
func (c *Client) LifecycleEvents(ctx context.Context) (<-chan Lifecycle, error) {
	// Host is irrelevant: the transport dials the unix socket regardless.
	conn, _, err := websocket.Dial(ctx, "ws://incus/1.0/events?type=lifecycle", &websocket.DialOptions{HTTPClient: c.ws})
	if err != nil {
		return nil, fmt.Errorf("events: %w%s", err, socketHint(err))
	}
	conn.SetReadLimit(-1)
	out := make(chan Lifecycle, 16)
	go func() {
		defer close(out)
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var ev struct {
				Type     string `json:"type"`
				Metadata struct {
					Action string `json:"action"`
					Source string `json:"source"`
				} `json:"metadata"`
			}
			if json.Unmarshal(data, &ev) != nil || ev.Type != "lifecycle" {
				continue
			}
			select {
			case out <- Lifecycle{Action: ev.Metadata.Action, Source: ev.Metadata.Source}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Busy reports whether an Incus operation is under way on an instance: a
// start, a stop, a restart. Status alone cannot say, because between the two
// halves of a restart Incus reports the instance Stopped.
func (c *Client) Busy(name string) (bool, error) {
	var ops map[string][]struct {
		Resources map[string][]string `json:"resources"`
	}
	if _, err := c.Get("/1.0/operations?recursion=1", &ops); err != nil {
		return false, err
	}
	want := "/1.0/instances/" + url.PathEscape(name)
	for _, status := range []string{"pending", "running"} {
		for _, op := range ops[status] {
			for _, r := range op.Resources["instances"] {
				if path, _, _ := strings.Cut(r, "?"); path == want {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
