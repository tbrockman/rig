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
		{"/root/.nix-profile/bin", "a nix-profile-installed agent is not on a transient unit's PATH"},
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
		// Resume must be decided from the transcript on disk, never from a
		// marker set before claude has actually created the session.
		"compgen -G", "$SID.jsonl",
		// All three credential shapes, so none is dropped by a later tidy-up.
		"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CREDENTIALS_B64",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("embedded runner is missing %q", want)
		}
	}
}

const failedRun = `{"type":"system","subtype":"init","session_id":"abc"}
{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}
{"type":"result","subtype":"success","result":"Failed to authenticate: OAuth session expired and could not be refreshed","errors":["Failed to authenticate: OAuth session expired and could not be refreshed"]}`

// An agent that cannot authenticate looks exactly like one that crashed: the
// unit restart-loops and last exit is 1. The reason must be reachable without
// knowing that events.jsonl exists.
func TestLastErrorFindsTheReasonARunStopped(t *testing.T) {
	got := LastError(failedRun)
	if !strings.Contains(got, "OAuth session expired") {
		t.Fatalf("did not surface the failure, got %q", got)
	}
}

// A healthy run must not be reported as an error.
func TestLastErrorIsSilentOnSuccess(t *testing.T) {
	ok := `{"type":"result","subtype":"success","result":"done","errors":[]}`
	if got := LastError(ok); got != "" {
		t.Fatalf("reported an error for a clean run: %q", got)
	}
}

// "OAuth session expired" does not say what to do, and the cause — that another
// consumer rotated the refresh token — is not guessable from the text.
func TestExplainErrorAddsTheCauseAndTheFix(t *testing.T) {
	got := ExplainError("Failed to authenticate: OAuth session expired and could not be refreshed")
	for _, want := range []string{"rotates", "rig restart", "setup-token"} {
		if !strings.Contains(got, want) {
			t.Errorf("explanation should mention %q, got:\n%s", want, got)
		}
	}
}

func TestExplainErrorLeavesOtherErrorsAlone(t *testing.T) {
	msg := "disk full"
	if ExplainError(msg) != msg {
		t.Error("an error it has nothing to add to must pass through unchanged")
	}
}
