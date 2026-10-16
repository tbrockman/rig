package agent

import "strconv"

// StartOptions are `rig agent start`'s settings, recorded on the instance so
// that a bare `rig agent start <vm>` resumes the run the way it was started.
// Only the session used to be recorded, and a restart that forgot
// --until-done made the agent stop after its next turn, with nothing to say
// why. A flag given explicitly wins, and is recorded in turn.
type StartOptions struct {
	PromptFile string // the brief, on the host; absolute, since a later start may run from elsewhere
	Workdir    string
	Model      string // "" is the guest's default
	Timeout    string
	MemoryMax  string
	UntilDone  bool
	Restarts   int // 0 is the default for the mode: see DefaultRestarts
}

// The instance keys StartOptions is recorded under, by flag name.
const optionPrefix = "user.rig.agent.opt."

// OptionFlags are the start flags that are recorded, in StartOptions' order.
var OptionFlags = []string{"prompt-file", "workdir", "model", "timeout", "memory-max", "until-done", "max-restarts"}

// OptionKey is the instance key a start flag is recorded under.
func OptionKey(flag string) string { return optionPrefix + flag }

// DefaultRestarts is the restart budget when none was chosen. Turn boundaries
// spend it, and a brief with several missions in it has many: five, the right
// number for crashes, would stop a healthy --until-done agent inside an hour.
func DefaultRestarts(untilDone bool) int {
	if untilDone {
		return 20
	}
	return 5
}

// EffectiveRestarts is the restart budget the unit gets.
func (o StartOptions) EffectiveRestarts() int {
	if o.Restarts > 0 {
		return o.Restarts
	}
	return DefaultRestarts(o.UntilDone)
}

// RecordedOptions reads what the instance recorded. An option never recorded
// is its zero value, which the start flags read as "take my default": every
// flag's default is what an absent record means.
func RecordedOptions(cfg map[string]string) StartOptions {
	get := func(flag string) string { return cfg[OptionKey(flag)] }
	o := StartOptions{
		PromptFile: get("prompt-file"),
		Workdir:    get("workdir"),
		Model:      get("model"),
		Timeout:    get("timeout"),
		MemoryMax:  get("memory-max"),
		UntilDone:  get("until-done") == "true",
	}
	o.Restarts, _ = strconv.Atoi(get("max-restarts"))
	return o
}

// Config is o as instance keys. An empty value removes the key, which is how
// a model cleared with --model "" (the guest's default) and a restart budget
// left to the mode are recorded: as nothing.
func (o StartOptions) Config() map[string]string {
	restarts := ""
	if o.Restarts > 0 {
		restarts = strconv.Itoa(o.Restarts)
	}
	return map[string]string{
		OptionKey("prompt-file"):  o.PromptFile,
		OptionKey("workdir"):      o.Workdir,
		OptionKey("model"):        o.Model,
		OptionKey("timeout"):      o.Timeout,
		OptionKey("memory-max"):   o.MemoryMax,
		OptionKey("until-done"):   strconv.FormatBool(o.UntilDone),
		OptionKey("max-restarts"): restarts,
	}
}
