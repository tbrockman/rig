package claudecode

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The endpoint's answer, as usage.py printed it: a window per key, with a
// utilization in percent and a reset time; other keys are ignored.
const usageAnswer = `{"five_hour":{"utilization":19,"resets_at":"2026-10-30T13:00:00Z"},
"seven_day":{"utilization":55.5,"resets_at":"2026-11-04T08:00:00Z"},"seven_day_opus":null,
"extra_usage":{"is_enabled":false}}`

func TestQuotaRequestsAreForLoginsAndNotForAKey(t *testing.T) {
	// No login on this host for the test: the home is empty.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	reqs, err := h.QuotaRequests(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-fake"})
	if err != nil || len(reqs) != 1 {
		t.Fatalf("%+v %v", reqs, err)
	}
	req := reqs[0]
	if req.URL != usageEndpoint || req.Headers["Authorization"] != "Bearer sk-ant-oat-fake" || req.Headers["anthropic-beta"] == "" || !strings.Contains(req.Source, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("the request must carry the token and the OAuth beta header, and say its source: %+v", req)
	}
	q, err := req.Parse([]byte(usageAnswer), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	five, _ := q.Window("five_hour")
	seven, _ := q.Window("seven_day")
	if five.Utilization != 0.19 || seven.Utilization != 0.555 || five.ResetsAt.Format(time.RFC3339) != "2026-10-30T13:00:00Z" || len(q.Windows) != 2 {
		t.Errorf("got %+v", q)
	}

	// A login snapshot is the best source, before a token; this host's own
	// login comes last, named as an assumption.
	snap := base64.StdEncoding.EncodeToString([]byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-snap","refreshToken":"x"}}`))
	os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-host"}}`), 0o600)
	reqs, err = h.QuotaRequests(map[string]string{"CLAUDE_CREDENTIALS_B64": snap, "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-fake"})
	if err != nil || len(reqs) != 3 || reqs[0].Headers["Authorization"] != "Bearer sk-ant-oat-snap" ||
		reqs[1].Headers["Authorization"] != "Bearer sk-ant-oat-fake" || reqs[2].Headers["Authorization"] != "Bearer sk-ant-oat-host" ||
		!strings.Contains(reqs[2].Source, "assumed") {
		t.Errorf("sources in order: %v", err)
		for _, r := range reqs {
			t.Logf("  %s", r.Source)
		}
	}
	// An API key, and no login on the host: no window, not an error.
	os.Remove(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json"))
	if reqs, err := h.QuotaRequests(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api-fake"}); len(reqs) != 0 || err != nil {
		t.Errorf("api key: %v %v", reqs, err)
	}
	if _, err := parseQuota([]byte(`{"nothing":1}`), time.Now()); err == nil || !strings.Contains(err.Error(), "no window") {
		t.Error("an answer with no window is an error, not an empty quota")
	}
}
