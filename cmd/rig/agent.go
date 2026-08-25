package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"rig/internal/agent"
	"rig/internal/creds"
	"rig/internal/incus"
)

// exec runs a short control command in the guest. Bounded: everything the agent
// verbs ask for is a status read or a unit action, and a hang here would be an
// operator command that never returns.
func (a *app) exec(name, command string) (string, error) {
	return a.c.Exec(name, command, incus.ExecOpts{Timeout: 60 * time.Second})
}

// The agent verbs. Grouped under one noun because they share state in the
// guest: start creates it, send appends to it, status and log read it.
func (a *app) agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "agent",
		GroupID: "guest",
		Short:   "Run an unattended agent in the guest, and talk to it",
		Long: "Runs a coding agent as a systemd unit inside the guest so it outlives\n" +
			"your shell. Its session UUID is fixed and stored, so a crash resumes the\n" +
			"conversation instead of restarting the brief. Messages you send are a\n" +
			"file in the guest, delivered at the agent's next turn or next restart —\n" +
			"never a pipe, which would die with either process.",
	}
	cmd.AddCommand(a.agentStartCmd(), a.agentSendCmd(), a.agentStatusCmd(),
		a.agentLogCmd(), a.agentStopCmd())
	return cmd
}

func (a *app) agentStartCmd() *cobra.Command {
	var promptFile, workdir, memMax, timeout string
	var restarts int
	cmd := &cobra.Command{
		Use:   "start <name>",
		Short: "Start the unattended agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			inst, _, err := a.c.Instance(name)
			if err != nil {
				return err
			}
			if inst.Config[creds.InstanceKey] == "" {
				return fmt.Errorf("%s has no credential file.\n"+
					"  rig start %s --env <file>   # or rig restart --env", name, name)
			}
			if out, _ := a.exec(name, "systemctl is-active "+agent.Unit); strings.TrimSpace(out) == "active" {
				return fmt.Errorf("%s is already running on %s.\n"+
					"  rig agent status %s   # see what it is doing\n"+
					"  rig agent stop %s     # stop it first", agent.Unit, name, name, name)
			}

			prompt, err := os.ReadFile(promptFile)
			if err != nil {
				return err
			}

			// A session that already exists is resumed, not replaced. Losing a
			// transcript to a re-run of `agent start` would be exactly the
			// mistake this command exists to prevent.
			session := inst.Config[agent.SessionKey]
			if session == "" {
				if session, err = agent.NewSessionID(); err != nil {
					return err
				}
				if err := a.c.SetConfigKey(name, agent.SessionKey, session); err != nil {
					return err
				}
				note("new session %s", session)
			} else {
				note("resuming session %s", session)
			}

			if err := a.c.Mkdir(name, agent.Dir, 0o700); err != nil {
				return err
			}
			if err := a.c.WriteFile(name, agent.RunnerPath, agent.Runner(), 0o755); err != nil {
				return err
			}
			if err := a.c.WriteFile(name, agent.PromptPath, prompt, 0o600); err != nil {
				return err
			}

			// Clear a stale failure state, or systemd refuses the unit name.
			_, _ = a.exec(name, "systemctl reset-failed "+agent.Unit+" 2>/dev/null || true")

			argv := agent.SystemdRun(agent.UnitOpts{
				Workdir: workdir, Session: session, Timeout: timeout,
				MemoryMax: memMax, Restarts: restarts,
			})
			out, err := a.exec(name, shellQuote(argv))
			if err != nil {
				return fmt.Errorf("starting %s: %w\n%s", agent.Unit, err, out)
			}
			note("started %s in %s (memory cap %s, %d restarts before it gives up)",
				agent.Unit, workdir, memMax, restarts)
			note("watch it:  rig agent status %s", name)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&promptFile, "prompt-file", "", "host file holding the agent's brief (required)")
	f.StringVar(&workdir, "workdir", "/work", "working directory in the guest")
	f.StringVar(&memMax, "memory-max", "80%", "systemd MemoryMax for the unit; a runaway child is killed before the guest OOMs")
	f.StringVar(&timeout, "timeout", "6h", "kill a single agent run after this long; it restarts and resumes")
	f.IntVar(&restarts, "max-restarts", 5, "restarts allowed per hour before systemd gives up and waits for a human")
	_ = cmd.MarkFlagRequired("prompt-file")
	return cmd
}

