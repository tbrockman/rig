// Package claudecode is the Claude Code harness for rig's unattended agent:
// the binary, how it is installed, how it authenticates, the runner that
// launches it, and how its stream-json event log is read. It registers itself
// with internal/agent under the name "claude-code", which is the default.
package claudecode

import (
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/tbrockman/rig/internal/agent"
)

//go:embed run.sh
var runner []byte

// Harness is Claude Code. A value, not a pointer: it has no state.
type Harness struct{}

var _ agent.Harness = Harness{}

func init() { agent.Register(Harness{}) }

// Name is what rig.yaml's agent field takes and what the instance records.
func (Harness) Name() string { return "claude-code" }

// Binary is what the runner execs.
func (Harness) Binary() string { return "claude" }

// InstallHint is the command that puts the binary where the unit will find it.
//
// The unfree opt-in is not decoration. claude-code is unfree, and a flake
// reference does not read ~/.config/nixpkgs/config.nix — flake evaluation is
// pure — so the base image's own `nixpkgs.config.allowUnfree` does not apply
// either. A plain `nix profile install nixpkgs#claude-code` in a rig guest
// fails with a wall of text whose three suggested fixes are all for the
// non-flake path.
func (Harness) InstallHint() string {
	return "NIXPKGS_ALLOW_UNFREE=1 nix profile install --impure nixpkgs#claude-code"
}

// Probe asks whether the binary is on the unit's PATH.
//
// The runner execs bare `claude`, so a guest without it fails in the least
// legible way rig has: systemd starts the unit, the runner dies instantly,
// Restart=on-failure tries again, and the operator sees a restart loop with
// "last exit 127" and no explanation. The binary is a project decision — it
// comes from a project flake or `nix profile install`, not from the base image —
// so its absence is an ordinary state to be in, and worth one clear sentence
// rather than a diagnosis.
func (h Harness) Probe() string {
	return "PATH=" + agent.UnitPATH + " command -v " + h.Binary() + " || true"
}

// Runner is the script the unit runs; run.sh in this package.
func (Harness) Runner() []byte { return runner }

// Credentials are the three ways Claude Code authenticates, ordered by how
// well they survive an unattended run. The runner accepts whichever the
// operator supplied, and materialises the snapshot only when it is the only
// one on offer; run.sh says why.
func (Harness) Credentials() []agent.Credential {
	return []agent.Credential{
		{
			Var:       "CLAUDE_CODE_OAUTH_TOKEN",
			Endpoints: []string{"api.anthropic.com"},
			Note:      "preferred: long-lived, from `claude setup-token`; nothing else rotates it",
		},
		{
			Var:       "ANTHROPIC_API_KEY",
			Endpoints: []string{"api.anthropic.com"},
			Note:      "an API key; the same property",
		},
		{
			Var: "CLAUDE_CREDENTIALS_B64",
			// A base64 JSON snapshot of ~/.claude/.credentials.json: the token
			// inside cannot be matched as a substring of a request, so the
			// proxy cannot stand in for it.
			Endpoints: nil,
			Note:      "a snapshot of ~/.claude/.credentials.json; fragile, a refresh elsewhere rotates it out from under the guest",
		},
	}
}

// TrustEnv makes Claude Code trust an extra CA. It is a Node program, and Node
// reads NODE_EXTRA_CA_CERTS — the CA alone, added to its own roots — and
// ignores SSL_CERT_FILE, so the bundle is of no use to it.
func (Harness) TrustEnv(caFile, _ string) []string {
	return []string{"NODE_EXTRA_CA_CERTS=" + caFile}
}

// LastError returns the most recent error the agent reported, from a tail of
// the stream-json event log.
//
// It exists because an agent that cannot authenticate looks identical to one
// that crashed: the unit restart-loops, `last exit` is 1, and the reason is
// buried in a megabyte of JSON. Surfacing it turns "why did it stop" into one
// command.
func (Harness) LastError(resultsTail string) string {
	var last string
	for _, line := range strings.Split(resultsTail, "\n") {
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
func (Harness) ExplainError(msg string) string {
	if strings.Contains(msg, "OAuth session expired") {
		return msg + "\n" +
			"      The credential snapshot is stale. Refreshing an OAuth session rotates\n" +
			"      its refresh token, so another consumer of the same credential — a\n" +
			"      Claude Code session on the host — invalidates this copy when it\n" +
			"      refreshes. Re-snapshot it and inject it without a VM restart:\n" +
			"        rig creds <vm> <env-file>\n" +
			"      A unit still retrying picks it up on its next try; one that gave up\n" +
			"      resumes with `rig agent start`. A token from `claude setup-token`\n" +
			"      avoids this entirely."
	}
	return msg
}

// AssistantText pulls the agent's own words out of a stream-json tail, dropping
// tool calls and everything else. This is what makes reading progress cheap:
// the raw event log is megabytes, and almost none of it is worth an operator's
// attention.
func (Harness) AssistantText(speechTail string, limit int) []string {
	var out []string
	for _, line := range strings.Split(speechTail, "\n") {
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

// TailSpeechCmd tails the "assistant" events. The grep is a prefilter, not a
// parser: AssistantText still decides what counts.
func (Harness) TailSpeechCmd(limitBytes int) string {
	return agent.TailCmd(`"type":"assistant"`, limitBytes)
}

// TailResultsCmd tails the "result" events, where a failure reason lives.
func (Harness) TailResultsCmd(limitBytes int) string {
	return agent.TailCmd(`"type":"result"`, limitBytes)
}

// Delivery is how an item reaches Claude Code: the PostToolUse hook the
// runner wires with --settings runs after every tool call, the lead's and
// its subagents' alike, and the guest build delivers only when the hook's
// input proves the lead (guest.LeadProof); then what every harness has.
func (Harness) Delivery() []agent.DeliveryPath {
	return append([]agent.DeliveryPath{{
		Via:     agent.ViaHook,
		When:    "after the lead's next tool call, through its PostToolUse hook (a subagent's tool call does not take it)",
		MidTurn: true,
	}}, agent.CommonDelivery()...)
}
