package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tbrockman/rig/internal/agent"
)

// A message with lines and quotes in it cannot survive the shell as words,
// so it comes from a file or stdin; but never from both, and never empty.
func TestMessageTextFromWordsOrAFile(t *testing.T) {
	dir := t.TempDir()
	msg := filepath.Join(dir, "note.md")
	os.WriteFile(msg, []byte("line one\n'quoted' line two\n\n"), 0o644)
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte("\n  \n"), 0o644)

	if got, err := messageText([]string{"hello", "there"}, ""); err != nil || got != "hello there" {
		t.Errorf("words: %q, %v", got, err)
	}
	if got, err := messageText(nil, msg); err != nil || got != "line one\n'quoted' line two" {
		t.Errorf("file: %q, %v", got, err)
	}
	for _, c := range []struct {
		words []string
		file  string
	}{{[]string{"hi"}, msg}, {nil, ""}, {nil, empty}} {
		if _, err := messageText(c.words, c.file); err == nil {
			t.Errorf("words %v with file %q must be refused", c.words, c.file)
		}
	}
}

// The host makes the quota request the harness names, and reports the
// provider's refusal in its own words.
func TestFetchQuotaCarriesTheHeadersAndReportsRefusals(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		if r.URL.Path == "/no" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"invalid token"}`))
			return
		}
		w.Write([]byte(`{"five_hour":{"utilization":42,"resets_at":"2026-10-30T13:00:00Z"}}`))
	}))
	defer srv.Close()
	parse := func(body []byte, _ time.Time) (*agent.Quota, error) {
		return &agent.Quota{Windows: []agent.Window{{Name: "five_hour", Utilization: 0.42}}}, nil
	}
	q, err := fetchQuota(agent.QuotaRequest{URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer x"}, Parse: parse})
	if err != nil || got.Get("Authorization") != "Bearer x" || q.Windows[0].Utilization != 0.42 {
		t.Errorf("got %+v, %v, headers %v", q, err, got)
	}
	if _, err := fetchQuota(agent.QuotaRequest{URL: srv.URL + "/no", Parse: parse}); err == nil || !strings.Contains(err.Error(), "401") || !errors.Is(err, errRefused) {
		t.Errorf("a refusal must be reported as one, got %v", err)
	}
}
