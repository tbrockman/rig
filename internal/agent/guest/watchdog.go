package guest

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tbrockman/rig/internal/agent"
)

// The run watchdog, and the budgets the guest can enforce by itself. The
// runner starts it beside the agent (`rig hook watchdog --pid`), and asks it
// once before a launch (`--check`). It kills the run and leaves a marker:
// `watchdog` for a limit on one run (idle, which the runner also writes for
// the outer timeout), which the runner resumes with a note naming it;
// `BUDGET` for a limit on the mission (hours, tokens), which the runner
// treats as `rig agent stop` would, the session kept. Enforced here, in
// the guest, so the stop depends on nobody watching from the host; what
// needs the credential (a subscription window) stays with `rig agent
// watch`.

// Limits is what the unit's environment says the run may not exceed.
type Limits struct {
	Idle    time.Duration // RIG_AGENT_IDLE_TIMEOUT: a run with nothing new in its event log for this long is killed and resumed
	Hours   float64       // RIG_AGENT_BUDGET_HOURS: wall-clock since Started
	Tokens  int64         // RIG_AGENT_BUDGET_TOKENS: Consumption.Total over the session
	Started time.Time     // RIG_AGENT_STARTED, unix seconds: when rig agent start ran
}

// LimitsFromEnv reads the unit's environment. A variable left out is no
// limit of that kind.
func LimitsFromEnv() (Limits, error) {
	var l Limits
	var err error
	if v := os.Getenv("RIG_AGENT_IDLE_TIMEOUT"); v != "" {
		if l.Idle, err = time.ParseDuration(v); err != nil {
			return l, fmt.Errorf("RIG_AGENT_IDLE_TIMEOUT: %w", err)
		}
	}
	if v := os.Getenv("RIG_AGENT_BUDGET_HOURS"); v != "" {
		if l.Hours, err = strconv.ParseFloat(v, 64); err != nil {
			return l, fmt.Errorf("RIG_AGENT_BUDGET_HOURS: %w", err)
		}
	}
	if v := os.Getenv("RIG_AGENT_BUDGET_TOKENS"); v != "" {
		if l.Tokens, err = strconv.ParseInt(v, 10, 64); err != nil {
			return l, fmt.Errorf("RIG_AGENT_BUDGET_TOKENS: %w", err)
		}
	}
	if v := os.Getenv("RIG_AGENT_STARTED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return l, fmt.Errorf("RIG_AGENT_STARTED: %w", err)
		}
		l.Started = time.Unix(n, 0)
	}
	return l, nil
}

// Watchdog watches one run of the agent in Box against Limits, reading the
// event log with the harness's parser.
type Watchdog struct {
	Box
	Harness agent.Harness
	Limits  Limits
	// since is when the watchdog started: the idle clock cannot start
	// earlier, since the event log's mtime is the previous run's end.
	since time.Time
	// tokens are checked when the log has grown, and not more often than
	// every few minutes: the log is read whole each time.
	lastSize    int64
	lastTokens  time.Time
	tokensTotal int64
}

// NewWatchdog is a watchdog for b, starting its idle clock now.
func NewWatchdog(b Box, h agent.Harness, l Limits) *Watchdog {
	return &Watchdog{Box: b, Harness: h, Limits: l, since: b.now()}
}

// ErrNoEvents is a usage read with no event log yet.
var ErrNoEvents = errors.New("no event log yet")

// Usage is what the harness says the session has consumed, from the event
// log on disk.
func (b Box) Usage(h agent.Harness) (agent.Consumption, error) {
	f, err := os.Open(b.path("events.jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return agent.Consumption{}, ErrNoEvents
		}
		return agent.Consumption{}, err
	}
	defer f.Close()
	c, ok := h.Usage(f)
	if !ok {
		return c, fmt.Errorf("the %s harness logs no usage", h.Name())
	}
	return c, nil
}

// Fired is what a check found: the reason, one line, and whether it is a
// budget (the mission's) rather than a run's limit.
type Fired struct {
	Reason string
	Budget bool
}

