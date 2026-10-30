package main

import (
	"os"
	"path/filepath"
	"testing"
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
