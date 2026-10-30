package guest

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tbrockman/rig/internal/agent"
)

// A harness whose event log is one number per line: the tokens that line
// consumed. Enough for the watchdog, which only asks for the total.
type countingHarness struct{ stub }

type stub struct{}

func (stub) Name() string                                                  { return "counting" }
func (stub) Binary() string                                                { return "x" }
func (stub) InstallHint() string                                           { return "" }
func (stub) Probe() string                                                 { return "" }
func (stub) Runner() []byte                                                { return nil }
func (stub) Credentials() []agent.Credential                               { return nil }
func (stub) TrustEnv(_, _ string) []string                                 { return nil }
func (stub) LastError(string) string                                       { return "" }
func (stub) ExplainError(m string) string                                  { return m }
func (stub) AssistantText(string, int) []string                            { return nil }
func (stub) TailSpeechCmd(int) string                                      { return "" }
func (stub) TailResultsCmd(int) string                                     { return "" }
func (stub) Delivery() []agent.DeliveryPath                                { return agent.CommonDelivery() }
func (stub) Usage(io.Reader) (agent.Consumption, bool)                     { return agent.Consumption{}, false }
func (stub) QuotaRequests(map[string]string) ([]agent.QuotaRequest, error) { return nil, nil }

func (countingHarness) Usage(events io.Reader) (agent.Consumption, bool) {
	buf, _ := io.ReadAll(events)
	var c agent.Consumption
	for _, line := range strings.Fields(string(buf)) {
		var n int64
		for _, r := range line {
			n = n*10 + int64(r-'0')
		}
		c.Output += n
	}
	return c, true
}

func TestWatchdogChecksBudgetsBeforeIdle(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f.box.Now = func() time.Time { return now }
	events := filepath.Join(f.box.Dir, "events.jsonl")

	// Nothing set: nothing fires, however long it has been.
	w := NewWatchdog(f.box, countingHarness{}, Limits{})
	now = now.Add(48 * time.Hour)
	if fired, ok := w.Check(); ok {
		t.Fatalf("no limits, yet: %+v", fired)
	}

	// Idle counts from the watchdog's start, not from an old log.
	os.WriteFile(events, []byte("10\n"), 0o600)
	old := now.Add(-3 * time.Hour)
	os.Chtimes(events, old, old)
	w = NewWatchdog(f.box, countingHarness{}, Limits{Idle: 20 * time.Minute})
	if fired, ok := w.Check(); ok {
		t.Fatalf("a log older than the run is not idleness of this run: %+v", fired)
	}
	now = now.Add(21 * time.Minute)
	fired, ok := w.Check()
	if !ok || fired.Budget || !strings.HasPrefix(fired.Reason, "idle: nothing new in the event log for 21m") {
		t.Fatalf("got %+v %v", fired, ok)
	}
	// A new event resets it.
	os.Chtimes(events, now, now)
	if fired, ok := w.Check(); ok {
		t.Fatalf("the log just grew, yet: %+v", fired)
	}

	// Hours is a budget, and judged before idle.
	w = NewWatchdog(f.box, countingHarness{}, Limits{Idle: time.Minute, Hours: 2, Started: now.Add(-3 * time.Hour)})
	now = now.Add(5 * time.Minute)
	fired, ok = w.Check()
	if !ok || !fired.Budget || !strings.HasPrefix(fired.Reason, "hours: 3.1 of 2") {
		t.Fatalf("got %+v %v", fired, ok)
	}

	// Tokens too, from the harness's count.
	os.WriteFile(events, []byte("10\n20\n30\n"), 0o600)
	w = NewWatchdog(f.box, countingHarness{}, Limits{Tokens: 50})
	fired, ok = w.Check()
	if !ok || !fired.Budget || fired.Reason != "tokens: 60 of 50 over the session" {
		t.Fatalf("got %+v %v", fired, ok)
	}
	if err := w.Mark(fired); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(f.box.Dir, agent.BudgetName))
	if first, _, _ := strings.Cut(string(raw), "\n"); first != fired.Reason {
		t.Errorf("BUDGET's first line is the reason, got %q", raw)
	}
	w.Mark(Fired{Reason: "idle: x"})
	if _, err := os.Stat(filepath.Join(f.box.Dir, agent.WatchdogName)); err != nil {
		t.Error("a run's limit leaves the watchdog marker")
	}
}

// Run ends the process it was given when a limit fires, and says so.
func TestWatchdogEndsTheRun(t *testing.T) {
	f := newFixture(t)
	f.box.Now = time.Now // the fixture's clock stands still, and idleness needs one that runs
	sleep := exec.Command("sleep", "60")
	if err := sleep.Start(); err != nil {
		t.Skip("no sleep:", err)
	}
	defer sleep.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- sleep.Wait() }()

	w := NewWatchdog(f.box, countingHarness{}, Limits{Idle: 50 * time.Millisecond})
	fired, ok := w.Run(make(chan struct{}), sleep.Process.Pid, 20*time.Millisecond)
	if !ok || !strings.HasPrefix(fired.Reason, "idle:") {
		t.Fatalf("got %+v %v", fired, ok)
	}
	select {
	case err := <-done:
		if ee, _ := err.(*exec.ExitError); ee == nil || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
			t.Errorf("the run must be ended with SIGTERM, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run was not ended")
	}
	if _, err := os.Stat(filepath.Join(f.box.Dir, agent.WatchdogName)); err != nil {
		t.Error("no marker left")
	}

	// Stopped by the runner: nothing fires, nothing is marked.
	stop := make(chan struct{})
	close(stop)
	if _, ok := NewWatchdog(f.box, countingHarness{}, Limits{Idle: time.Hour}).Run(stop, 0, time.Millisecond); ok {
		t.Error("a stopped watchdog fires nothing")
	}
}

func TestLimitsFromEnv(t *testing.T) {
	t.Setenv("RIG_AGENT_IDLE_TIMEOUT", "20m")
	t.Setenv("RIG_AGENT_BUDGET_HOURS", "24")
	t.Setenv("RIG_AGENT_BUDGET_TOKENS", "50000000")
	t.Setenv("RIG_AGENT_STARTED", "1793390400")
	l, err := LimitsFromEnv()
	if err != nil || l.Idle != 20*time.Minute || l.Hours != 24 || l.Tokens != 50000000 || l.Started.Unix() != 1793390400 {
		t.Errorf("got %+v, %v", l, err)
	}
	t.Setenv("RIG_AGENT_BUDGET_HOURS", "a day")
	if _, err := LimitsFromEnv(); err == nil {
		t.Error("an unreadable limit must be an error, not no limit")
	}
}
