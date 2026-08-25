package agent

import (
	"regexp"
	"strings"
	"testing"
)

// claude requires a real UUID for --session-id; an arbitrary string is rejected
// and the failure would only surface at launch.
func TestNewSessionIDIsAUUIDv4(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := NewSessionID()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(id) {
			t.Fatalf("not a v4 UUID: %s", id)
		}
		if seen[id] {
			t.Fatalf("duplicate session id: %s", id)
		}
		seen[id] = true
	}
}

func args(o UnitOpts) string { return strings.Join(SystemdRun(o), " ") }

// Each of these properties is load-bearing, and three of them were learned by
// losing a run. A test so that a future tidy-up cannot quietly drop one.
func TestSystemdRunCarriesTheSurvivalProperties(t *testing.T) {
	got := args(UnitOpts{Workdir: "/work/p", Session: "sid", Timeout: "6h",
		MemoryMax: "80%", Restarts: 5})

	for _, want := range []struct{ flag, why string }{
		{"--property=OOMPolicy=continue", "a killed child must not take the agent down with it"},
		{"--property=MemoryMax=80%", "the unit must hit its own cap before the guest OOMs globally"},
		{"--property=Restart=on-failure", "a crash must be recovered, not waited on"},
		{"--property=StartLimitBurst=5", "a flapping run must escalate rather than loop"},
		{"--setenv=RIG_AGENT_SESSION=sid", "the session must be fixed so a restart resumes"},
		{"--working-directory=/work/p", "claude scopes session lookup to the project dir"},
	} {
		if !strings.Contains(got, want.flag) {
			t.Errorf("missing %s — %s", want.flag, want.why)
		}
	}
}

// A transient unit is handed a PATH with no nix on it and dies instantly with
// "nix: command not found", which reads like a broken image.
func TestSystemdRunUsesALoginShell(t *testing.T) {
	got := args(UnitOpts{Workdir: "/work", Session: "s", Timeout: "1h", Restarts: 1})
	if !strings.Contains(got, "bash -lc") {
		t.Error("must run under a login shell so /etc/profile sets PATH")
	}
	if !strings.Contains(got, RunnerPath) {
		t.Error("must invoke the installed runner")
	}
}

func TestMemoryMaxIsOptional(t *testing.T) {
	got := args(UnitOpts{Workdir: "/work", Session: "s", Timeout: "1h", Restarts: 1})
	if strings.Contains(got, "MemoryMax") {
		t.Error("an empty MemoryMax must not emit the property at all")
	}
}

const stream = `{"type":"system","subtype":"init","session_id":"abc"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Gate E1 green."}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"..."}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Starting E2."}]}}
{"type":"result","subtype":"success","num_turns":2}`

// The whole point of the log verb: megabytes of tool calls in, only the agent's
// own words out.
func TestAssistantTextDropsEverythingButSpeech(t *testing.T) {
	got := AssistantText(stream, 0)
	want := []string{"Gate E1 green.", "Starting E2."}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, got[i], want[i])
		}
	}
}

// Tailing by bytes almost always cuts the first line in half. That must be
// skipped silently, not abort the parse and hide everything after it.
func TestAssistantTextSurvivesATruncatedFirstLine(t *testing.T) {
	got := AssistantText(`ype":"assistant","message":{"conte`+"\n"+stream, 0)
	if len(got) != 2 {
		t.Fatalf("a truncated leading line broke the parse: %v", got)
	}
}

func TestAssistantTextLimitKeepsTheMostRecent(t *testing.T) {
	got := AssistantText(stream, 1)
	if len(got) != 1 || got[0] != "Starting E2." {
		t.Fatalf("limit must keep the newest, got %v", got)
	}
}

func TestAssistantTextHandlesEmptyInput(t *testing.T) {
	if got := AssistantText("", 5); len(got) != 0 {
		t.Fatalf("want none, got %v", got)
	}
}

// The runner is embedded, so a missing or truncated file is a build-time-shaped
// bug that only shows up at launch.
func TestRunnerIsEmbeddedAndPlausible(t *testing.T) {
	r := string(Runner())
	for _, want := range []string{
		"--resume", "--session-id", "RIG_AGENT_SESSION",
		"IS_SANDBOX=1", "inbox", "projects", "last_exit",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("embedded runner is missing %q", want)
		}
	}
}
