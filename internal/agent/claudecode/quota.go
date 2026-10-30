package claudecode

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tbrockman/rig/internal/agent"
)

// usageEndpoint is where a Claude subscription reports its rate-limit
// windows for an OAuth login, with the OAuth beta header; what `claude`
// itself reads to show the usage bars. An API key has no such thing, which
// is why a quota is the credential's, not the harness's, to have. The
// request is described here and made by the host (cmd/rig): this package
// is in the guest build too, which carries no HTTP client.
const usageEndpoint = "https://api.anthropic.com/api/oauth/usage"

// QuotaRequests names the requests for the credential among vars, best
// first: the access token inside a CLAUDE_CREDENTIALS_B64 snapshot, which
// is a full login; CLAUDE_CODE_OAUTH_TOKEN, which the endpoint refuses when
// it came from `claude setup-token` (that token lacks the user:profile
// scope; DEV_NOTES) but is tried since not every token is one; and then
// this host's own Claude login, assumed to be the same account, which is
// what one mission's overseer read by hand. An API key alone, with no
// login on the host, is no quota and no error. A token goes into its
// request's headers and nowhere else.
func (Harness) QuotaRequests(vars map[string]string) ([]agent.QuotaRequest, error) {
	var reqs []agent.QuotaRequest
	add := func(source, token string) {
		if token != "" {
			reqs = append(reqs, quotaRequest(source, token))
		}
	}
	if snap := vars["CLAUDE_CREDENTIALS_B64"]; snap != "" {
		raw, err := base64.StdEncoding.DecodeString(snap)
		if err != nil {
			return nil, fmt.Errorf("CLAUDE_CREDENTIALS_B64: %w", err)
		}
		token, err := accessToken(raw)
		if err != nil {
			return nil, fmt.Errorf("CLAUDE_CREDENTIALS_B64: %w", err)
		}
		add("the credential file's login snapshot (CLAUDE_CREDENTIALS_B64)", token)
	}
	add("the credential file's CLAUDE_CODE_OAUTH_TOKEN", vars["CLAUDE_CODE_OAUTH_TOKEN"])
	if raw, err := os.ReadFile(hostLoginFile()); err == nil {
		if token, err := accessToken(raw); err == nil {
			add("this host's own Claude login ("+hostLoginFile()+"), assumed to be the same account", token)
		}
	}
	return reqs, nil
}

// hostLoginFile is where `claude` keeps the login on this host.
func hostLoginFile() string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, ".credentials.json")
}

// accessToken is the OAuth access token inside a credentials file.
func accessToken(raw []byte) (string, error) {
	var snap struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return "", err
	}
	return snap.OAuth.AccessToken, nil
}

func quotaRequest(source, token string) agent.QuotaRequest {
	return agent.QuotaRequest{
		Source: source,
		URL:    usageEndpoint,
		Headers: map[string]string{
			"Authorization":  "Bearer " + token,
			"anthropic-beta": "oauth-2025-04-20",
			"Accept":         "application/json",
			"User-Agent":     "claude-code/2.1.314 (rig)",
		},
		Parse: parseQuota,
	}
}

// parseQuota reads the endpoint's answer: an object per window, each with
// a utilization in percent and a reset time, under whatever names the
// provider uses (five_hour, seven_day, and per-model variants). Anything
// else at the top level is ignored.
func parseQuota(body []byte, now time.Time) (*agent.Quota, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("the usage endpoint's answer is not JSON: %w", err)
	}
	q := &agent.Quota{Fetched: now}
	for name, raw := range top {
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		if json.Unmarshal(raw, &w) != nil || w.Utilization == nil {
			continue
		}
		win := agent.Window{Name: name, Utilization: *w.Utilization / 100}
		if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
			win.ResetsAt = t
		}
		q.Windows = append(q.Windows, win)
	}
	if len(q.Windows) == 0 {
		return nil, fmt.Errorf("the usage endpoint's answer names no window: %s", agent.Clean(string(body), 200))
	}
	return q, nil
}
