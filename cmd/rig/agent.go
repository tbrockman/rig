package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/agent"
	"github.com/tbrockman/rig/internal/creds"
	"github.com/tbrockman/rig/internal/incus"
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
	var promptFile, workdir, memMax, timeout, model string
	var restarts int
	var newSession, untilDone bool
	cmd := &cobra.Command{
		Use:   "start <vm>",
		Short: "Start the unattended agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
					"  rig start %s --env <env-file>   # or rig restart --env", name, name)
			}
			if out, _ := a.exec(name, "systemctl is-active "+agent.Unit); strings.TrimSpace(out) == "active" {
				return fmt.Errorf("%s is already running on %s.\n"+
					"  rig agent status %s   # see what it is doing\n"+
					"  rig agent stop %s     # stop it first", agent.Unit, name, name, name)
			}

			// Look for the binary before creating any state. Without this the
			// unit starts, dies on exec, and restart-loops — and `rig agent
			// status` reports "last exit 127" and an empty last error, which
			// says nothing about the one thing that is wrong.
			if out, err := a.exec(name, agent.ClaudeProbe()); err != nil || strings.TrimSpace(out) == "" {
				return fmt.Errorf("no `claude` on the agent unit's PATH in %s.\n"+
					"The agent binary is a project decision, not part of the base image,\n"+
					"so a fresh VM does not have one until you put it there:\n"+
					"  rig exec %s %s\n"+
					"PATH searched: %s", name, name, agent.InstallHint, agent.UnitPATH)
			}

			prompt, err := os.ReadFile(promptFile)
			if err != nil {
				return err
			}

			// A session that already exists is resumed, not replaced. Losing a
			// transcript to a re-run of `agent start` would be exactly the
			// mistake this command exists to prevent.
			//
			// `--new-session` is the deliberate exception: a *second* mission in
			// a VM that already earned its warm nix store and its checkout wants
			// the machine, not the conversation. Resuming there would hand the
			// agent a finished brief and a transcript of work it must not redo.
			// The old transcript stays on disk under ~/.claude/projects, so this
			// abandons a conversation rather than destroying one.
			session := inst.Config[agent.SessionKey]
			switch {
			case session != "" && newSession:
				previous := session
				if session, err = agent.NewSessionID(); err != nil {
					return err
				}
				if err := a.c.SetConfigKey(name, agent.SessionKey, session); err != nil {
					return err
				}
				note("new session %s (leaving %s on disk, unresumed)", session, previous)
			case session == "":
				if session, err = agent.NewSessionID(); err != nil {
					return err
				}
				if err := a.c.SetConfigKey(name, agent.SessionKey, session); err != nil {
					return err
				}
				note("new session %s", session)
			default:
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

			// A DONE marker from the previous mission would end this one before
			// its first turn: --until-done reads it as "already finished". The
			// turn_boundary note is the same kind of leftover, and would open
			// this run with a resumption note about a turn that is not this
			// run's.
			_, _ = a.exec(name, "rm -f "+agent.DonePath+" "+agent.Dir+"/turn_boundary")

			// Turn boundaries spend the restart budget, and a brief with several
			// missions in it has many. Five — the right number for crashes —
			// would stop a healthy agent inside an hour, so raise it unless the
			// operator picked a number themselves.
			if untilDone && !cmd.Flags().Changed("max-restarts") {
				restarts = 20
			}

			argv := agent.SystemdRun(agent.UnitOpts{
				Workdir: workdir, Session: session, Timeout: timeout,
				MemoryMax: memMax, Restarts: restarts, UntilDone: untilDone, Model: model,
			})
			out, err := a.exec(name, shellQuote(argv))
			if err != nil {
				return fmt.Errorf("starting %s: %w\n%s", agent.Unit, err, out)
			}
			note("started %s in %s (memory cap %s, %d restarts before it gives up)",
				agent.Unit, workdir, memMax, restarts)
			if model != "" {
				note("model: %s", model)
			}
			if untilDone {
				note("--until-done: a clean exit resumes instead of stopping, until the agent creates %s",
					agent.DonePath)
			}
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
	f.BoolVar(&newSession, "new-session", false, "start a fresh conversation instead of resuming; for a second mission in the same VM")
	f.StringVar(&model, "model", "",
		"model for the agent — an alias like 'fable' or 'opus' takes the latest of that family; empty uses the guest's default")
	f.BoolVar(&untilDone, "until-done", false,
		"treat a clean exit as a turn boundary, not a result: resume until the agent creates "+agent.DonePath)
	_ = cmd.MarkFlagRequired("prompt-file")
	return cmd
}

