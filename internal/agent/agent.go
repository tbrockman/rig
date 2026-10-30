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
// Messages to the agent are files in the guest, not a pipe from the host. A
// file survives a crash, needs nothing holding it open, and cannot couple the
// two processes' lifetimes. Each is signed on the host and verified in the
// guest before the agent sees it (envelope.go), and reaches a running agent at
// its next tool call through a hook rather than waiting for its next launch
// (channel.go has the layout).
package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Dir holds everything about one agent run. On disk, not tmpfs: the transcript
// has to outlive a VM stop or --resume has nothing to resume from.
const (
	Dir        = "/var/lib/rig-agent"
	RunnerPath = Dir + "/" + RunnerName
	PromptPath = Dir + "/prompt"
	DonePath   = Dir + "/" + DoneName
	EventsPath = Dir + "/events.jsonl"
	StderrPath = Dir + "/stderr.log"
	SessionKey = "user.rig.agent.session"
	Unit       = "rig-agent"
)

// NewSessionID returns a UUIDv4: the shape an agent CLI that takes a session
// id asks for. rig generates it rather than letting the guest do it, because
// rig is what has to remember it across a crash.
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

// UnitPATH is where the agent looks for its binary.
//
// The nix profile comes first: an agent binary installed with `nix profile
// install` lands there. /usr/bin is the one writable directory that survives a
// reboot, for a helper put there by hand or a stand-in binary in a test.
//
// It reaches the agent by a detour. The unit runs the runner under a login
// shell, so that /etc/profile sets up the nix environment — the store paths
// `nix develop` needs, the certificate bundle the agent needs to speak TLS. But
// NixOS's /etc/profile exports PATH absolutely, so whatever the unit set is
// discarded at that point (measured: a unit started with PATH=/marker/bin
// ran its login shell with no /marker/bin in sight). So the unit passes this
// value as RIG_AGENT_PATH as well, and the runner puts it back in front once
// the login shell is done. Until that existed, `rig agent start`'s preflight
// searched this list while the unit searched the login shell's, and a binary
// in /usr/bin passed the check and then restart-looped on exit 127 — the exact
// failure the check was written to catch.
//
// GuestBinDir comes first of all: it holds the guest build of rig, which the
// agent calls as plain `rig`, and which `rig agent start` rewrites every time,
// so nothing else on the guest can stand in for it by being found earlier.
//
// One constant rather than a literal at the use site, because the preflight
// has to look down exactly the PATH the runner will use.
const UnitPATH = GuestBinDir + ":/root/.nix-profile/bin:/run/wrappers/bin:/run/current-system/sw/bin:/usr/bin:/bin"

// ExitUnverified is the runner's exit when it will not launch because nothing
// from the operator can be verified: the guest build of rig is missing, or the
// brief does not verify. The unit does not restart it.
const ExitUnverified = 77

// ExitBudget is the runner's exit when a budget fired: the BUDGET marker
// says which. The unit does not restart it; the session is kept, and `rig
// agent start` after the budget is raised resumes it.
const ExitBudget = 79

