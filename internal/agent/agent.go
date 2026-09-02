// Package agent runs an unattended coding agent inside a guest, and keeps a
// channel to it open without pulling its output into the operator's context.
//
// The design is shaped by three failures seen in practice:
//
//   - An agent started as a foreground `incus exec` dies when the operator's
//     shell does, and streams everything it says into the operator's context.
//     So it runs as a systemd unit and writes to files.
//   - An agent that crashes and is restarted from its original prompt redoes
//     work it has already finished. So rig fixes the session UUID up front and
//     resumes it, rather than starting a new conversation each time.
//   - A runaway child — a geometry sidecar, a compiler — can OOM the whole
//     unit and take the agent with it. So the unit caps its own memory and
//     declines to die when the kernel kills one of its children.
//
// Messages to the agent are a file in the guest, not a pipe from the host.
// A file survives a crash, needs nothing holding it open, and cannot couple the
// two processes' lifetimes.
package agent

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

//go:embed run.sh
var runner []byte

// Dir holds everything about one agent run. On disk, not tmpfs: the transcript
// has to outlive a VM stop or --resume has nothing to resume from.
const (
	Dir        = "/var/lib/rig-agent"
	RunnerPath = Dir + "/run"
	PromptPath = Dir + "/prompt"
	InboxPath  = Dir + "/inbox"
	DonePath   = Dir + "/DONE"
	SessionKey = "user.rig.agent.session"
	Unit       = "rig-agent"
)

// Delimiter opens each queued message in the inbox file.
//
// The inbox is plain text, appended to and read whole, so without a marker
// there is nothing to count by but newlines — and a queued message is usually
// several lines, which is how "one message" came to be reported as eleven. It
// reads as a heading in the brief the agent is handed, which is where the file
// ends up.
const Delimiter = "--- operator message ---"

// Pending counts the messages in an inbox file.
//
// An inbox written before delimiters existed has none, and is one message.
func Pending(inbox string) int {
	if strings.TrimSpace(inbox) == "" {
		return 0
	}
	if n := strings.Count(inbox, Delimiter); n > 0 {
		return n
	}
	return 1
}

// NewSessionID returns a UUIDv4. Claude requires a UUID for --session-id, and
// rig generates it rather than letting the guest do it, because rig is what has
// to remember it across a crash.
func NewSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}

// UnitPATH is what the unit runs with.
//
// The nix profile comes first: an agent binary installed with `nix profile
// install` lands there, and a transient unit does not inherit a login shell's
// PATH. Leaving it out fails as "failed to run command 'claude': No such file
// or directory", which reads like a broken image rather than a missing entry.
//
// One constant rather than a literal at the use site, because the preflight
// check has to look down exactly the PATH the unit will use. A check that
// searched a login shell's PATH instead would pass for a binary the unit cannot
// see, which is the failure it exists to catch.
const UnitPATH = "/root/.nix-profile/bin:/run/wrappers/bin:/run/current-system/sw/bin:/usr/bin:/bin"

// InstallHint is the command that puts the agent binary where the unit will
// find it.
//
// The unfree opt-in is not decoration. claude-code is unfree, and a flake
// reference does not read ~/.config/nixpkgs/config.nix — flake evaluation is
// pure — so the base image's own `nixpkgs.config.allowUnfree` does not apply
// either. A plain `nix profile install nixpkgs#claude-code` in a rig guest
// fails with a wall of text whose three suggested fixes are all for the
// non-flake path.
const InstallHint = "NIXPKGS_ALLOW_UNFREE=1 nix profile install --impure nixpkgs#claude-code"

// ClaudeProbe asks whether the agent binary is on the unit's PATH.
//
// `rig agent` runs bare `claude`, so a guest without it fails in the least
// legible way rig has: systemd starts the unit, the runner dies instantly,
// Restart=on-failure tries again, and the operator sees a restart loop with
// "last exit 127" and no explanation. The binary is a project decision — it
// comes from a project flake or `nix profile install`, not from the base image —
// so its absence is an ordinary state to be in, and worth one clear sentence
// rather than a diagnosis.
func ClaudeProbe() string {
	return "PATH=" + UnitPATH + " command -v claude || true"
}

// UnitOpts are the knobs that decide how the unit survives trouble.
type UnitOpts struct {
	Workdir   string
	Session   string
	Timeout   string // passed to timeout(1) inside the guest
	MemoryMax string // systemd syntax; "80%" is a percentage of guest RAM
	Restarts  int    // burst allowed before systemd gives up
	UntilDone bool   // keep resuming after a clean exit until the agent says it is finished
}

