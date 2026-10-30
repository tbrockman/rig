package agent

import (
	"strings"
	"testing"
	"time"
)

func types(evs []Event) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Type)
	}
	return strings.Join(s, ",")
}

func TestChangesStartWithWhereThingsStand(t *testing.T) {
	cur := Probe{Active: "active", Restarts: "0", Pending: 1,
		Asks:    []Ask{{ID: "new", Text: "newer?"}, {ID: "old", Text: "older?"}, {ID: "done", Text: "answered"}},
		Answers: map[string]Envelope{"done": {}},
		SHA256:  map[string]string{BinName: "bad"},
		Done:    true, Summary: "all green"}
	evs := Changes(nil, cur, map[string]string{Installed[1].Key: "good"})
	if got := types(evs); got != "watching,ask-opened,ask-opened,integrity,done" {
		t.Fatalf("got %s", got)
	}
	cur.Budget = "hours: 25.0 of 24"
	if evs := Changes(nil, cur, nil); evs[len(evs)-1].Type != EvBudget || evs[len(evs)-1].Value("reason") != cur.Budget {
		t.Errorf("a budget that already fired is reported at the start: %+v", evs)
	}
	if evs[0].Value("open-asks") != "2" || evs[1].Value("id") != "old" || evs[2].Value("id") != "new" {
		t.Errorf("open asks oldest first, answered ones left out: %+v", evs)
	}
}

func TestChangesSayEachThingOnce(t *testing.T) {
	prev := Probe{Active: "active", Restarts: "0", StatusMD: "10.1",
		Asks: []Ask{{ID: "a"}}, Answers: map[string]Envelope{},
		Deliveries: 2, RecentDeliveries: []Delivery{{Seq: 1}, {Seq: 2}},
		SHA256: map[string]string{BinName: "good"}}
	cur := Probe{Active: "failed", Restarts: "1", StatusMD: "12.2",
		Asks: []Ask{{ID: "b", Text: "which?"}, {ID: "a"}}, Answers: map[string]Envelope{"a": {}},
		Deliveries: 4, RecentDeliveries: []Delivery{{Seq: 1}, {Seq: 2}, {Seq: 3, Kind: "message", ID: "m3", Path: "hook"}, {Seq: 4, Kind: "answer", ID: "a", Path: "wait"}},
		Tampers: 1, RecentTampers: []Tamper{{File: "rejected/x.msg.1", Reason: "bad signature"}},
		SHA256: map[string]string{BinName: "swapped"}, Done: true, Summary: "finished",
		Budget: "tokens: 60,000 of 50,000", Watchdog: "idle: nothing new for 21m"}
	cfg := map[string]string{Installed[1].Key: "good"}

	evs := Changes(&prev, cur, cfg)
	want := "unit,restarts,ask-opened,ask-answered,delivered,delivered,tamper,integrity,done,budget,watchdog,status-changed"
	if got := types(evs); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if evs[4].Value("seq") != "3" || evs[4].Value("path") != "hook" || evs[5].Value("kind") != "answer" {
		t.Errorf("deliveries: %+v %+v", evs[4], evs[5])
	}
	if again := Changes(&cur, cur, cfg); len(again) != 0 {
		t.Errorf("nothing changed, yet: %s", types(again))
	}
}

// A log replaced by a shorter one has nothing new in it; a burst longer than
// the probe's window reports what the window holds.
func TestNewTailIsBoundedByWhatWasRead(t *testing.T) {
	if got := newTail(5, 3, []int{1, 2, 3}); got != nil {
		t.Errorf("got %v", got)
	}
	if got := newTail(0, 50, []int{48, 49, 50}); len(got) != 3 {
		t.Errorf("got %v", got)
	}
}

// One line, three positional fields, then key=value that a script can split:
// guest text cannot add a line or a field.
func TestEventLinesAreOneParseableLine(t *testing.T) {
	e := NewEvent(EvAskOpened, "id", "q1", "text", "Bump CUDA?\nfake=1 \x1b[2J", "empty", "")
	line := e.Line(time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC), "cuda-agent")
	want := `2026-10-16T10:00:00Z cuda-agent ask-opened id=q1 text="Bump CUDA? fake=1  [2J" empty=""`
	if line != want {
		t.Errorf("got  %s\nwant %s", line, want)
	}
}