func (a *app) agentSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send <vm> <message>...",
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
			// Each message opens with a delimiter, so a message of several
			// lines still counts as one.
			updated += agent.Delimiter + "\n" + msg + "\n"
			if err := a.c.Mkdir(name, agent.Dir, 0o700); err != nil {
				return err
			}
			if err := a.c.WriteFile(name, agent.InboxPath, []byte(updated), 0o600); err != nil {
				return err
			}
			note("queued for %s (%d message(s) pending)", name, agent.Pending(updated))
			return nil
		},
	}
	return cmd
}

func (a *app) agentStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <vm>",
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
			raw, _ := a.exec(name, agent.StatusProbe())
			p := agent.ParseProbe(raw)

			row := func(k, v string) {
				if v == "" {
					v = "—"
				}
				fmt.Printf("  %-14s %s\n", k, v)
			}
			row("unit", p.Active)
			row("session", inst.Config[agent.SessionKey])
			row("restarts", p.Restarts)

			// Whether the agent thinks it is finished, from the marker it sets
			// itself. Under --until-done this is the difference between "it
			// stopped because it is done" and "it stopped because it ran out of
			// restarts", which otherwise look identical from `unit inactive`.
			if p.Done {
				row("mission", "COMPLETE (the agent created "+agent.DonePath+")")
			}

			// last_exit is written when a run ends, and simply stays there.
			// Printed bare next to `unit active` it reads as this run's
			// verdict: a three-day-old 1, left by a credential failure before
			// a reboot, sent a supervisor looking for a crash that was not
			// happening. Say which run it belongs to, and how old it is.
			if code := get(agent.Dir + "/last_exit"); code != "" {
				age := ""
				if p.Exited > 0 && p.Now > 0 {
					age = agent.Age(p.Now - p.Exited)
				}
				switch {
				case p.Started > 0 && p.Exited > 0 && p.Exited < p.Started:
					row("last exit", fmt.Sprintf("%s (an earlier run, %s ago)", code, age))
				case age != "":
					row("last exit", fmt.Sprintf("%s (%s ago)", code, age))
				default:
					row("last exit", code)
				}
			} else {
				row("last exit", "")
			}

			row("queued msgs", strconv.Itoa(agent.Pending(get(agent.InboxPath))))

			// Why it stopped, if it stopped badly. Reading it here means the
			// operator does not have to know events.jsonl exists.
			if p.Active != "active" {
				if raw, err := a.exec(name, agent.TailResultsCmd(20000)); err == nil {
					if e := agent.LastError(raw); e != "" {
						fmt.Printf("\n  last error   %s\n", agent.ExplainError(e))
					}
				}
			}

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
		Use:   "log <vm>",
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
			// Filter and tail in the guest, so a huge log never crosses the
			// wire and the window is spent on speech rather than on the tool
			// output surrounding it.
			out, err := a.exec(name, agent.TailSpeechCmd(400000))
			if err != nil {
				return err
			}
			if strings.Contains(out, agent.NoEvents) {
				note("no event log yet — the agent has not started writing one.\n"+
					"  rig agent status %s   # is the unit even up", name)
				return nil
			}
			texts := agent.AssistantText(out, lines)
			if len(texts) == 0 {
				// Now a real statement about the agent rather than about the
				// read: the window held only assistant events, and none of
				// them was speech.
				note("the agent has written events but said nothing yet; it is working in tools.\n"+
					"  rig agent status %s   # unit, restarts, and any last error", name)
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
		Use:   "stop <vm>",
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
