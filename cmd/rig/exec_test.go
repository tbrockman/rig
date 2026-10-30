package main

import (
	"io"
	"strings"
	"testing"
)

// Several words arrive exactly as given; one is a script. Joining words with
// spaces, as rig once did, lost quoting and ran a `;` as a second command.
func TestGuestCommandKeepsWordsWhole(t *testing.T) {
	if got := guestCommand([]string{"cd /work && ls"}); got != "cd /work && ls" {
		t.Errorf("one argument is a script: %q", got)
	}
	got := guestCommand([]string{"printf", `[%s]\n`, "a  b", "c;echo INJECTED"})
	want := `'printf' '[%s]\n' 'a  b' 'c;echo INJECTED'`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// The command goes after --. Each mistake is refused before rig touches a VM,
// with the command that works.
func TestExecRequiresTheDash(t *testing.T) {
	for in, want := range map[string]string{
		"myvm cd":             "rig exec myvm -- cd",
		"myvm":                "no command",
		"myvm --":             "no command",
		"myvm extra -- ls":    "name one VM",
		"myvm ls -la":         "goes after --",
		"--dir /tmp myvm pwd": "rig exec myvm -- pwd",
	} {
		c := (&app{}).execCmd()
		c.SetArgs(strings.Fields(in))
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		err := c.Execute()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("rig exec %s: got %v, want an error containing %q", in, err, want)
		}
	}
	c := (&app{}).execCmd()
	c.SetArgs([]string{"myvm", "cd /work && git log"})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "rig exec myvm -- 'cd /work && git log'") {
		t.Errorf("the suggestion must quote a script as one word, got %v", err)
	}
}
