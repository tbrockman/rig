package agent

import (
	"os"
	"os/exec"
	"path/filepath"
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
		{"--property=RestartPreventExitStatus=77", "a launch refused because nothing verifies must not be retried"},
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
// So it is run, against a directory laid out the way the guest lays it out.
func TestStatusProbeReadsTheChannel(t *testing.T) {
	d := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(d, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("last_exit", "0\n")
	write(DoneName, "all three gates green\n2026-10-16T10:00:00Z\n")
	write("STATUS.md", "# status\n")
	write(InboxName+"/000000000007.msg", "x")
	write(InboxName+"/.tmp-8", "half-written")
	write(LegacyInboxName, "old unsigned text")
	write(RunnerName, "runner")
	write(PubName, "key")
	write(AsksName+"/cuda.json", `{"id":"cuda","text":"Bump CUDA to 12.6?","time":"2026-10-16T09:00:00Z"}`+"\n")
	write(AsksName+"/docs.json", `{"id":"docs","text":"Which README sections?","time":"2026-10-16T08:00:00Z"}`+"\n")
	write(AnswersName+"/docs.msg", `{"v":1,"session":"s","seq":3,"time":"2026-10-16T08:30:00Z","kind":"answer","id":"docs","body":"Only the table."}`+"\nsig\n")
	write(DeliveriesName, `{"time":"t","seq":3,"kind":"answer","id":"docs","path":"hook"}`+"\n")
	write(TamperName, `{"time":"t","file":"rejected/x","reason":"bad signature"}`+"\n")

	out, err := exec.Command("bash", "-c", statusProbe(d)).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	p := ParseProbe(string(out))
	if !p.Done || p.Summary != "all three gates green" || p.Exited == 0 || p.Now == 0 || p.StatusMD == "" {
		t.Errorf("unit and markers: %+v", p)
	}
	if p.Pending != 1 {
		t.Errorf("pending = %d; a sender's temp file is not a message", p.Pending)
	}
	if !p.Legacy {
		t.Error("an older rig's unsigned inbox must be noticed")
	}
	if len(p.SHA256[RunnerName]) != 64 || len(p.SHA256[PubName]) != 64 || p.SHA256[BinName] != "" {
		t.Errorf("hashes: %v (a missing file hashes to nothing)", p.SHA256)
	}
	if open := p.OpenAsks(); len(open) != 1 || open[0].ID != "cuda" || open[0].Text != "Bump CUDA to 12.6?" {
		t.Errorf("open asks: %+v", open)
	}
	if p.Answers["docs"].Body != "Only the table." {
		t.Errorf("answers: %+v", p.Answers)
	}
	if p.Deliveries != 1 || len(p.RecentDeliveries) != 1 || p.RecentDeliveries[0].Path != "hook" {
		t.Errorf("deliveries: %d %+v", p.Deliveries, p.RecentDeliveries)
	}
	if p.Tampers != 1 || p.RecentTampers[0].Reason != "bad signature" {
		t.Errorf("tampers: %d %+v", p.Tampers, p.RecentTampers)
	}
	if StatusProbe() != statusProbe(Dir) {
		t.Error("the probe rig sends must be this one, at " + Dir)
	}
}

// What the guest wrote is shown on the operator's terminal and in watch's
// one-line events, so it cannot carry an escape sequence or a second line.
func TestCleanKeepsGuestTextToOneHarmlessLine(t *testing.T) {
	got := Clean("ok\x1b[2J\nfake=line\r\tend", 100)
	if strings.ContainsAny(got, "\x1b\n\r\t") {
		t.Errorf("control characters survived: %q", got)
	}
	if got := Clean("abcdef", 3); got != "abc…" {
		t.Errorf("cut: %q", got)
	}
}

// A recorded hash that the guest's file no longer matches is the one thing the
// host can see of a replaced verifier or key.
func TestMismatchedJudgesOnlyWhatWasRecorded(t *testing.T) {
	p := Probe{SHA256: map[string]string{RunnerName: "aa", BinName: "bb", PubName: ""}}
	cfg := map[string]string{Installed[0].Key: "aa", Installed[1].Key: "cc", Installed[2].Key: "dd"}
	got := p.Mismatched(cfg)
	if strings.Join(got, ",") != BinName+","+PubName {
		t.Errorf("got %v: a changed file and a missing one are both mismatches", got)
	}
	if got := p.Mismatched(map[string]string{}); len(got) != 0 {
		t.Errorf("nothing recorded judges nothing, got %v", got)
	}
}

// The agent calls `rig` by name, so the guest build must be the first one its
// PATH finds.
func TestTheGuestBuildIsFirstOnTheUnitPATH(t *testing.T) {
	if !strings.HasPrefix(UnitPATH, GuestBinDir+":") || GuestBinPath != GuestBinDir+"/rig" {
		t.Errorf("UnitPATH %s must start with %s", UnitPATH, GuestBinDir)
	}
}
