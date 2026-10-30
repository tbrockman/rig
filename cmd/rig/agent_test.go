package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tbrockman/rig/internal/agent"
	"github.com/tbrockman/rig/internal/manifest"
)

// A bare `rig agent start` resumes the run as it was started: forgetting
// --until-done on a restart once made an agent stop after its next turn.
func TestStartOptionsAreTheFlagThenTheRecordThenTheDefault(t *testing.T) {
	recorded := agent.StartOptions{PromptFile: "/home/me/brief.md", Workdir: "/work/p", Model: "fable",
		Timeout: "2h", MemoryMax: "50%", UntilDone: true, Restarts: 30}
	defaults := agent.StartOptions{Workdir: "/work", Timeout: "6h", MemoryMax: "80%"}
	nothing := func(string) bool { return false }

	got, err := startOptions(nothing, recorded, agent.StartOptions{}, defaults)
	if err != nil || got != recorded {
		t.Fatalf("a bare start must take what was recorded: %+v, %v", got, err)
	}

	// Flags given now win, even when they say "the default": --model "" is
	// the guest's default model, and --until-done=false turns it off.
	given := defaults
	given.Workdir = "/work/q"
	changed := func(f string) bool { return f == "model" || f == "until-done" || f == "workdir" }
	got, _ = startOptions(changed, recorded, agent.StartOptions{}, given)
	want := recorded
	want.Model, want.UntilDone, want.Workdir = "", false, "/work/q"
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}

	// Nothing recorded: the flags' defaults, and a brief is required.
	if _, err := startOptions(nothing, agent.StartOptions{}, agent.StartOptions{}, defaults); err == nil ||
		!strings.Contains(err.Error(), "--prompt-file") {
		t.Errorf("a first start without a brief must say what is missing, got %v", err)
	}
	t.Chdir(t.TempDir())
	given = defaults
	given.PromptFile = "brief.md"
	got, _ = startOptions(func(f string) bool { return f == "prompt-file" }, agent.StartOptions{}, agent.StartOptions{}, given)
	if !filepath.IsAbs(got.PromptFile) || got.EffectiveRestarts() != agent.DefaultRestarts(false) {
		t.Errorf("the brief is recorded absolute, and the budget is the mode's: %+v", got)
	}

	// The manifest's guest.agent is the layer under the record: a first start
	// reads it whole, a flag given now still wins, and a record from an
	// earlier start stands in for it until rig apply -f clears the record.
	fromFile := manifestDefaults(manifest.Agent{PromptFile: "/home/me/proj/brief.md", Workdir: "/work/proj",
		Model: "claude-fable-5-1", Timeout: "4h", MemoryMax: "70%", UntilDone: true})
	got, err = startOptions(nothing, agent.StartOptions{}, fromFile, defaults)
	if err != nil || got != fromFile {
		t.Errorf("a first start takes the manifest's settings: %+v, %v", got, err)
	}
	got, _ = startOptions(func(f string) bool { return f == "model" }, agent.StartOptions{}, fromFile, given)
	if got.Model != "" || got.Workdir != "/work/proj" || !got.UntilDone {
		t.Errorf("a flag wins over the file, the rest is the file's: %+v", got)
	}
	got, _ = startOptions(nothing, recorded, fromFile, defaults)
	if got != recorded {
		t.Errorf("the last start's record wins over the file until apply clears it: %+v", got)
	}
}

func TestAgentVerbsSayWhatTheyDoNow(t *testing.T) {
	a := &app{}
	start := a.agentStartCmd()
	if ann := start.Flags().Lookup("prompt-file").Annotations; len(ann) != 0 {
		t.Error("--prompt-file is recorded, so a resume must not require it")
	}
	// The old help promised delivery "if it is polling"; nothing polled.
	if send := a.agentSendCmd(); strings.Contains(send.Long, "polling") || !strings.Contains(send.Long, "next tool") {
		t.Errorf("send's help:\n%s", send.Long)
	}
	for _, c := range []string{a.agentSendCmd().Use, a.agentAnswerCmd().Use, a.agentAsksCmd().Use,
		a.agentBriefCmd().Use, a.agentWatchCmd().Use, start.Use, a.agentStatusCmd().Use} {
		if !strings.Contains(c, "<vm>") {
			t.Errorf("%q must name its VM operand", c)
		}
	}
	watch := a.agentWatchCmd().Long
	for _, ev := range []string{agent.EvWatching, agent.EvAskOpened, agent.EvAskAnswered, agent.EvDelivered,
		agent.EvTamper, agent.EvIntegrity, agent.EvDone, agent.EvUnit, agent.EvRestarts, agent.EvStatus,
		agent.EvUnreachable, "RIG_EVENT"} {
		if !strings.Contains(watch, ev) {
			t.Errorf("watch's help does not document %s", ev)
		}
	}
}

func TestDiffStatCountsChangedLinesOnly(t *testing.T) {
	d := agent.UnifiedDiff("a", "b", "x\ny\nz\n", "x\nY\nz\nw\n")
	if a, r := diffStat(d); a != 2 || r != 1 {
		t.Errorf("+%d -%d in\n%s", a, r, d)
	}
}
