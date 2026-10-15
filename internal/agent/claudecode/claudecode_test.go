package claudecode

import (
	"strings"
	"testing"

	"github.com/tbrockman/rig/internal/agent"
)

var h = Harness{}

// Importing this package is what makes "claude-code" exist; cmd/rig relies on
// it being the default.
func TestRegistersItselfAsTheDefault(t *testing.T) {
	if agent.Default().Name() != "claude-code" {
		t.Fatalf("the default harness is %q, want claude-code", agent.Default().Name())
	}
	got, err := agent.Lookup("claude-code")
	if err != nil || got.Binary() != "claude" {
		t.Fatalf("Lookup(claude-code) = %v, %v", got, err)
	}
}

// The order is the order the runner prefers them, and the proxy needs to know
// which of them it can stand in for.
func TestCredentialsSayWhatTheyAreAndWhereTheyGo(t *testing.T) {
	cs := h.Credentials()
	want := []struct {
		v         string
		endpoints []string
	}{
		{"CLAUDE_CODE_OAUTH_TOKEN", []string{"api.anthropic.com"}},
		{"ANTHROPIC_API_KEY", []string{"api.anthropic.com"}},
		{"CLAUDE_CREDENTIALS_B64", nil},
	}
	if len(cs) != len(want) {
		t.Fatalf("got %d credentials, want %d", len(cs), len(want))
	}
	for i, w := range want {
		if cs[i].Var != w.v {
			t.Errorf("[%d] %s, want %s (order is preference)", i, cs[i].Var, w.v)
		}
		if strings.Join(cs[i].Endpoints, ",") != strings.Join(w.endpoints, ",") {
			t.Errorf("%s endpoints %v, want %v", w.v, cs[i].Endpoints, w.endpoints)
		}
		if cs[i].Note == "" {
			t.Errorf("%s has no note for the operator", w.v)
		}
	}
	// The snapshot is a JSON blob whose token cannot be matched as a substring
	// of a request; nil, not an empty list, is how that is said.
	if cs[2].Endpoints != nil {
		t.Error("CLAUDE_CREDENTIALS_B64 cannot be proxied; Endpoints must be nil")
	}
	// The runner must accept exactly these, or a credential the harness
	// advertises would be refused at launch with exit 78.
	r := string(h.Runner())
	for _, c := range cs {
		if !strings.Contains(r, c.Var) {
			t.Errorf("the runner does not read %s", c.Var)
		}
	}
}

// Node reads NODE_EXTRA_CA_CERTS and ignores SSL_CERT_FILE, so the bundle is
// the wrong file to hand it.
func TestTrustEnvIsNodeShaped(t *testing.T) {
	got := h.TrustEnv("/run/rig-ca/ca.pem", "/run/rig-ca/bundle.crt")
	if len(got) != 1 || got[0] != "NODE_EXTRA_CA_CERTS=/run/rig-ca/ca.pem" {
		t.Fatalf("got %v", got)
	}
}

func TestProbeLooksDownTheUnitPATH(t *testing.T) {
	p := h.Probe()
	if !strings.Contains(p, "PATH="+agent.UnitPATH) || !strings.Contains(p, "command -v claude") {
		t.Errorf("the preflight must search exactly the unit's PATH for claude, got %q", p)
	}
}

