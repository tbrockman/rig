package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/tbrockman/rig/internal/manifest"
)

// The operator audit log. Every host verb that acts on a VM appends one
// line to a per-VM log on this host: time, who, the verb, its arguments,
// the outcome. It is what lets an operator trust a privileged overseer
// after the fact instead of confirming each step in advance; the channel
// records what was sent, this records what was done.
//
// Under the state directory, keyed by the VM's name, rather than beside the
// manifest: a VM made with `rig new` has no manifest, a project directory
// is often a repository where a log is noise, and the log must outlive a
// manifest that moves. Lines are appended, never rewritten; nothing here
// holds a secret, since a credential is a path in every verb's arguments.

// actorEnv names who runs rig: `rig overseer` sets it to "overseer" for the
// harness it launches, so the log tells the deputy's verbs from the
// operator's own.
const actorEnv = "RIG_ACTOR"

// stateDir is rig's state on this host: $XDG_STATE_HOME/rig, else
// ~/.local/state/rig.
func stateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "rig"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "rig"), nil
}

// auditPath is vm's operator log.
func auditPath(vm string) (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "audit", vm+".log"), nil
}

// auditLine renders one record. Arguments are shown as typed, cut to a
// bounded length: a message's text is the channel's to keep whole.
func auditLine(now time.Time, actor, verb string, args []string, err error) string {
	outcome := "ok"
	if err != nil {
		first, _, _ := strings.Cut(err.Error(), "\n")
		outcome = "error: " + first
	}
	shown := quoteForDisplay(args)
	if len(shown) > 300 {
		shown = shown[:300] + "…"
	}
	line := fmt.Sprintf("%s %s %s", now.UTC().Format(time.RFC3339), actor, verb)
	if shown != "" {
		line += " " + shown
	}
	return line + " -> " + outcome
}

// appendAudit records one verb on vm. A log that cannot be written is said
// on stderr and not fatal: the verb already ran.
func appendAudit(vm, verb string, args []string, err error) {
	path, perr := auditPath(vm)
	if perr == nil {
		perr = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	var f *os.File
	if perr == nil {
		f, perr = os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	}
	if perr != nil {
		fmt.Fprintf(os.Stderr, "rig: audit log: %v\n", perr)
		return
	}
	defer f.Close()
	actor := os.Getenv(actorEnv)
	if actor == "" {
		actor = "operator"
	}
	fmt.Fprintln(f, auditLine(time.Now(), actor, verb, args, err))
}

// auditedVerbs are the verbs that act on a VM, with how each names it: the
// first operand, or the manifest -f names.
var auditedVerbs = map[string]func(*cobra.Command, []string) string{
	"start": firstArg, "stop": firstArg, "restart": firstArg, "rm": firstArg, "creds": firstArg,
	"forward": firstArg, "push": firstArg, "pull": firstArg, "exec": firstArg, "sh": firstArg,
	"mount": firstArg, "verify": firstArg, "claim": firstArg,
	"delete": manifestArg, "apply": manifestArg,
}

func firstArg(_ *cobra.Command, args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func manifestArg(cmd *cobra.Command, _ []string) string {
	file, _ := cmd.Flags().GetString("file")
	if file == "" {
		return ""
	}
	m, err := manifest.Load(file)
	if err != nil {
		return ""
	}
	return m.Guest.Name
}

// auditVerbs wraps the audited verbs' RunE so each appends its record after
// running, whatever its outcome. Every `rig agent` verb is audited, since
// all of them act on the agent's VM.
func auditVerbs(root *cobra.Command) {
	for _, c := range root.Commands() {
		if vmOf, ok := auditedVerbs[c.Name()]; ok {
			audit(c, vmOf)
		}
		if c.Name() == "agent" {
			for _, sub := range c.Commands() {
				audit(sub, firstArg)
			}
		}
	}
}

func audit(c *cobra.Command, vmOf func(*cobra.Command, []string) string) {
	run := c.RunE
	if run == nil {
		return
	}
	c.RunE = func(cmd *cobra.Command, args []string) error {
		err := run(cmd, args)
		if vm := vmOf(cmd, args); vm != "" && nameRE.MatchString(vm) {
			var flags []string
			cmd.Flags().Visit(func(f *pflag.Flag) { flags = append(flags, "--"+f.Name+"="+f.Value.String()) })
			appendAudit(vm, strings.TrimPrefix(cmd.CommandPath(), "rig "), append(flags, args...), err)
		}
		return err
	}
}
