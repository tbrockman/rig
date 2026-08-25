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
	SessionKey = "user.rig.agent.session"
	Unit       = "rig-agent"
)

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

// UnitOpts are the knobs that decide how the unit survives trouble.
type UnitOpts struct {
	Workdir   string
	Session   string
	Timeout   string // passed to timeout(1) inside the guest
	MemoryMax string // systemd syntax; "80%" is a percentage of guest RAM
	Restarts  int    // burst allowed before systemd gives up
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
		"--setenv=PATH=/run/wrappers/bin:/run/current-system/sw/bin:/usr/bin:/bin",
		"--setenv=RIG_AGENT_WORKDIR=" + o.Workdir,
		"--setenv=RIG_AGENT_SESSION=" + o.Session,
		"--setenv=RIG_AGENT_TIMEOUT=" + o.Timeout,
		"--description=rig unattended agent",
	}
	if o.MemoryMax != "" {
		args = append(args, "--property=MemoryMax="+o.MemoryMax)
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
