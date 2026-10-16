package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// diff(1) is the reference: the agent reads these as unified diffs, so they
// must be exactly what the tool everyone knows prints.
func TestUnifiedDiffMatchesDiffU(t *testing.T) {
	if _, err := exec.LookPath("diff"); err != nil {
		t.Skip("no diff(1) to compare against")
	}
	brief := "# Mission\n\nBuild the kernel.\n\n## Rules\n- commit often\n- keep STATUS.md current\n- never push\n\n## Gates\nE1: tests green\nE2: benchmarks\nE3: docs\n"
	for name, c := range map[string]struct{ a, b string }{
		"one rule changed": {brief, strings.Replace(brief, "- never push", "- push to the fork only", 1)},
		"rule added":       {brief, strings.Replace(brief, "- never push\n", "- never push\n- no new dependencies\n", 1)},
		"head and tail":    {brief, "# Mission (v2)\n" + strings.TrimPrefix(brief, "# Mission\n") + "E4: release notes\n"},
		"far apart":        {brief, strings.Replace(strings.Replace(brief, "Build the kernel.", "Build the kernel and the modules.", 1), "E3: docs", "E3: docs and man pages", 1)},
		"from nothing":     {"", "a\nb\n"},
		"to nothing":       {"a\nb\n", ""},
		"everything":       {"a\nb\nc\n", "x\ny\n"},
	} {
		dir := t.TempDir()
		pa, pb := filepath.Join(dir, "a"), filepath.Join(dir, "b")
		os.WriteFile(pa, []byte(c.a), 0o600)
		os.WriteFile(pb, []byte(c.b), 0o600)
		want, _ := exec.Command("diff", "-u", "--label", "brief (as delivered)", "--label", "brief (now)", pa, pb).Output()
		got := UnifiedDiff("brief (as delivered)", "brief (now)", c.a, c.b)
		if got != string(want) {
			t.Errorf("%s:\n--- got\n%s--- want\n%s", name, got, want)
		}
	}
}

func TestUnifiedDiffOfEqualTextIsEmpty(t *testing.T) {
	if d := UnifiedDiff("a", "b", "same\n", "same\n"); d != "" {
		t.Errorf("got %q", d)
	}
}

// A middle too large to compare is still a correct diff: everything in it
// removed, then everything added.
func TestUnifiedDiffOfAHugeChangeIsStillADiff(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 3000; i++ {
		a.WriteString("old line\n")
		b.WriteString("new line\n")
	}
	d := UnifiedDiff("a", "b", a.String(), b.String())
	if !strings.HasPrefix(d, "--- a\n+++ b\n@@ -1,3000 +1,3000 @@\n") ||
		strings.Count(d, "\n-old line") != 3000 || strings.Count(d, "\n+new line") != 3000 {
		t.Errorf("got %.200q", d)
	}
}