// Check looks once. A budget is looked at before the idle clock: a run that
// is both idle and over budget stops for good, not for a resume.
func (w *Watchdog) Check() (Fired, bool) {
	now := w.now()
	if w.Limits.Hours > 0 && !w.Limits.Started.IsZero() {
		if since := now.Sub(w.Limits.Started); since.Hours() > w.Limits.Hours {
			return Fired{Reason: fmt.Sprintf("hours: %.1f of %g since rig agent start (%s)", since.Hours(), w.Limits.Hours, agent.Stamp(w.Limits.Started)), Budget: true}, true
		}
	}
	if w.Limits.Tokens > 0 {
		if total, ok := w.tokens(now); ok && total > w.Limits.Tokens {
			return Fired{Reason: fmt.Sprintf("tokens: %s of %s over the session", agent.Thousands(total), agent.Thousands(w.Limits.Tokens)), Budget: true}, true
		}
	}
	if w.Limits.Idle > 0 {
		last := w.since
		if st, err := os.Stat(w.path("events.jsonl")); err == nil && st.ModTime().After(last) {
			last = st.ModTime()
		}
		if idle := now.Sub(last); idle > w.Limits.Idle {
			return Fired{Reason: fmt.Sprintf("idle: nothing new in the event log for %s (limit %s)", idle.Round(time.Second), w.Limits.Idle)}, true
		}
	}
	return Fired{}, false
}

// tokens is the session's total, re-read when the log has grown and at most
// every five minutes; ok is false when it could not be read.
func (w *Watchdog) tokens(now time.Time) (int64, bool) {
	st, err := os.Stat(w.path("events.jsonl"))
	if err != nil {
		return 0, false
	}
	if st.Size() == w.lastSize && !w.lastTokens.IsZero() {
		return w.tokensTotal, true
	}
	if !w.lastTokens.IsZero() && now.Sub(w.lastTokens) < 5*time.Minute {
		return w.tokensTotal, true
	}
	c, err := w.Usage(w.Harness)
	if err != nil {
		return 0, false
	}
	w.lastSize, w.lastTokens, w.tokensTotal = st.Size(), now, c.Total()
	return c.Total(), true
}

// Mark leaves what fired where the runner and the host read it: BUDGET for
// a budget, watchdog for a run's limit. The first line is the reason.
func (w *Watchdog) Mark(f Fired) error {
	name := agent.WatchdogName
	if f.Budget {
		name = agent.BudgetName
	}
	return writeAtomic(w.path(name), []byte(f.Reason+"\n"+agent.Stamp(w.now())+"\n"), 0o644)
}

// Run checks every interval until something fires or stop is closed. When
// something fires it marks it, ends the run (SIGTERM to pid, SIGKILL after
// a grace period) and returns what fired. pid is what the runner started:
// timeout(1), which passes the signal on to the agent.
func (w *Watchdog) Run(stop <-chan struct{}, pid int, every time.Duration) (Fired, bool) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return Fired{}, false
		case <-t.C:
		}
		f, ok := w.Check()
		if !ok {
			continue
		}
		if err := w.Mark(f); err != nil {
			logErr(w.Box, "hook watchdog", err)
		}
		end(pid, 30*time.Second)
		return f, true
	}
}

// end stops a process: SIGTERM, then SIGKILL when it is still there after
// grace. A pid of 0 is nothing to end.
func end(pid int, grace time.Duration) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for deadline := time.Now().Add(grace); time.Now().Before(deadline); {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// logErr appends a line to stderr.log, where the runner's notes go.
func logErr(b Box, verb string, err error) {
	f, ferr := os.OpenFile(b.path("stderr.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if ferr != nil {
		fmt.Fprintf(os.Stderr, "rig %s: %v\n", verb, err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] rig %s: %v\n", agent.Stamp(b.now()), verb, strings.TrimSpace(err.Error()))
}