// SystemdRun builds the systemd-run invocation.
//
// Each property is load-bearing:
//
//	OOMPolicy=continue — the kernel may kill a runaway child, but the agent
//	  itself must survive to see the failure and diagnose it. systemd's default
//	  (stop) tears down the whole unit, which turns a sidecar leak into a dead
//	  run.
//	MemoryMax — bounds the blast radius to this unit instead of letting the
//	  guest reach a global OOM, where the kernel picks a victim by heuristic and
//	  may well pick the agent.
//	Restart=on-failure — with a fixed session UUID a restart resumes the
//	  conversation, so restarting is recovery rather than starting over. This
//	  property would be wrong without the UUID.
//	StartLimit* — a persistently failing run must escalate to a human instead
//	  of flapping forever.
func SystemdRun(o UnitOpts) []string {
	args := []string{
		"systemd-run",
		"--unit=" + Unit,
		"--collect",
		"--working-directory=" + o.Workdir,
		"--property=OOMPolicy=continue",
		"--property=Restart=on-failure",
		"--property=RestartSec=30s",
		"--property=StartLimitIntervalSec=3600",
		"--property=StartLimitBurst=" + strconv.Itoa(o.Restarts),
		"--setenv=HOME=/root",
		"--setenv=TERM=dumb",
		"--setenv=PATH=" + UnitPATH,
		"--setenv=RIG_AGENT_WORKDIR=" + o.Workdir,
		"--setenv=RIG_AGENT_SESSION=" + o.Session,
		"--setenv=RIG_AGENT_TIMEOUT=" + o.Timeout,
		"--description=rig unattended agent",
	}
	if o.MemoryMax != "" {
		args = append(args, "--property=MemoryMax="+o.MemoryMax)
	}
	if o.UntilDone {
		args = append(args, "--setenv=RIG_AGENT_UNTIL_DONE=1")
	}
	// A login shell so /etc/profile puts nix on PATH; a transient unit is
	// otherwise handed a PATH with no nix in it and dies instantly.
	return append(args, "--", "/run/current-system/sw/bin/bash", "-lc", RunnerPath)
}

// Runner is the wrapper script rig installs into the guest.
func Runner() []byte { return runner }

// Status is the bounded answer to "how is it going" — everything here is a
// handful of bytes, so asking is cheap enough to do often.
type Status struct {
	Unit     string
	Active   string
	Session  string
	Restarts int
	LastExit string
	Inbox    int    // messages queued, not yet delivered
	StatusMD string // whatever the agent wrote for us
	Events   int    // lines in events.jsonl
}

