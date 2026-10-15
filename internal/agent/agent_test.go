package agent

import (
	"regexp"
	"strings"
	"testing"
)

// An agent CLI that takes a session id wants a real UUID; an arbitrary string
// is rejected and the failure would only surface at launch.
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
		{"--working-directory=/work/p", "the agent scopes session lookup to the project dir"},
		{"/root/.nix-profile/bin", "a nix-profile-installed agent is not on a transient unit's PATH"},
		{"--setenv=RIG_AGENT_PATH=" + UnitPATH, "the login shell discards PATH; the runner needs the unit's list by another name"},
	} {
		if !strings.Contains(got, want.flag) {
			t.Errorf("missing %s — %s", want.flag, want.why)
		}
	}
}

// The runner contract names the environment a harness may rely on. Every
// variable in it must actually be set, or a second harness written to the
// contract would be reading nothing.
func TestSystemdRunSetsTheContractEnvironment(t *testing.T) {
	got := args(UnitOpts{Workdir: "/work/p", Session: "sid", Timeout: "2h",
		Restarts: 1, UntilDone: true, Model: "fable"})
	for _, want := range []string{
		"--setenv=HOME=/root", "--setenv=TERM=dumb", "--setenv=PATH=" + UnitPATH,
		"--setenv=RIG_AGENT_PATH=" + UnitPATH, "--setenv=RIG_AGENT_WORKDIR=/work/p",
		"--setenv=RIG_AGENT_SESSION=sid", "--setenv=RIG_AGENT_TIMEOUT=2h",
		"--setenv=RIG_AGENT_UNTIL_DONE=1", "--setenv=RIG_AGENT_MODEL=fable",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
	// Unset, not empty: the runner tests presence, and an empty model would
	// be passed to the agent as a flag with no value.
	plain := args(UnitOpts{Workdir: "/work", Session: "s", Timeout: "1h", Restarts: 1})
	if strings.Contains(plain, "RIG_AGENT_MODEL") {
		t.Error("no model asked for must mean no RIG_AGENT_MODEL at all")
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

func TestPendingCountsMessagesNotLines(t *testing.T) {
	one := Delimiter + "\nline one\nline two\nline three\n"
	if got := Pending(one); got != 1 {
		t.Fatalf("a three-line message is one message, got %d", got)
	}
	if got := Pending(one + Delimiter + "\nanother\n"); got != 2 {
		t.Fatalf("two messages, got %d", got)
	}
	if got := Pending(""); got != 0 {
		t.Fatalf("an empty inbox has nothing pending, got %d", got)
	}
	// Written before delimiters existed: one message, not four.
	if got := Pending("a\nb\nc\nd\n"); got != 1 {
		t.Fatalf("a legacy inbox is one message, got %d", got)
	}
}

// The bug this replaced: `rig agent log` tailed raw bytes and filtered
// afterwards, so a window filled by one large tool result produced "nothing
// said yet" about an agent that was talking. Filtering has to happen in the
// guest, before the window is chosen.
func TestTailCmdFiltersBeforeItTails(t *testing.T) {
	got := TailCmd(`"type":"speech"`, 400000)
	grep := strings.Index(got, `grep -a '"type":"speech"' `+EventsPath)
	tail := strings.Index(got, "tail -c 400000")
	if grep < 0 || tail < 0 {
		t.Fatalf("expected a grep piped into a tail, got %q", got)
	}
	if grep > tail {
		t.Error("the tail must consume the filter's output, not the raw log — " +
			"otherwise tool traffic can fill the window and speech is reported as silence")
	}
	if !strings.Contains(got, NoEvents) {
		t.Error("must distinguish a missing log from a silent agent")
	}
}

func TestParseProbeReadsWhatSystemdSaid(t *testing.T) {
	p := ParseProbe("active=active\nrestarts=2\nstarted=1788214465\nnow=1788214665\nexited=1787955911\n")
	if p.Active != "active" || p.Restarts != "2" {
		t.Errorf("unit facts: %+v", p)
	}
	if p.Started != 1788214465 || p.Now != 1788214665 || p.Exited != 1787955911 {
		t.Errorf("clock facts: %+v", p)
	}
	if p.Exited >= p.Started {
		t.Error("this fixture is the real case: an exit older than the run reading it")
	}
}

// A probe that could not answer must leave zeros rather than a number status
// would present as fact.
func TestParseProbeLeavesUnanswerableFieldsZero(t *testing.T) {
	p := ParseProbe("active=inactive\nrestarts=\nstarted=\nnow=1788214665\nexited=\n")
	if p.Started != 0 || p.Exited != 0 || p.Restarts != "" {
		t.Errorf("missing values must stay empty, got %+v", p)
	}
}

func TestAgeIsCoarseAndShort(t *testing.T) {
	for _, c := range []struct {
		sec  int64
		want string
	}{{45, "45s"}, {600, "10m"}, {7300, "2h 1m"}, {258711, "2d 23h"}} {
		if got := Age(c.sec); got != c.want {
			t.Errorf("Age(%d) = %q, want %q", c.sec, got, c.want)
		}
	}
}

func TestUntilDoneIsOffByDefault(t *testing.T) {
	plain := strings.Join(SystemdRun(UnitOpts{Workdir: "/work", Session: "s", Timeout: "6h"}), " ")
	if strings.Contains(plain, "RIG_AGENT_UNTIL_DONE") {
		t.Error("the honest default for an unattended process is to stop when it says it is finished")
	}
	on := strings.Join(SystemdRun(UnitOpts{Workdir: "/work", Session: "s", Timeout: "6h", UntilDone: true}), " ")
	if !strings.Contains(on, "--setenv=RIG_AGENT_UNTIL_DONE=1") {
		t.Error("--until-done must reach the unit's environment")
	}
}

// Under --until-done, "unit inactive" means either "finished" or "ran out of
// restarts". Those want opposite reactions, so status has to be able to tell
// them apart.
func TestParseProbeReadsTheDoneMarker(t *testing.T) {
	if p := ParseProbe("active=inactive\nrestarts=3\ndone=yes\n"); !p.Done {
		t.Error("a present DONE marker must be reported as a completed mission")
	}
	if p := ParseProbe("active=active\nrestarts=0\ndone=\n"); p.Done {
		t.Error("an absent marker must not read as complete")
	}
}

// The probe is one round trip and its format is positional in the fmt string;
// a mismatched index silently produces a command that asks the wrong question.
func TestStatusProbeAsksAboutTheRightPaths(t *testing.T) {
	probe := StatusProbe()
	for _, want := range []string{Unit, Dir + "/last_exit", Dir + "/DONE"} {
		if !strings.Contains(probe, want) {
			t.Errorf("probe does not mention %q:\n%s", want, probe)
		}
	}
}
