package main

import (
	"strings"
	"testing"
)

func TestTailLinesKeepsTheLastN(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{"a\nb\n", 5, "a\nb\n"},
		{"a\nb\n", 0, "a\nb\n"},
		{"", 3, ""},
	} {
		if got := tailLines(c.in, c.n); got != c.want {
			t.Errorf("tailLines(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

// A console log drops its head as it grows; following it must print only what
// is new after a wrap, and all of a buffer that shares nothing (a restart).
func TestConsoleDeltaSurvivesTheBufferWrapping(t *testing.T) {
	prev := "boot 1\nboot 2\nlogin:\n"
	if got := consoleDelta(prev, prev+"more\n"); got != "more\n" {
		t.Errorf("growth: %q", got)
	}
	wrapped := "boot 2\nlogin:\nmore\n"
	if got := consoleDelta(prev, wrapped); got != "more\n" {
		t.Errorf("after a wrap: %q", got)
	}
	if got := consoleDelta(prev, "fresh boot\n"); got != "fresh boot\n" {
		t.Errorf("a new buffer: %q", got)
	}
	if got := consoleDelta(prev, prev); got != "" {
		t.Errorf("no change: %q", got)
	}
}

func TestNewSinceShowsOnlyWhatTheWindowAdds(t *testing.T) {
	f := strings.Fields
	for _, c := range []struct{ seen, window, want string }{
		{"a b c", "b c d e", "d e"},
		{"a b c", "a b c", ""},
		{"a b c", "x y", "x y"},     // a gap: everything is new
		{"x y x", "y x y x", "y x"}, // repeats: the longest overlap wins
		{"", "a", "a"},
	} {
		got := strings.Join(newSince(f(c.seen), f(c.window)), " ")
		if got != c.want {
			t.Errorf("newSince(%q, %q) = %q, want %q", c.seen, c.window, got, c.want)
		}
	}
}
