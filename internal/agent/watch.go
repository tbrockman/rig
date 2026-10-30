package agent

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Event is one line of `rig agent watch`: something about the agent that
// changed, for an operator, or a tool that turns each line into a
// notification.
type Event struct {
	Type   string
	Fields []Field
}

// Field is one key=value of an Event, in the order it is printed.
type Field struct{ Key, Value string }

// The events watch prints. The names and their fields are an interface:
// scripts match on them.
const (
	EvWatching    = "watching"     // first line: unit, restarts, open-asks, pending
	EvAskOpened   = "ask-opened"   // id, text
	EvAskAnswered = "ask-answered" // id
	EvDelivered   = "delivered"    // seq, kind, id, path (launch, hook, wait or inbox), to (lead or caller)
	EvTamper      = "tamper"       // file, reason: an item the guest refused
	EvIntegrity   = "integrity"    // file: an installed file no longer what rig wrote
	EvDone        = "done"         // summary
	EvUnit        = "unit"         // from, to
	EvRestarts    = "restarts"     // from, to
	EvStatus      = "status-changed"
	EvUnreachable = "unreachable" // error: the probe failed; said once until it works again
	EvReachable   = "reachable"
	EvBudget      = "budget"   // reason: a budget fired and stopped the agent, session kept
	EvWatchdog    = "watchdog" // reason: a run's limit cut it; it resumes with a note

	// The soft thresholds (thresholds.go): state is crossed or cleared.
	EvIdle        = "idle"         // state, age, limit: no new commit on the workdir's branch
	EvLongUnit    = "long-unit"    // state, unit, age, limit: a transient unit the agent started, still running
	EvStaleStatus = "stale-status" // state, age, limit: STATUS.md untouched
	EvUsage       = "usage"        // state, window, utilization, limit: a subscription window
	EvForwardDown = "forward-down" // port, reason: a declared forward has nothing behind it
	EvForwardUp   = "forward-up"   // port
	EvSent        = "sent"         // threshold, id: the message on: named for a crossing was queued
)

func ev(typ string, kv ...string) Event {
	e := Event{Type: typ}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Fields = append(e.Fields, Field{kv[i], kv[i+1]})
	}
	return e
}

// NewEvent is an event of typ with fields given as key, value, key, value.
func NewEvent(typ string, kv ...string) Event { return ev(typ, kv...) }

// Line renders e as `<time> <vm> <type> key=value ...`: three positional
// fields, then logfmt, a value quoted (Go syntax) when it is empty or holds a
// space, a quote or an equals sign. One line always: guest text is Cleaned.
func (e Event) Line(t time.Time, vm string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s", t.UTC().Format(time.RFC3339), vm, e.Type)
	for _, f := range e.Fields {
		v := Clean(f.Value, 300)
		if v == "" || strings.ContainsAny(v, " \"=\\") {
			v = strconv.Quote(v)
		}
		fmt.Fprintf(&b, " %s=%s", f.Key, v)
	}
	return b.String()
}

// Value is the first field named key, or "".
func (e Event) Value(key string) string {
	for _, f := range e.Fields {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
}

// Changes is what changed between two probes, oldest first. With no previous
// probe it is where things stand: a watching line, then what already wants
// the operator (open asks, a replaced file, a finished brief), since a
// watcher started after the fact must still hear of those.
func Changes(prev *Probe, cur Probe, config map[string]string) []Event {
	var out []Event
	if prev == nil {
		open := cur.OpenAsks()
		out = append(out, ev(EvWatching, "unit", cur.Active, "restarts", cur.Restarts,
			"open-asks", strconv.Itoa(len(open)), "pending", strconv.Itoa(cur.Pending)))
		for i := len(open) - 1; i >= 0; i-- {
			out = append(out, ev(EvAskOpened, "id", open[i].ID, "text", open[i].Text))
		}
		for _, f := range cur.Mismatched(config) {
			out = append(out, ev(EvIntegrity, "file", f))
		}
		if cur.Done {
			out = append(out, ev(EvDone, "summary", cur.Summary))
		}
		if cur.Budget != "" {
			out = append(out, ev(EvBudget, "reason", cur.Budget))
		}
		return out
	}

	if prev.Active != cur.Active {
		out = append(out, ev(EvUnit, "from", prev.Active, "to", cur.Active))
	}
	if prev.Restarts != cur.Restarts {
		out = append(out, ev(EvRestarts, "from", prev.Restarts, "to", cur.Restarts))
	}

	seen := map[string]bool{}
	for _, a := range prev.Asks {
		seen[a.ID] = true
	}
	for i := len(cur.Asks) - 1; i >= 0; i-- { // newest first in the probe
		if a := cur.Asks[i]; !seen[a.ID] {
			out = append(out, ev(EvAskOpened, "id", a.ID, "text", a.Text))
		}
	}
	for _, a := range cur.Asks {
		_, was := prev.Answers[a.ID]
		if _, is := cur.Answers[a.ID]; is && !was {
			out = append(out, ev(EvAskAnswered, "id", a.ID))
		}
	}

	for _, d := range newTail(prev.Deliveries, cur.Deliveries, cur.RecentDeliveries) {
		out = append(out, ev(EvDelivered, "seq", strconv.FormatUint(d.Seq, 10), "kind", d.Kind, "id", d.ID, "path", d.Path, "to", d.To))
	}
	for _, t := range newTail(prev.Tampers, cur.Tampers, cur.RecentTampers) {
		out = append(out, ev(EvTamper, "file", t.File, "reason", t.Reason))
	}

	was := map[string]bool{}
	for _, f := range prev.Mismatched(config) {
		was[f] = true
	}
	for _, f := range cur.Mismatched(config) {
		if !was[f] {
			out = append(out, ev(EvIntegrity, "file", f))
		}
	}

	if cur.Done && !prev.Done {
		out = append(out, ev(EvDone, "summary", cur.Summary))
	}
	if cur.Budget != "" && cur.Budget != prev.Budget {
		out = append(out, ev(EvBudget, "reason", cur.Budget))
	}
	if cur.Watchdog != "" && cur.Watchdog != prev.Watchdog {
		out = append(out, ev(EvWatchdog, "reason", cur.Watchdog))
	}
	if cur.StatusMD != prev.StatusMD && cur.StatusMD != "" {
		out = append(out, ev(EvStatus))
	}
	return out
}

// newTail is the lines of a log that are new since it had before lines, from
// its recent tail. A log that shrank was replaced; nothing in it is new.
func newTail[T any](before, now int, recent []T) []T {
	n := now - before
	if n <= 0 {
		return nil
	}
	if n > len(recent) {
		n = len(recent)
	}
	return recent[len(recent)-n:]
}