// UnitOpts are the knobs that decide how the unit survives trouble.
type UnitOpts struct {
	Workdir     string
	Session     string
	Harness     string // the harness's name, for the guest build to pick its parser
	Timeout     string // passed to timeout(1) inside the guest
	IdleTimeout string // a run with nothing new in its event log for this long is killed and resumed; "" for never
	MemoryMax   string // systemd syntax; "80%" is a percentage of guest RAM
	Restarts    int    // burst allowed before systemd gives up
	UntilDone   bool   // keep resuming after a clean exit until the agent says it is finished
	Model       string // agent model; empty means whatever the guest's default is
	Started     int64  // unix seconds rig agent start ran, from which the hours budget counts

	// The budgets the guest enforces itself; 0 is none of that kind.
	BudgetHours    float64
	BudgetTokens   int64
	BudgetRestarts int
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
//	RestartPreventExitStatus=77 79 — the runner's "nothing from the operator
//	  verifies" (no verifier, or a brief whose signature fails; retrying
//	  cannot fix either, and a run that would act on an unverified brief must
//	  not happen five times) and "a budget fired" (the mission's limit, which
//	  a restart would only cross again).
func SystemdRun(o UnitOpts) []string {
	args := []string{
		"systemd-run",
		"--unit=" + Unit,
		"--collect",
		"--working-directory=" + o.Workdir,
		"--property=OOMPolicy=continue",
		"--property=Restart=on-failure",
		"--property=RestartSec=30s",
		"--property=RestartPreventExitStatus=" + strconv.Itoa(ExitUnverified) + " " + strconv.Itoa(ExitBudget),
		"--property=StartLimitIntervalSec=3600",
		"--property=StartLimitBurst=" + strconv.Itoa(o.Restarts),
		"--setenv=HOME=/root",
		"--setenv=TERM=dumb",
		"--setenv=PATH=" + UnitPATH,
		"--setenv=RIG_AGENT_PATH=" + UnitPATH,
		"--setenv=RIG_AGENT_WORKDIR=" + o.Workdir,
		"--setenv=RIG_AGENT_SESSION=" + o.Session,
		"--setenv=RIG_AGENT_TIMEOUT=" + o.Timeout,
		"--setenv=RIG_AGENT_STARTED=" + strconv.FormatInt(o.Started, 10),
		"--description=rig unattended agent",
	}
	if o.Harness != "" {
		args = append(args, "--setenv=RIG_AGENT_HARNESS="+o.Harness)
	}
	if o.IdleTimeout != "" {
		args = append(args, "--setenv=RIG_AGENT_IDLE_TIMEOUT="+o.IdleTimeout)
	}
	if o.BudgetHours > 0 {
		args = append(args, "--setenv=RIG_AGENT_BUDGET_HOURS="+strconv.FormatFloat(o.BudgetHours, 'f', -1, 64))
	}
	if o.BudgetTokens > 0 {
		args = append(args, "--setenv=RIG_AGENT_BUDGET_TOKENS="+strconv.FormatInt(o.BudgetTokens, 10))
	}
	if o.BudgetRestarts > 0 {
		args = append(args, "--setenv=RIG_AGENT_BUDGET_RESTARTS="+strconv.Itoa(o.BudgetRestarts))
	}
	if o.MemoryMax != "" {
		args = append(args, "--property=MemoryMax="+o.MemoryMax)
	}
	if o.UntilDone {
		args = append(args, "--setenv=RIG_AGENT_UNTIL_DONE=1")
	}
	// An alias ("fable", "opus") resolves to the latest of that family at the
	// API, so pinning a version here would go stale on its own. Passed through
	// untouched either way: rig has no business knowing which models exist.
	if o.Model != "" {
		args = append(args, "--setenv=RIG_AGENT_MODEL="+o.Model)
	}
	// A login shell so /etc/profile puts nix on PATH; a transient unit is
	// otherwise handed a PATH with no nix in it and dies instantly.
	return append(args, "--", "/run/current-system/sw/bin/bash", "-lc", RunnerPath)
}

// NoEvents is what the guest prints when there is no event log at all. It
// distinguishes "the agent has not started" from "the agent has not spoken",
// which look identical from an empty read and want different reactions.
const NoEvents = "__rig_no_events__"

// TailCmd reads the lines of the event log that contain match, filtering in
// the guest *before* the tail rather than after it. It is the mechanics behind
// a harness's TailSpeechCmd and TailResultsCmd; the harness supplies the match,
// since the log's format is its own, and its parser still decides what counts.
//
// Tailing raw bytes and filtering afterwards looks equivalent and is not. A
// single tool result — a build log, a file read — is routinely hundreds of
// kilobytes, so a fixed byte window often holds nothing but tool traffic, and
// `rig agent log` then reported "nothing said yet" about an agent that had been
// talking for hours. Filtering first means the window holds only the lines
// asked for, so it always spans real speech; on the other read, a crash whose
// last megabyte is tool output would otherwise report no reason at all.
//
// match is a grep basic regular expression, single-quoted into the command, so
// it must not contain a single quote.
func TailCmd(match string, limitBytes int) string {
	return fmt.Sprintf(
		`test -s %[1]s || { echo %[2]s; exit 0; }; grep -a '%[3]s' %[1]s | tail -c %[4]d`,
		EventsPath, NoEvents, match, limitBytes)
}

// Probe is the bounded half of `agent status` and all of `agent watch`: what
// systemd knows about the unit, the clock, and the channel's state, in one
// round trip. Everything here is a handful of bytes per item, with a cap on
// the items, so asking is cheap enough to do every few seconds.
type Probe struct {
	Active   string
	Restarts string
	Started  int64  // unix seconds the current invocation went active
	Now      int64  // guest clock, so ages are computed in the guest's frame
	Exited   int64  // mtime of last_exit
	Done     bool   // the agent created the DONE marker
	Summary  string // DONE's first line, which `rig done` fills
	Budget   string // BUDGET's first line: the budget that stopped the agent, "" when none
	Watchdog string // watchdog's first line: a run's limit that fired and has not been resumed past yet
	StatusMD string // STATUS.md's size and mtime: it changed when this did

	Pending int               // envelopes in inbox.d, not yet taken
	Legacy  bool              // an older rig's unsigned inbox has content
	SHA256  map[string]string // each Installed name -> its hash in the guest, "" when missing
	Asks    []Ask             // newest first, at most maxProbed
	Answers map[string]Envelope

	Deliveries       int        // lines in deliveries.jsonl
	RecentDeliveries []Delivery // its last few, oldest first
	Tampers          int        // lines in tamper.jsonl
	RecentTampers    []Tamper   // its last few, oldest first

	// The workdir's git state, when ProbeOpts named one and it is a checkout.
	Branch    string
	Head      int64 // unix seconds of the branch's tip commit
	Tip       string
	Dirty     int      // files git status lists
	Worktrees []string // git worktree list, one line each

	Units      []TransientUnit // transient services running, other than rig's
	DiskUsedKB int64           // / in the guest
	DiskFreeKB int64
	MemUsedKB  int64
	MemAvailKB int64
	Listening  map[int]bool // TCP ports with a listener in the guest; nil when the guest did not say
}

// TransientUnit is a systemd-run service running in the guest, with when
// it went active.
type TransientUnit struct {
	Name  string
	Since int64 // unix seconds, 0 when unknown
}

// ProbeOpts widens the probe to what the agent's run adds: the workdir's
// git state, and the transient units, narrowed by a glob when one is set.
type ProbeOpts struct {
	Workdir string
	Units   string // a shell glob over unit names; "" is every transient service but rig's
}

// How many of each list item the probe reads, and how many bytes of each.
const (
	maxProbed  = 100
	recentLogs = 20
)

// StatusProbe asks the guest for everything status and watch need.
//
// It reports the *unit's* restart count rather than a counter rig keeps
// itself. A file in /var/lib outlives reboots, missions and new sessions, and
// one did: a three-day-old count of 2 was reported as the current process's
// second restart. NRestarts belongs to this unit invocation and resets when a
// human starts it, which is the question being asked.
//
// Every line is key=value. What the guest wrote (asks, answers, log lines) is
// cut to a bounded length and one line each; it is data from the guest, which
// the host displays only after cleaning it (Clean).
func StatusProbe() string { return statusProbe(Dir, ProbeOpts{}) }

// StatusProbeWith is StatusProbe plus what ProbeOpts asks for.
func StatusProbeWith(o ProbeOpts) string { return statusProbe(Dir, o) }

func statusProbe(dir string, o ProbeOpts) string {
	sums := ""
	for _, f := range Installed {
		sums += fmt.Sprintf("echo \"sha256:%[1]s=$(test -f $d/%[1]s && sha256sum < $d/%[1]s | cut -c1-64)\"\n", f.Name)
	}
	glob := o.Units
	if glob == "" {
		glob = "*"
	}
	return fmt.Sprintf(`u=%[1]s d=%[2]s w=%[16]s
echo "active=$(systemctl is-active $u 2>/dev/null)"
echo "restarts=$(systemctl show $u -p NRestarts --value 2>/dev/null)"
echo "started=$(date -d "$(systemctl show $u -p ActiveEnterTimestamp --value 2>/dev/null)" +%%s 2>/dev/null)"
echo "now=$(date +%%s)"
echo "exited=$(stat -c %%Y $d/last_exit 2>/dev/null)"
echo "done=$(test -e $d/%[3]s && echo yes)"
echo "summary=$(head -n 1 $d/%[3]s 2>/dev/null | head -c 300 | tr -d '\n')"
echo "budget=$(head -n 1 $d/%[14]s 2>/dev/null | head -c 300 | tr -d '\n')"
echo "watchdog=$(head -n 1 $d/%[15]s 2>/dev/null | head -c 300 | tr -d '\n')"
echo "statusmd=$(stat -c %%s.%%Y $d/STATUS.md 2>/dev/null)"
echo "pending=$(ls -1 $d/%[4]s 2>/dev/null | grep -c '\%[5]s$')"
echo "legacy=$(test -s $d/%[6]s && echo yes)"
%[7]sls -t $d/%[8]s 2>/dev/null | grep '\.json$' | head -n %[9]d | while IFS= read -r f; do
  printf 'ask=%%s ' "${f%%.json}"; head -c 2048 "$d/%[8]s/$f" | tr -d '\n'; echo
done
ls $d/%[10]s 2>/dev/null | grep '\%[5]s$' | head -n %[9]d | while IFS= read -r f; do
  printf 'answer=%%s ' "${f%%%[5]s}"; head -n 1 "$d/%[10]s/$f" | head -c 2048 | tr -d '\n'; echo
done
echo "deliveries=$(cat $d/%[11]s 2>/dev/null | wc -l)"
tail -n %[13]d $d/%[11]s 2>/dev/null | cut -c1-1024 | sed 's/^/delivery=/'
echo "tampers=$(cat $d/%[12]s 2>/dev/null | wc -l)"
tail -n %[13]d $d/%[12]s 2>/dev/null | cut -c1-1024 | sed 's/^/tamper=/'
if [ -n "$w" ] && git -C "$w" rev-parse --git-dir >/dev/null 2>&1; then
  echo "branch=$(git -C "$w" rev-parse --abbrev-ref HEAD 2>/dev/null)"
  echo "head=$(git -C "$w" log -1 --format=%%ct 2>/dev/null)"
  echo "tip=$(git -C "$w" log -1 --format='%%h %%s' 2>/dev/null | head -c 200 | tr -d '\n')"
  echo "dirty=$(git -C "$w" status --short 2>/dev/null | wc -l)"
  git -C "$w" worktree list 2>/dev/null | cut -c1-300 | sed 's/^/worktree=/'
fi
for f in /run/systemd/transient/*.service; do
  [ -e "$f" ] || continue
  n=$(basename "$f")
  case "$n" in $u.service|rig-*.service) continue;; esac
  case "$n" in %[17]s) ;; *) continue;; esac
  [ "$(systemctl is-active "$n" 2>/dev/null)" = active ] || continue
  t=$(systemctl show "$n" -p ActiveEnterTimestamp --value 2>/dev/null)
  echo "unit=$n $([ -n "$t" ] && date -d "$t" +%%s 2>/dev/null)"
done
echo "disk=$(df -Pk / 2>/dev/null | awk 'NR==2{print $3, $4}')"
echo "mem=$(free -k 2>/dev/null | awk 'NR==2{print $3, $7}')"
echo "listen=$(ss -ltnH 2>/dev/null | awk '{print $4}' | sed 's/.*://' | sort -un | tr '\n' ' ')"`,
		Unit, dir, DoneName, InboxName, EnvelopeExt, LegacyInboxName, sums,
		AsksName, maxProbed, AnswersName, DeliveriesName, TamperName, recentLogs,
		BudgetName, WatchdogName, shellWord(o.Workdir), glob)
}

// shellWord quotes s for the probe's shell; "" is ”.
func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ParseProbe reads StatusProbe's output. Anything missing stays zero: a probe
// that could not answer must not invent a number status will present as fact.
func ParseProbe(out string) Probe {
	p := Probe{SHA256: map[string]string{}, Answers: map[string]Envelope{}}
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
		case "summary":
			p.Summary = strings.TrimSpace(v)
		case "budget":
			p.Budget = strings.TrimSpace(v)
		case "watchdog":
			p.Watchdog = strings.TrimSpace(v)
		case "statusmd":
			p.StatusMD = strings.TrimSpace(v)
		case "pending":
			p.Pending = int(num(v))
		case "legacy":
			p.Legacy = strings.TrimSpace(v) == "yes"
		case "deliveries":
			p.Deliveries = int(num(v))
		case "tampers":
			p.Tampers = int(num(v))
		case "ask":
			// The file name is the id; the record may have been cut short.
			id, rec, _ := strings.Cut(v, " ")
			a := Ask{ID: id}
			_ = json.Unmarshal([]byte(rec), &a)
			a.ID = id
			p.Asks = append(p.Asks, a)
		case "answer":
			id, rec, _ := strings.Cut(v, " ")
			var e Envelope
			_ = json.Unmarshal([]byte(rec), &e)
			p.Answers[id] = e
		case "delivery":
			var d Delivery
			if json.Unmarshal([]byte(v), &d) == nil {
				p.RecentDeliveries = append(p.RecentDeliveries, d)
			}
		case "tamper":
			var t Tamper
			if json.Unmarshal([]byte(v), &t) == nil {
				p.RecentTampers = append(p.RecentTampers, t)
			}
		case "branch":
			p.Branch = strings.TrimSpace(v)
		case "head":
			p.Head = num(v)
		case "tip":
			p.Tip = strings.TrimSpace(v)
		case "dirty":
			p.Dirty = int(num(v))
		case "worktree":
			p.Worktrees = append(p.Worktrees, strings.TrimSpace(v))
		case "unit":
			name, since, _ := strings.Cut(strings.TrimSpace(v), " ")
			if name != "" {
				p.Units = append(p.Units, TransientUnit{Name: name, Since: num(since)})
			}
		case "disk":
			used, free, _ := strings.Cut(strings.TrimSpace(v), " ")
			p.DiskUsedKB, p.DiskFreeKB = num(used), num(free)
		case "mem":
			used, avail, _ := strings.Cut(strings.TrimSpace(v), " ")
			p.MemUsedKB, p.MemAvailKB = num(used), num(avail)
		case "listen":
			p.Listening = map[int]bool{}
			for _, port := range strings.Fields(v) {
				if n, err := strconv.Atoi(port); err == nil {
					p.Listening[n] = true
				}
			}
		default:
			if name, ok := strings.CutPrefix(k, "sha256:"); ok {
				p.SHA256[name] = strings.TrimSpace(v)
			}
		}
	}
	return p
}

