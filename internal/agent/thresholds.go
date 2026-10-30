package agent

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Soft thresholds: facts about a run that rig observes without the
// harness's help, each with the point past which the operator wants to
// hear. One mission's overseer computed "nothing landed for two hours", "a
// gate unit has run 50 minutes" and "usage at 70% of the window" by hand,
// with a shell script around status, git, systemctl and a usage endpoint.
// Now each crossing and clearing is a watch event and a status line, and
// nothing here stops the agent: that is what budgets are for.

// Thresholds is what guest.agent.watch asks to be watched; zero is unset.
type Thresholds struct {
	Idle     time.Duration // no new commit on the workdir's branch for this long
	Unit     time.Duration // a transient unit the agent started has run this long
	Status   time.Duration // STATUS.md untouched for this long
	Usage    *UsageLimit   // a subscription window's utilization
	Forwards []int         // declared forwards, each expected to have a listener in the guest
}

// UsageLimit is a subscription window and the fraction of it.
type UsageLimit struct {
	Window string
	Max    float64
}

// The names of the thresholds, as readings and `on:` keys use them.
const (
	ThrIdle    = "idle"
	ThrUnit    = "unit"
	ThrStatus  = "status"
	ThrUsage   = "usage"
	ThrForward = "forward"
)

// Reading is one threshold's state at one probe.
type Reading struct {
	Name   string // ThrIdle and the others
	Detail string // what it is about, when there are several: a unit's name, a forward's port
	Known  bool   // the fact could be read; false is "no git in the workdir", "no quota for this credential"
	Over   bool   // past the threshold
	Value  string // for the operator and the event: "130m", "0.72", "no listener"
	Limit  string // likewise: "120m", "0.70"
}

// Key tells readings of one threshold apart when there are several.
func (r Reading) Key() string { return r.Name + "/" + r.Detail }

// Evaluate reads every threshold off a probe (and a quota, when there is
// one; holderUp is whether the host's forward holder runs). Readings are in
// a fixed order, units and forwards sorted, so two evaluations compare.
func Evaluate(t Thresholds, p Probe, q *Quota, holderUp bool) []Reading {
	var out []Reading
	now := time.Unix(p.Now, 0)
	minutes := func(d time.Duration) string { return strconv.Itoa(int(d.Minutes())) + "m" }
	if t.Idle > 0 {
		r := Reading{Name: ThrIdle, Limit: minutes(t.Idle)}
		if p.Head > 0 && p.Now > 0 {
			age := now.Sub(time.Unix(p.Head, 0))
			r.Known, r.Over, r.Value = true, age > t.Idle, minutes(age)
		} else {
			r.Value = "no commit to read"
		}
		out = append(out, r)
	}
	if t.Unit > 0 {
		units := append([]TransientUnit(nil), p.Units...)
		sort.Slice(units, func(i, j int) bool { return units[i].Name < units[j].Name })
		for _, u := range units {
			r := Reading{Name: ThrUnit, Detail: u.Name, Limit: minutes(t.Unit), Known: true}
			if u.Since > 0 && p.Now > 0 {
				age := now.Sub(time.Unix(u.Since, 0))
				r.Over, r.Value = age > t.Unit, minutes(age)
			} else {
				r.Known, r.Value = false, "running, age unknown"
			}
			out = append(out, r)
		}
	}
	if t.Status > 0 {
		r := Reading{Name: ThrStatus, Limit: minutes(t.Status)}
		if mtime := statusMTime(p.StatusMD); mtime > 0 && p.Now > 0 {
			age := now.Sub(time.Unix(mtime, 0))
			r.Known, r.Over, r.Value = true, age > t.Status, minutes(age)
		} else {
			r.Value = "no STATUS.md"
		}
		out = append(out, r)
	}
	if t.Usage != nil {
		r := Reading{Name: ThrUsage, Detail: t.Usage.Window, Limit: fmt.Sprintf("%.2f", t.Usage.Max)}
		if w, ok := q.Window(t.Usage.Window); ok {
			r.Known, r.Over, r.Value = true, w.Utilization > t.Usage.Max, fmt.Sprintf("%.2f", w.Utilization)
		} else {
			r.Value = "no quota for this credential"
		}
		out = append(out, r)
	}
	ports := append([]int(nil), t.Forwards...)
	sort.Ints(ports)
	for _, port := range ports {
		r := Reading{Name: ThrForward, Detail: strconv.Itoa(port), Known: true, Limit: "listening"}
		switch {
		case !holderUp:
			r.Over, r.Value = true, "holder not running on the host"
		case p.Listening != nil && !p.Listening[port]:
			r.Over, r.Value = true, "nothing listens on it in the guest"
		case p.Listening == nil:
			r.Known, r.Value = false, "the guest did not say what listens"
		default:
			r.Value = "listening"
		}
		out = append(out, r)
	}
	return out
}

// statusMTime reads the mtime out of the probe's "size.mtime" for STATUS.md.
func statusMTime(statusMD string) int64 {
	for i := len(statusMD) - 1; i >= 0; i-- {
		if statusMD[i] == '.' {
			n, _ := strconv.ParseInt(statusMD[i+1:], 10, 64)
			return n
		}
	}
	return 0
}

// ThresholdEvents is what changed between two evaluations: a threshold
// crossed, or cleared. With no previous evaluation, every threshold already
// over is reported as crossed, since a watcher started after the fact must
// still hear of it. One that cannot be read says nothing.
func ThresholdEvents(prev, cur []Reading) []Event {
	was := map[string]Reading{}
	for _, r := range prev {
		was[r.Key()] = r
	}
	var out []Event
	for _, r := range cur {
		if !r.Known {
			continue
		}
		before, seen := was[r.Key()]
		if seen && before.Known && before.Over == r.Over {
			continue
		}
		if !r.Over && (!seen || !before.Known) {
			continue // under, and nothing was said before: nothing to clear
		}
		out = append(out, r.Event())
	}
	return out
}

// Event renders a reading as a watch event: its threshold's name, crossed or
// cleared, the value and the limit, and what it is about.
func (r Reading) Event() Event {
	state := "crossed"
	if !r.Over {
		state = "cleared"
	}
	switch r.Name {
	case ThrUnit:
		return ev(EvLongUnit, "state", state, "unit", r.Detail, "age", r.Value, "limit", r.Limit)
	case ThrUsage:
		return ev(EvUsage, "state", state, "window", r.Detail, "utilization", r.Value, "limit", r.Limit)
	case ThrForward:
		if r.Over {
			return ev(EvForwardDown, "port", r.Detail, "reason", r.Value)
		}
		return ev(EvForwardUp, "port", r.Detail)
	case ThrStatus:
		return ev(EvStaleStatus, "state", state, "age", r.Value, "limit", r.Limit)
	}
	return ev(EvIdle, "state", state, "age", r.Value, "limit", r.Limit)
}

// Line is a reading for `rig agent status --full`.
func (r Reading) Line() string {
	what := r.Name
	if r.Detail != "" {
		what += " " + r.Detail
	}
	switch {
	case !r.Known:
		return fmt.Sprintf("%-22s ?        %s (limit %s)", what, r.Value, r.Limit)
	case r.Over:
		return fmt.Sprintf("%-22s OVER     %s (limit %s)", what, r.Value, r.Limit)
	}
	return fmt.Sprintf("%-22s ok       %s (limit %s)", what, r.Value, r.Limit)
}