func (a *app) agentSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send <name> <message>...",
		Short: "Queue a message for the agent",
		Long: "Appends to a file in the guest. The agent picks it up at its next turn\n" +
			"if it is polling, and unconditionally on its next restart. Nothing is\n" +
			"held open, so neither process can hang waiting for the other.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			msg := strings.Join(args[1:], " ")
			existing, _ := a.c.Pull(name, agent.InboxPath)
			updated := string(existing)
			if updated != "" && !strings.HasSuffix(updated, "\n") {
				updated += "\n"
			}
			updated += msg + "\n"
			if err := a.c.Mkdir(name, agent.Dir, 0o700); err != nil {
				return err
			}
			if err := a.c.WriteFile(name, agent.InboxPath, []byte(updated), 0o600); err != nil {
				return err
			}
			note("queued for %s (%d message(s) pending)", name, strings.Count(updated, "\n"))
			return nil
		},
	}
	return cmd
}

func (a *app) agentStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <name>",
		Short: "Is it alive, how many times has it crashed, what does it say",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			inst, _, err := a.c.Instance(name)
			if err != nil {
				return err
			}
			get := func(p string) string {
				b, err := a.c.Pull(name, p)
				if err != nil {
					return ""
				}
				return strings.TrimSpace(string(b))
			}
			active, _ := a.exec(name, "systemctl is-active "+agent.Unit)
			nrestart, _ := a.exec(name, "systemctl show "+agent.Unit+" -p NRestarts --value")

			row := func(k, v string) {
				if v == "" {
					v = "—"
				}
				fmt.Printf("  %-14s %s\n", k, v)
			}
			row("unit", strings.TrimSpace(active))
			row("session", inst.Config[agent.SessionKey])
			row("restarts", strings.TrimSpace(nrestart))
			row("last exit", get(agent.Dir+"/last_exit"))

			inbox := get(agent.InboxPath)
			pending := 0
			if inbox != "" {
				pending = len(strings.Split(inbox, "\n"))
			}
			row("queued msgs", strconv.Itoa(pending))

			if s := get(agent.Dir + "/STATUS.md"); s != "" {
				fmt.Printf("\n--- STATUS.md (the agent's own words) ---\n%s\n", s)
			} else {
				fmt.Printf("\nNo %s/STATUS.md yet. If you want one, tell the agent to keep it\n"+
					"current in its brief — rig cannot make it write one.\n", agent.Dir)
			}
			return nil
		},
	}
}

func (a *app) agentLogCmd() *cobra.Command {
	var lines int
	cmd := &cobra.Command{
		Use:   "log <name>",
		Short: "The agent's recent words, without the tool-call noise",
		Long: "Reads a bounded tail of the event stream and prints only what the agent\n" +
			"said. The raw log is megabytes of tool calls; this is the part worth an\n" +
			"operator's attention, and it stays small enough to read often.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			// Tail bytes in the guest so a huge log never crosses the wire.
			out, err := a.exec(name,
				"tail -c 400000 "+agent.Dir+"/events.jsonl 2>/dev/null || true")
			if err != nil {
				return err
			}
			texts := agent.AssistantText(out, lines)
			if len(texts) == 0 {
				note("nothing said yet (or no events.jsonl); try: rig agent status %s", name)
				return nil
			}
			for _, t := range texts {
				fmt.Printf("• %s\n\n", t)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&lines, "lines", 10, "how many of the most recent messages to show")
	return cmd
}

func (a *app) agentStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop the agent (its session is kept, so it can be resumed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			if _, err := a.exec(name, "systemctl stop "+agent.Unit); err != nil {
				return err
			}
			note("stopped %s; `rig agent start %s` resumes the same session", agent.Unit, name)
			return nil
		},
	}
}

// shellQuote renders an argv as a single shell command, quoting each word so a
// prompt or path containing spaces cannot split into two arguments.
func shellQuote(argv []string) string {
	q := make([]string, len(argv))
	for i, s := range argv {
		q[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}
