package agent

import "testing"

// Recorded, read back, the same: a bare `rig agent start` resumes the run the
// way it was started.
func TestStartOptionsRoundTripThroughTheInstance(t *testing.T) {
	for _, o := range []StartOptions{
		{PromptFile: "/home/me/brief.md", Workdir: "/work/p", Model: "fable", Timeout: "6h",
			MemoryMax: "80%", UntilDone: true, Restarts: 30},
		{PromptFile: "/home/me/brief.md", Workdir: "/work", Timeout: "2h", MemoryMax: "50%"},
	} {
		cfg := map[string]string{"user.rig.agent.session": "s"}
		for k, v := range o.Config() {
			if v != "" {
				cfg[k] = v
			}
		}
		if got := RecordedOptions(cfg); got != o {
			t.Errorf("got %+v, want %+v", got, o)
		}
	}
}

// Nothing recorded is the zero value, which every flag reads as "my default".
// A cleared model and a budget left to the mode are recorded as nothing.
func TestUnrecordedOptionsAreZero(t *testing.T) {
	if got := RecordedOptions(map[string]string{}); got != (StartOptions{}) {
		t.Errorf("got %+v", got)
	}
	cfg := StartOptions{Workdir: "/work"}.Config()
	if cfg[OptionKey("model")] != "" || cfg[OptionKey("max-restarts")] != "" {
		t.Errorf("a default must be recorded as nothing, so SetConfigKeys removes it: %v", cfg)
	}
	for _, f := range OptionFlags {
		if _, ok := cfg[OptionKey(f)]; !ok {
			t.Errorf("flag %s is never recorded", f)
		}
	}
	if DefaultRestarts(false) != 5 || DefaultRestarts(true) != 20 {
		t.Error("the restart budgets are 5, and 20 under --until-done")
	}
}