// OpenAsks are the asks with no answer yet, newest first.
func (p Probe) OpenAsks() []Ask {
	var open []Ask
	for _, a := range p.Asks {
		if _, ok := p.Answers[a.ID]; !ok {
			open = append(open, a)
		}
	}
	return open
}

// Mismatched names the Installed files whose hash in the guest is not the one
// recorded on the instance (config). A file with nothing recorded is not
// judged: it was installed by a rig that did not record it. A probe that said
// nothing about hashes at all judges nothing either.
func (p Probe) Mismatched(config map[string]string) []string {
	if len(p.SHA256) == 0 {
		return nil
	}
	var bad []string
	for _, f := range Installed {
		want := config[f.Key]
		if want != "" && p.SHA256[f.Name] != want {
			bad = append(bad, f.Name)
		}
	}
	return bad
}

// AskAge is how long ago a was asked, in the guest's clock, or "".
func (p Probe) AskAge(a Ask) string {
	t, err := time.Parse(time.RFC3339, a.Time)
	if err != nil || p.Now == 0 {
		return ""
	}
	return Age(p.Now - t.Unix())
}

// Clean makes text from the guest safe to print on one line of a terminal:
// control characters (an escape sequence, a newline that would forge a second
// line) become spaces, and it is cut to max runes.
func Clean(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == max {
			b.WriteString("…")
			break
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
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
