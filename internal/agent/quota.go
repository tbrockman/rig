package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Usage has two kinds, kept apart. Consumption (harness.go) is tokens, from
// the harness's own event log, which every harness has. A quota is a
// subscription's window: a fraction used and a time it resets, read from
// the provider with the credential, and it exists only for some
// credentials: an OAuth login has one, an API key has none. A threshold or
// a budget names which kind it means, and the watch evaluates what exists.

// Quota is what the provider says of a credential's windows.
type Quota struct {
	Windows []Window
	Fetched time.Time
	Source  string // whose credential answered (QuotaRequest.Source)
}

// Window is one rate-limit window: five_hour, seven_day, and whatever else
// the provider reports.
type Window struct {
	Name        string
	Utilization float64 // 0 to 1
	ResetsAt    time.Time
}

// Window finds a window by name; ok is false when the quota has none, or
// there is no quota.
func (q *Quota) Window(name string) (Window, bool) {
	if q == nil {
		return Window{}, false
	}
	for _, w := range q.Windows {
		if w.Name == name {
			return w, true
		}
	}
	return Window{}, false
}

// String is the quota for an operator, one window per line.
func (q *Quota) String() string {
	if q == nil || len(q.Windows) == 0 {
		return "no quota: the credential has no subscription window (an API key), or none was read"
	}
	ws := append([]Window(nil), q.Windows...)
	sort.Slice(ws, func(i, j int) bool { return ws[i].Name < ws[j].Name })
	var lines []string
	for _, w := range ws {
		reset := ""
		if !w.ResetsAt.IsZero() {
			reset = fmt.Sprintf(", resets %s (in %s)", w.ResetsAt.UTC().Format(time.RFC3339), Age(int64(time.Until(w.ResetsAt).Seconds())))
		}
		lines = append(lines, fmt.Sprintf("%-18s %3.0f%%%s", w.Name, w.Utilization*100, reset))
	}
	return strings.Join(lines, "\n")
}
