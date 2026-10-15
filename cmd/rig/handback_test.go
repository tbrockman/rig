package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tbrockman/rig/internal/devices"
	"github.com/tbrockman/rig/internal/incus"
)

// look is one reading of an instance: its status, or "gone", and whether an
// operation is under way on it.
type look struct {
	status string
	busy   bool
}

// scriptedWatch plays Incus for awaitStop. Each reading of the instance takes
// the next look, the last one repeating. Each subscription delivers its
// events and closes, except the last, which stays open until its context ends.
type scriptedWatch struct {
	mu    sync.Mutex
	looks []look
	read  int
	cur   look
	subs  [][]incus.Lifecycle
	nsub  int
}

func (w *scriptedWatch) Instance(name string) (*incus.Instance, string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cur = w.looks[min(w.read, len(w.looks)-1)]
	w.read++
	if w.cur.status == "gone" {
		return nil, "", &incus.APIError{Code: 404, Message: "Instance not found"}
	}
	return &incus.Instance{Name: name, Status: w.cur.status}, "", nil
}

func (w *scriptedWatch) Busy(string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur.busy, nil
}

func (w *scriptedWatch) LifecycleEvents(ctx context.Context) (<-chan incus.Lifecycle, error) {
	w.mu.Lock()
	k := min(w.nsub, len(w.subs)-1)
	last := w.nsub >= len(w.subs)-1
	w.nsub++
	w.mu.Unlock()
	ch := make(chan incus.Lifecycle, len(w.subs[k]))
	for _, ev := range w.subs[k] {
		ch <- ev
	}
	if !last {
		close(ch)
		return ch, nil
	}
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

func (w *scriptedWatch) reads() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.read
}

func event(action, vm string) incus.Lifecycle {
	return incus.Lifecycle{Action: action, Source: "/1.0/instances/" + vm}
}

func await(t *testing.T, w *scriptedWatch) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return awaitStop(ctx, w, "daw", time.Millisecond)
}

// The case the watcher exists for: the guest powers itself off. Incus sends
// instance-shutdown a moment before it reports the instance Stopped.
func TestAwaitStopSeesAGuestShutItselfDown(t *testing.T) {
	w := &scriptedWatch{
		looks: []look{{"Running", false}, {"Running", false}, {"Stopped", false}},
		subs:  [][]incus.Lifecycle{{event("instance-shutdown", "daw")}},
	}
	if !await(t, w) {
		t.Error("a guest that shut itself down was not seen as stopped")
	}
}

// incus restart sends instance-stopped, and between its halves the instance
// reads Stopped. The operation under way is what says it is not a stop, and
// handing the card back then would pull it from under the start.
func TestAwaitStopIsNotFooledByARestart(t *testing.T) {
	w := &scriptedWatch{
		looks: []look{{"Running", false}, {"Stopped", true}, {"Stopped", true}, {"Running", true}, {"Running", false}},
		subs:  [][]incus.Lifecycle{{event("instance-stopped", "daw")}},
	}
	if await(t, w) {
		t.Error("a restart was taken for a stop")
	}
}

// A guest rebooting itself sends no stop event but reads Stopped until it is
// back. Found that way on (re)connecting, a stop has to last to count.
func TestAwaitStopWaitsOutAReboot(t *testing.T) {
	w := &scriptedWatch{
		looks: []look{{"Stopped", false}, {"Stopped", false}, {"Stopped", false}, {"Running", false}},
		subs:  [][]incus.Lifecycle{{}},
	}
	if await(t, w) {
		t.Error("a reboot was taken for a stop")
	}
}

// A stop while nothing was listening, found on connecting, is a stop once it
// lasts.
func TestAwaitStopFindsAStopItMissed(t *testing.T) {
	w := &scriptedWatch{looks: []look{{"Stopped", false}}, subs: [][]incus.Lifecycle{{}}}
	if !await(t, w) {
		t.Error("a VM that stayed stopped was not seen as stopped")
	}
}

// Other instances' events are not this one's, and a deleted instance has
// nothing left to hand back.
func TestAwaitStopIgnoresOthersAndEndsOnDeletion(t *testing.T) {
	w := &scriptedWatch{
		looks: []look{{"Running", false}},
		subs:  [][]incus.Lifecycle{{event("instance-shutdown", "other"), event("instance-deleted", "daw")}},
	}
	if await(t, w) {
		t.Error("a deleted instance was reported stopped")
	}
	if n := w.reads(); n != 1 {
		t.Errorf("read the instance %d times; another VM's shutdown must not be followed", n)
	}
}

// Incus restarting drops the event stream. The watcher subscribes again and
// still sees the stop that comes after.
func TestAwaitStopResubscribesWhenTheStreamDrops(t *testing.T) {
	w := &scriptedWatch{
		looks: []look{{"Running", false}, {"Running", false}, {"Stopped", false}},
		subs:  [][]incus.Lifecycle{{}, {event("instance-shutdown", "daw")}},
	}
	if !await(t, w) {
		t.Error("the stop after a dropped stream was missed")
	}
}

// The lock crosses sudo as a flag, and only when RIG_LOCK moved it.
func TestHandbackArgs(t *testing.T) {
	if got := strings.Join(handbackArgs("daw", devices.DefaultLock, true), " "); got != "host handback daw --background" {
		t.Errorf("got %q", got)
	}
	if got := strings.Join(handbackArgs("daw", "/tmp/r.lock", false), " "); got != "host handback daw --lock=/tmp/r.lock" {
		t.Errorf("got %q", got)
	}
}
