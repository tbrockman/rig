package main

import (
	"slices"
	"strings"
	"testing"
)

// The third operand replaced --dest and --out, which keep working for
// existing scripts. Given both, neither can be assumed to be the one meant.
func TestTheDestinationIsGivenOnce(t *testing.T) {
	a := &app{}
	for _, flag := range []string{"dest", "out"} {
		c := a.pushCmd()
		if flag == "out" {
			c = a.pullCmd()
		}
		if got, err := destination(c, []string{"vm", "src", "there"}, flag); err != nil || got != "there" {
			t.Errorf("positional: got %q, %v", got, err)
		}
		c.Flags().Set(flag, "elsewhere")
		if got, err := destination(c, []string{"vm", "src"}, flag); err != nil || got != "elsewhere" {
			t.Errorf("--%s alone must still work: got %q, %v", flag, got, err)
		}
		if _, err := destination(c, []string{"vm", "src", "there"}, flag); err == nil {
			t.Errorf("a third operand and --%s together must be refused", flag)
		}
	}
}

// push and pull are mirrors, and their usage lines must read as such.
func TestPushAndPullTakeTheSameShape(t *testing.T) {
	a := &app{}
	push, pull := a.pushCmd().Use, a.pullCmd().Use
	if push != "push <vm> <host-path> [<guest-path>]" || pull != "pull <vm> <guest-path> [<host-path>]" {
		t.Errorf("got %q and %q", push, pull)
	}
	if !strings.Contains(a.pullCmd().Long, "current directory") {
		t.Error("help must say what a relative host path resolves against")
	}
}

func TestShIsTheVerbAndShellItsAlias(t *testing.T) {
	c := (&app{}).shellCmd()
	if c.Name() != "sh" || !slices.Contains(c.Aliases, "shell") {
		t.Errorf("got %q, aliases %v", c.Name(), c.Aliases)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 2048: "2.0 KiB", 3 << 30: "3.0 GiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