// LastError returns the most recent error the agent reported, from a tail of
// the event stream.
//
// It exists because an agent that cannot authenticate looks identical to one
// that crashed: the unit restart-loops, `last exit` is 1, and the reason is
// buried in a megabyte of JSON. Surfacing it turns "why did it stop" into one
// command.
func LastError(streamJSON string) string {
	var last string
	for _, line := range strings.Split(streamJSON, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type    string   `json:"type"`
			IsError bool     `json:"is_error"`
			Errors  []string `json:"errors"`
			Result  string   `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type != "result" {
			continue
		}
		// Whether a run failed is a field, not a word in its prose. This used
		// to fall back to searching the result text for "failed", which read a
		// successful mission's own summary — "a diagnosis that a fix failed to
		// confirm" — and presented it to the operator under "last error". The
		// run had is_error false and subtype success.
		//
		// errors[] is still checked first and independently: the OAuth failure
		// this function exists for arrives with subtype "success" and a
		// populated errors[], so trusting is_error alone would miss it.
		switch {
		case len(ev.Errors) > 0:
			last = ev.Errors[len(ev.Errors)-1]
		case ev.IsError:
			last = ev.Result
		default:
			// A clean result is the end of the story: an error from a run that
			// was later retried successfully is history, not the reason this
			// one stopped.
			last = ""
		}
	}
	return last
}

// ExplainError adds the fix to errors whose cause is not obvious from the text.
func ExplainError(msg string) string {
	if strings.Contains(msg, "OAuth session expired") {
		return msg + "\n" +
			"      The credential snapshot is stale. Refreshing an OAuth session rotates\n" +
			"      its refresh token, so another consumer of the same credential — a\n" +
			"      Claude Code session on the host — invalidates this copy when it\n" +
			"      refreshes. Re-snapshot it, then `rig restart` and start again.\n" +
			"      A token from `claude setup-token` avoids this entirely."
	}
	return msg
}

// NoEvents is what the guest prints when there is no event log at all. It
// distinguishes "the agent has not started" from "the agent has not spoken",
// which look identical from an empty read and want different reactions.
const NoEvents = "__rig_no_events__"

// TailSpeechCmd reads the agent's own words out of the event log, filtering in
// the guest *before* the tail rather than after it.
//
// Tailing raw bytes and filtering here looks equivalent and is not. A single
// tool result — a build log, a file read — is routinely hundreds of kilobytes,
// so a fixed byte window often holds nothing but tool traffic, and `rig agent
// log` then reported "nothing said yet" about an agent that had been talking
// for hours. Filtering first means the window holds only assistant events, so
// it always spans real speech. The grep is a prefilter, not a parser:
// AssistantText still decides what counts.
func TailSpeechCmd(limitBytes int) string {
	return tailCmd("assistant", limitBytes)
}

// TailResultsCmd reads the run's result events, where a failure reason lives.
// Same reasoning as TailSpeechCmd: a crash whose last megabyte is tool output
// would otherwise report no reason at all.
func TailResultsCmd(limitBytes int) string {
	return tailCmd("result", limitBytes)
}

func tailCmd(evType string, limitBytes int) string {
	log := Dir + "/events.jsonl"
	return fmt.Sprintf(
		`test -s %[1]s || { echo %[2]s; exit 0; }; grep -a '"type":"%[3]s"' %[1]s | tail -c %[4]d`,
		log, NoEvents, evType, limitBytes)
}

// Probe is the scalar half of `agent status`: what systemd knows about the
// unit, plus the clock, in one round trip.
type Probe struct {
	Active   string
	Restarts string
	Started  int64 // unix seconds the current invocation went active
	Now      int64 // guest clock, so ages are computed in the guest's frame
	Exited   int64 // mtime of last_exit
	Done     bool  // the agent created the DONE marker
}

// StatusProbe asks the guest for everything status needs that is not a file's
// contents.
//
// It reports the *unit's* restart count rather than a counter rig keeps
// itself. A file in /var/lib outlives reboots, missions and new sessions, and
// one did: a three-day-old count of 2 was reported as the current process's
// second restart. NRestarts belongs to this unit invocation and resets when a
// human starts it, which is the question being asked.
func StatusProbe() string {
	return fmt.Sprintf(`u=%[1]s
echo "active=$(systemctl is-active $u 2>/dev/null)"
echo "restarts=$(systemctl show $u -p NRestarts --value 2>/dev/null)"
echo "started=$(date -d "$(systemctl show $u -p ActiveEnterTimestamp --value 2>/dev/null)" +%%s 2>/dev/null)"
echo "now=$(date +%%s)"
echo "exited=$(stat -c %%Y %[2]s/last_exit 2>/dev/null)"
echo "done=$(test -e %[2]s/DONE && echo yes)"`, Unit, Dir)
}

// ParseProbe reads StatusProbe's output. Anything missing stays zero: a probe
// that could not answer must not invent a number status will present as fact.
func ParseProbe(out string) Probe {
	var p Probe
	num := func(s string) int64 {
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "active":
			p.Active = strings.TrimSpace(v)
		case "restarts":
			p.Restarts = strings.TrimSpace(v)
		case "started":
			p.Started = num(v)
		case "now":
			p.Now = num(v)
		case "exited":
			p.Exited = num(v)
		case "done":
			p.Done = strings.TrimSpace(v) == "yes"
		}
	}
	return p
}

// Age renders a duration the way an operator reads one: coarse and short. The
// point is never the precision, it is whether a number is from this run or from
// last week.
func Age(sec int64) string {
	switch {
	case sec < 0:
		return ""
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh %dm", sec/3600, (sec%3600)/60)
	default:
		return fmt.Sprintf("%dd %dh", sec/86400, (sec%86400)/3600)
	}
}

// AssistantText pulls the agent's own words out of a stream-json tail, dropping
// tool calls and everything else. This is what makes reading progress cheap:
// the raw event log is megabytes, and almost none of it is worth an operator's
// attention.
func AssistantText(streamJSON string, limit int) []string {
	var out []string
	for _, line := range strings.Split(streamJSON, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // a truncated first line is expected when tailing bytes
		}
		if ev.Type != "assistant" {
			continue
		}
		for _, c := range ev.Message.Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				out = append(out, strings.TrimSpace(c.Text))
			}
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}
