package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Harness is what differs between agents. Everything else in this package is
// shared: the unit, the layout under Dir, the session id, the status probe.
//
// # The runner contract
//
// Runner() is the script the unit runs, as RunnerPath, under a login shell
// (`bash -lc`). What follows is what the shared side promises it and what it
// must do in return; a second harness implements exactly this.
//
// The unit's environment:
//
//	HOME=/root  TERM=dumb  PATH=UnitPATH (GuestBinDir first: `rig` is the guest build)
//	RIG_AGENT_PATH       UnitPATH again. NixOS's /etc/profile exports PATH
//	                     absolutely, so the login shell has thrown the unit's
//	                     PATH away by the time the runner starts; the runner
//	                     prepends this to PATH first, keeping the login shell's
//	                     entries behind it (they carry the nix store paths and
//	                     the TLS bundle).
//	RIG_AGENT_WORKDIR    where to run; the runner cds there, or exits 78.
//	RIG_AGENT_SESSION    the session id rig chose and recorded (SessionKey); a
//	                     UUIDv4. The runner starts or resumes this session and
//	                     never another, and the guest build of rig accepts only
//	                     what the operator signed for it.
//	RIG_AGENT_TIMEOUT    for timeout(1) around the agent: one run is killed
//	                     after this long, and systemd resumes it.
//	RIG_AGENT_UNTIL_DONE "1" when a clean exit is a turn boundary, not a
//	                     result; unset otherwise.
//	RIG_AGENT_MODEL      a model name to pass through, when the operator gave
//	                     one; unset otherwise, so the guest's default applies.
//
// Files under Dir, all written by the shared side unless noted:
//
//	bin/rig           the guest build of rig (GuestBinPath). Everything the
//	                  runner knows of the operator comes through it: `rig
//	                  brief`, `rig hook launch`, `rig hook settings`.
//	operator.pub      what it verifies with; the private key stays on the host.
//	brief.msg         the brief, signed (BriefPath). The runner reads it with
//	                  `rig brief`, sends it as the prompt only when it creates
//	                  the session, and exits ExitUnverified (77) without
//	                  launching when it does not verify.
//	inbox.d/          signed envelopes from `rig agent send`, `answer` and
//	                  `brief` (channel.go has the rest of the channel's files).
//	                  The runner appends what `rig hook launch` prints to this
//	                  launch's prompt, and passes `rig hook settings` as
//	                  claude's --settings so the PostToolUse hook delivers what
//	                  arrives mid-turn.
//	launch.token      written by the runner at every launch: a random token
//	                  every delivery in this launch carries and the system
//	                  prompt names, so the agent can tell rig's deliveries from
//	                  text that only claims to be one.
//	events.jsonl      the agent's event stream, appended by the runner
//	                  (EventsPath). Its format is the harness's: TailSpeechCmd,
//	                  TailResultsCmd, AssistantText and LastError read it.
//	stderr.log        the agent's stderr and the runner's own notes, appended.
//	last_exit         the agent's exit code, written by the runner after every
//	                  run (77 for a launch it refused), and removed when a new
//	                  session starts.
//	turn_boundary     left by the runner at exit 75, read and removed by the
//	                  next launch; it is how a resume knows the previous exit
//	                  was not a crash.
//	DONE              created by the agent (DonePath), with `rig done`, to say
//	                  the brief is finished; removed by `rig agent start`.
//	STATUS.md         the agent's own, if its brief asks for one.
//
// Credentials are the environment in /run/rig/env, which the runner sources
// (exported) and which dies with the VM; it exits 78 when the file is missing
// or holds none of Credentials(). Everything else it needs across a VM stop —
// the transcript above all — it keeps under Dir, on disk.
//
// Resume or start is decided from evidence the runner keeps on disk (the
// transcript for RIG_AGENT_SESSION), never from a marker set before the agent
// ran: a launch that fails leaves nothing behind, so the next one starts
// cleanly. A resumed session is not sent the brief again: it already has it,
// and a copy per turn boundary once made a third of a transcript. Its prompt
// is a short note that points at `rig brief` and says which of three things
// happened — the turn ended (turn_boundary was present), the process crashed
// (systemd's NRestarts for Unit is non-zero), or it was started deliberately —
// because an agent told its process died when it merely stopped talking
// spends its next turn reconciling a tree nothing interrupted. A new session
// removes last_exit.
//
// Exit codes: the agent's own, written to last_exit, except that under
// RIG_AGENT_UNTIL_DONE a clean exit with no DONE marker leaves turn_boundary
// and exits 75, so Restart=on-failure resumes the conversation. 0 means the
// agent said it was finished; 127 is the binary missing from UnitPATH, which
// `rig agent status` names; 77 (ExitUnverified) is a launch refused because
// nothing from the operator verifies, said in stderr.log, and not restarted.
type Harness interface {
	Name() string                                        // "claude-code": the value rig.yaml's agent field takes, and what the instance records
	Binary() string                                      // what the runner execs: "claude"
	InstallHint() string                                 // the in-guest command that installs the binary onto UnitPATH
	Probe() string                                       // a guest command that prints the binary's path on UnitPATH, or nothing
	Runner() []byte                                      // the script the unit runs; it honours the runner contract documented in this package
	Credentials() []Credential                           // the env vars it authenticates with, most preferred first
	TrustEnv(caFile, bundleFile string) []string         // KEY=VALUE lines that make it trust an extra CA (bundleFile = system roots plus that CA, caFile = the CA alone)
	LastError(resultsTail string) string                 // why a run stopped, from TailResultsCmd's output; "" when nothing
	ExplainError(msg string) string                      // adds the fix to errors whose cause is not obvious from the text
	AssistantText(speechTail string, limit int) []string // what the agent said, newest last, from TailSpeechCmd's output
	TailSpeechCmd(limitBytes int) string                 // guest command: a bounded tail of the agent's own speech
	TailResultsCmd(limitBytes int) string                // guest command: a bounded tail of the run's result events
}