// NixOS's /etc/profile exports PATH absolutely, so the login shell the runner
// starts under throws the unit's PATH away. Measured: a unit given
// PATH=/marker/bin ran its login shell without it. The runner has to put the
// unit's list back, or the preflight checks a PATH nothing searches.
func TestRunnerRestoresTheUnitPATHAfterTheLoginShell(t *testing.T) {
	r := string(h.Runner())
	if !strings.Contains(r, `export PATH="$RIG_AGENT_PATH:$PATH"`) {
		t.Error("the runner must prepend the unit's PATH; the login shell has just replaced it")
	}
	// Prepend, never replace: the login shell's entries carry the TLS
	// certificate bundle and the nix store paths the agent's builds need.
	if strings.Contains(r, `export PATH="$RIG_AGENT_PATH"`) {
		t.Error("the login shell's PATH must be kept behind the unit's, not dropped")
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
	got := h.AssistantText(stream, 0)
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
	got := h.AssistantText(`ype":"assistant","message":{"conte`+"\n"+stream, 0)
	if len(got) != 2 {
		t.Fatalf("a truncated leading line broke the parse: %v", got)
	}
}

func TestAssistantTextLimitKeepsTheMostRecent(t *testing.T) {
	got := h.AssistantText(stream, 1)
	if len(got) != 1 || got[0] != "Starting E2." {
		t.Fatalf("limit must keep the newest, got %v", got)
	}
}

func TestAssistantTextHandlesEmptyInput(t *testing.T) {
	if got := h.AssistantText("", 5); len(got) != 0 {
		t.Fatalf("want none, got %v", got)
	}
}

// The runner is embedded, so a missing or truncated file is a build-time-shaped
// bug that only shows up at launch.
func TestRunnerIsEmbeddedAndPlausible(t *testing.T) {
	r := string(h.Runner())
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

// The runner contract, as the shared side states it: what the unit sets, what
// the runner must leave behind, and the exit code that means "keep going".
func TestRunnerHonoursTheContract(t *testing.T) {
	r := string(h.Runner())
	for _, want := range []string{
		"RIG_AGENT_PATH", "RIG_AGENT_WORKDIR", "RIG_AGENT_SESSION", "RIG_AGENT_TIMEOUT",
		"RIG_AGENT_UNTIL_DONE", "RIG_AGENT_MODEL",
		"/run/rig/env", `"$D/prompt"`, `"$D/inbox"`, `"$D/inbox.delivered"`,
		`"$D/events.jsonl"`, `"$D/stderr.log"`, `"$D/last_exit"`, `"$D/turn_boundary"`, `"$D/DONE"`,
		"exit 75", "exit 78",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("runner does not honour %q", want)
		}
	}
	if !strings.Contains(r, `D="${RIG_AGENT_DIR:-`+agent.Dir+`}"`) {
		t.Errorf("the runner's directory must be %s", agent.Dir)
	}
}

const failedRun = `{"type":"system","subtype":"init","session_id":"abc"}
{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}
{"type":"result","subtype":"success","result":"Failed to authenticate: OAuth session expired and could not be refreshed","errors":["Failed to authenticate: OAuth session expired and could not be refreshed"]}`

// An agent that cannot authenticate looks exactly like one that crashed: the
// unit restart-loops and last exit is 1. The reason must be reachable without
// knowing that events.jsonl exists.
func TestLastErrorFindsTheReasonARunStopped(t *testing.T) {
	got := h.LastError(failedRun)
	if !strings.Contains(got, "OAuth session expired") {
		t.Fatalf("did not surface the failure, got %q", got)
	}
}

// A healthy run must not be reported as an error.
func TestLastErrorIsSilentOnSuccess(t *testing.T) {
	ok := `{"type":"result","subtype":"success","result":"done","errors":[]}`
	if got := h.LastError(ok); got != "" {
		t.Fatalf("reported an error for a clean run: %q", got)
	}
}

// "OAuth session expired" does not say what to do, and the cause — that another
// consumer rotated the refresh token — is not guessable from the text.
func TestExplainErrorAddsTheCauseAndTheFix(t *testing.T) {
	got := h.ExplainError("Failed to authenticate: OAuth session expired and could not be refreshed")
	// `rig creds`, not `rig restart`: the whole point of that verb is that a
	// stale credential must not cost the VM, and the agent's own error is the
	// place an operator learns it exists.
	for _, want := range []string{"rotates", "rig creds", "setup-token"} {
		if !strings.Contains(got, want) {
			t.Errorf("explanation should mention %q, got:\n%s", want, got)
		}
	}
}

func TestExplainErrorLeavesOtherErrorsAlone(t *testing.T) {
	msg := "disk full"
	if h.ExplainError(msg) != msg {
		t.Error("an error it has nothing to add to must pass through unchanged")
	}
}

// The bug this replaced: `rig agent log` tailed raw bytes and filtered
// afterwards, so a window filled by one large tool result produced "nothing
// said yet" about an agent that was talking. Filtering has to happen in the
// guest, before the window is chosen.
func TestTailSpeechFiltersBeforeItTails(t *testing.T) {
	got := h.TailSpeechCmd(400000)
	grep := strings.Index(got, `grep -a '"type":"assistant"'`)
	tail := strings.Index(got, "tail -c 400000")
	if grep < 0 || tail < 0 {
		t.Fatalf("expected a grep piped into a tail, got %q", got)
	}
	if grep > tail {
		t.Error("the tail must consume the filter's output, not the raw log — " +
			"otherwise tool traffic can fill the window and speech is reported as silence")
	}
	if !strings.Contains(got, agent.NoEvents) {
		t.Error("must distinguish a missing log from a silent agent")
	}
}

// Same window problem on the other read: a crash whose last bytes are tool
// output would report no reason for stopping at all.
func TestTailResultsFiltersForResultEvents(t *testing.T) {
	got := h.TailResultsCmd(20000)
	if !strings.Contains(got, `grep -a '"type":"result"'`) {
		t.Errorf("must prefilter for result events, got %q", got)
	}
}

// The runner used to keep its own restart counter in /var/lib, which outlived
// reboots and missions: a deliberate start after three-day-old failures opened
// with "restart #2, the previous process exited unexpectedly". systemd's
// NRestarts is scoped to the invocation and is the only honest source.
func TestRunnerTakesItsRestartCountFromSystemd(t *testing.T) {
	r := string(h.Runner())
	if !strings.Contains(r, "-p NRestarts --value") {
		t.Error("must ask systemd how many times it restarted")
	}
	if strings.Contains(r, `> "$D/restarts"`) {
		t.Error("must not keep a second restart counter; a file outlives the unit it describes")
	}
	if !strings.Contains(r, "restarted after a failure") {
		t.Error("a real restart must still be announced as one")
	}
	if !strings.Contains(r, "started deliberately") {
		t.Error("a deliberate start must not be reported to the agent as a crash")
	}
	// Both paths still ask for reconciliation: an interrupted tool call is lost
	// whether the process was killed or stopped.
	if strings.Count(r, "reconcile it with what") != 2 {
		t.Error("both resume notes must ask the agent to reconcile its tree")
	}
}

// The real event that exposed this: a mission that finished cleanly, whose
// summary happened to contain the word "failed" in ordinary prose. rig printed
// the whole success report to the operator under "last error".
const succeededRunMentioningFailure = `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":410,"result":"Done. Three commits, each at a green make check. A diagnosis that a fix failed to confirm is a diagnosis to re-measure."}`

func TestLastErrorIgnoresTheWordFailedInASuccessfulSummary(t *testing.T) {
	if got := h.LastError(succeededRunMentioningFailure); got != "" {
		t.Errorf("a successful run has no last error; got %q", got)
	}
}

// is_error is the other half: a failure with no errors[] must still be named.
func TestLastErrorReportsAnIsErrorResult(t *testing.T) {
	const s = `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"the model hit an unrecoverable error"}`
	if got := h.LastError(s); !strings.Contains(got, "unrecoverable") {
		t.Errorf("an is_error result must be surfaced, got %q", got)
	}
}

// A failure that was retried and then succeeded must not be reported as the
// reason the agent stopped — the last result is the one that stopped it.
func TestLastErrorIsClearedByALaterSuccess(t *testing.T) {
	s := failedRun + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"done"}`
	if got := h.LastError(s); got != "" {
		t.Errorf("a later clean result ends the story; got %q", got)
	}
}

// --- until-done ----------------------------------------------------------

// `claude -p` returns when the model stops talking, which for a multi-mission
// brief is a turn boundary rather than a result. Under --until-done the runner
// has to say "not finished" to systemd so the restart resumes the conversation.
func TestRunnerResumesACleanExitOnlyWhenAskedTo(t *testing.T) {
	r := string(h.Runner())
	if !strings.Contains(r, "RIG_AGENT_UNTIL_DONE") {
		t.Fatal("the runner must read the opt-in; without it this changes every run's behaviour")
	}
	if !strings.Contains(r, "exit 75") {
		t.Error("a turn boundary must exit non-zero, or Restart=on-failure will not resume it")
	}
	// The agent's own marker is the terminating condition. Anything rig decided
	// for itself would be guessing at "finished", which is the mistake the old
	// shell loop made.
	if !strings.Contains(r, `[ ! -e "$D/DONE" ]`) {
		t.Error("only the agent's own DONE marker may end an --until-done run")
	}
}

// The failure this guards: a turn boundary and a crash are both a non-zero exit
// and both bump NRestarts, so without a note the agent is told its process died
// every time it simply stopped talking — and spends the next turn reconciling a
// working tree that nothing interrupted.
func TestRunnerDistinguishesATurnBoundaryFromACrash(t *testing.T) {
	r := string(h.Runner())
	if !strings.Contains(r, "turn_boundary") {
		t.Fatal("the runner must leave a note saying which kind of exit it was")
	}
	if !strings.Contains(r, "Nothing crashed and nothing was interrupted") {
		t.Error("a resumed turn must not be announced to the agent as a crash")
	}
	// Read once and cleared, or a genuine crash after a turn boundary inherits
	// the wrong explanation.
	if !strings.Contains(r, `rm -f "$D/turn_boundary"`) {
		t.Error("the note must be cleared by its reader")
	}
	if strings.Contains(r, "restarted after a failure") && !strings.Contains(r, "TURN_BOUNDARY") {
		t.Error("the crash note must be reached only when the exit was not a turn boundary")
	}
}
