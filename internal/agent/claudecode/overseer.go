package claudecode

import (
	"encoding/json"
	"path/filepath"

	"github.com/tbrockman/rig/internal/agent"
)

// Overseer launches Claude Code on the host as an overseer. The settings
// file carries the permissions: allow for every rig verb and a few plain
// commands, ask for the confirm list, deny for what an overseer never does.
// Claude Code reads deny, then ask, then allow, and an ask rule prompts
// even when a narrower allow matches, so the confirm list wins over the
// allow list by construction. Interactive, the mission is the first user
// message; unattended, it is -p with prompts auto-denied (dontAsk), which
// is what makes the confirm list a refusal with a message rather than a
// stall.
func (Harness) Overseer(o agent.OverseerOpts) ([]string, map[string][]byte, bool) {
	settings, err := json.MarshalIndent(map[string]any{
		"permissions": map[string]any{
			"allow": o.Allow,
			"ask":   o.Ask,
			"deny":  o.Deny,
		},
	}, "", "  ")
	if err != nil {
		return nil, nil, false
	}
	path := filepath.Join(o.FilesDir, "overseer-settings.json")
	argv := []string{"claude", "--settings", path, "--append-system-prompt", o.SystemPrompt}
	if o.Model != "" {
		argv = append(argv, "--model", o.Model)
	}
	if o.Unattended {
		argv = append(argv, "-p", "--permission-mode", "dontAsk")
	}
	// After --: a mission that begins with a dash is not an option (the
	// runner learned this with its resume notes).
	argv = append(argv, "--", o.Mission)
	return argv, map[string][]byte{"overseer-settings.json": settings}, true
}