// Credential is one way a harness authenticates.
type Credential struct {
	Var       string   // the environment variable
	Endpoints []string // where its value is sent, as "[METHOD[,METHOD] ]host[/path]" grants, e.g. "api.anthropic.com"; nil when the value cannot be proxied by substring (a JSON blob, a file)
	Note      string   // one line for an operator: what it is and what it survives
}

// HarnessKey records the harness on the instance; empty means Default().
const HarnessKey = "user.rig.agent.harness"

// defaultName is the harness Default() returns. A name rather than a value so
// this package stays free of the implementation, which registers itself.
const defaultName = "claude-code"

var (
	registryMu sync.RWMutex
	registry   = map[string]Harness{}
)

// Register makes an implementation known, under its Name(). Registering a
// name again replaces it, which is how a test substitutes a stub.
func Register(h Harness) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[h.Name()] = h
}

// Default is Claude Code. It panics when nothing is registered under that name:
// the implementation registers itself from its init, so a missing one is a
// missing import, not a state a running rig can be in.
func Default() Harness {
	h, err := Lookup(defaultName)
	if err != nil {
		panic(fmt.Sprintf("agent: no default harness: %v (import internal/agent/%s)", err, strings.ReplaceAll(defaultName, "-", "")))
	}
	return h
}

// Lookup finds a harness by Name(); "" is Default(). The error names the
// known harnesses, since the name came from a manifest or an instance.
func Lookup(name string) (Harness, error) {
	if name == "" {
		return Default(), nil
	}
	registryMu.RLock()
	h, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		known := strings.Join(Names(), ", ")
		if known == "" {
			known = "none registered"
		}
		return nil, fmt.Errorf("unknown agent harness %q (known: %s)", name, known)
	}
	return h, nil
}

// Names lists the registered harnesses, sorted.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ForInstance is the harness an instance recorded, from its config.
func ForInstance(cfg map[string]string) (Harness, error) {
	return Lookup(cfg[HarnessKey])
}
