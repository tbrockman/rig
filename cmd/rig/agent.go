package main

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/agent"
	_ "github.com/tbrockman/rig/internal/agent/claudecode" // the default harness registers itself
	"github.com/tbrockman/rig/internal/agent/guestbin"
	"github.com/tbrockman/rig/internal/agent/operator"
	"github.com/tbrockman/rig/internal/creds"
	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
)

// exec runs a short control command in the guest. Bounded: everything the agent
// verbs ask for is a status read or a unit action, and a hang here would be an
// operator command that never returns.
func (a *app) exec(name, command string) (string, error) {
	return a.c.Exec(name, command, incus.ExecOpts{Timeout: 60 * time.Second})
}

// harnessOf is the instance and the harness it recorded. Every agent verb that
// talks to the agent, rather than to the unit, starts here: which binary,
// which event format and which credentials are the harness's to say.
func (a *app) harnessOf(name string) (*incus.Instance, agent.Harness, error) {
	inst, _, err := a.c.Instance(name)
	if err != nil {
		return nil, nil, err
	}
	h, err := agent.ForInstance(inst.Config)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	return inst, h, nil
}

// The agent verbs. Grouped under one noun because they share state in the
// guest: start creates it, send, answer and brief add to it, status, asks,
// watch and log read it.
func (a *app) agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "agent",
		GroupID: "guest",
		Short:   "Run an agent unattended in the guest, and talk to it",
		Long: "Runs the agent as a systemd unit inside the guest so it outlives your\n" +
			"shell. Its session id is fixed and stored, so a crash resumes the\n" +
			"conversation instead of restarting the brief. What you send it is a file in\n" +
			"the guest, never a pipe, which would die with either process: signed here,\n" +
			"verified there by rig's guest build before the agent sees it, and delivered\n" +
			"after the agent's next tool call, or at its next launch. The agent asks you\n" +
			"things the same way; `rig agent watch` tells you when.\n\n" +
			"Harness: claude-code, the default; rig.yaml's agent: field picks another.",
	}
	cmd.AddCommand(a.agentInstallCmd(), a.agentStartCmd(), a.agentBriefCmd(), a.agentSendCmd(),
		a.agentAnswerCmd(), a.agentAsksCmd(), a.agentStatusCmd(), a.agentWatchCmd(),
		a.agentLogCmd(), a.agentStopCmd())
	return cmd
}

// agentInstallCmd puts the agent binary where the unit will find it.
//
// A verb rather than a hint to copy out of an error: the install can need
// flags that are not guessable (see claudecode's InstallHint), and a fresh VM
// needs it every time, so it belongs on the path an operator actually walks.
func (a *app) agentInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install <vm>",
		Short: "Put the agent binary on the unit's PATH",
		Long: "Installs the agent binary into the guest, onto the agent unit's PATH. The\n" +
			"base image does not carry it, so which build a VM runs is a project\n" +
			"decision, and a fresh VM has none. A project that would rather pin it\n" +
			"ships it in its guest flake instead (project-template/guest), and then\n" +
			"never needs this.\n\n" +
			"Runs the harness's install command in the guest; for claude-code:\n  " +
			agent.Default().InstallHint(),
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			_, h, err := a.harnessOf(name)
			if err != nil {
				return err
			}
			if out, err := a.exec(name, h.Probe()); err == nil && strings.TrimSpace(out) != "" {
				note("%s is already on the unit's PATH in %s: %s", h.Binary(), name, strings.TrimSpace(out))
				return nil
			}
			note("installing %s into %s (a download; this takes a while)", h.Binary(), name)
			if _, err := a.c.Exec(name, h.InstallHint(), incus.ExecOpts{
				Streaming: true, Timeout: 30 * time.Minute,
			}); err != nil {
				return fmt.Errorf("install failed in %s: %w", name, err)
			}
			out, err := a.exec(name, h.Probe())
			if err != nil || strings.TrimSpace(out) == "" {
				return fmt.Errorf("the install ran, but no `%s` appeared on the unit's PATH\n  PATH searched: %s", h.Binary(), agent.UnitPATH)
			}
			note("%s at %s", h.Binary(), strings.TrimSpace(out))
			return nil
		},
	}
}

func (a *app) agentStartCmd() *cobra.Command {
	var promptFile, workdir, memMax, timeout, model string
	var restarts int
	var newSession, untilDone bool
	cmd := &cobra.Command{
		Use:   "start <vm>",
		Short: "Start the unattended agent, or resume it as it was started",
		Long: "Starts the agent as the " + agent.Unit + " systemd unit in the guest, working in\n" +
			"--workdir on the brief in --prompt-file. Two things must already be true:\n" +
			"the VM has a credential file recorded (rig new/start --env), and the agent\n" +
			"binary is on the unit's PATH — the image does not carry it, so a fresh VM\n" +
			"has none.\n\n" +
			"The options are recorded on the instance, so a bare `rig agent start <vm>`\n" +
			"resumes the run as it was started: same brief file, workdir, model,\n" +
			"--until-done, timeout and limits. A flag given now wins, and is recorded.\n\n" +
			"A session that already exists is resumed, not replaced; --new-session\n" +
			"starts a fresh conversation for a second mission in the same VM. The brief\n" +
			"opens a new session and is not sent again on a resume, which already has\n" +
			"it; if the file changed since it was last delivered, the agent is sent the\n" +
			"diff instead (rig agent brief does that without a restart).\n\n" +
			"Each start installs the guest build of rig and this VM's public key, and\n" +
			"signs the brief with the private key, which stays in ~/.config/rig/agent/<vm>/.\n" +
			"Everything about the run lives under " + agent.Dir + " in the guest:\n" +
			"  brief.msg       the brief, signed; the runner will not launch on one that fails\n" +
			"  inbox.d/        what rig agent send, answer and brief queue, one signed file each\n" +
			"  asks/           the agent's questions (rig agent asks)\n" +
			"  bin/rig         the guest build of rig, first on the agent's PATH\n" +
			"  events.jsonl    the raw event log (`rig agent log` filters it)\n" +
			"  stderr.log      the runner's own notes and the agent's stderr\n" +
			"  STATUS.md       whatever the agent writes there; `rig agent status` prints it\n" +
			"  DONE            created by the agent (rig done) to say the brief is finished",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			inst, h, err := a.harnessOf(name)
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
					"  rig agent brief %s    # change the brief without a restart\n"+
					"  rig agent stop %s     # stop it first", agent.Unit, name, name, name, name)
			}

			// Look for the binary before creating any state. Without this the
			// unit starts, dies on exec, and restart-loops — and `rig agent
			// status` reports "last exit 127" and an empty last error, which
			// says nothing about the one thing that is wrong.
			if out, err := a.exec(name, h.Probe()); err != nil || strings.TrimSpace(out) == "" {
				return fmt.Errorf("no `%s` on the agent unit's PATH in %s.\n"+
					"The %s harness runs it, and the base image does not carry it, so a\n"+
					"fresh VM has none until you put one there:\n"+
					"  rig agent install %s   # runs: %s\n"+
					"PATH searched: %s", h.Binary(), name, h.Name(), name, h.InstallHint(), agent.UnitPATH)
			}
			guestRig, err := guestbin.For(inst.Architecture)
			if err != nil {
				return err
			}

			opts, err := startOptions(cmd.Flags().Changed, agent.RecordedOptions(inst.Config), agent.StartOptions{
				PromptFile: promptFile, Workdir: workdir, Model: model, Timeout: timeout,
				MemoryMax: memMax, UntilDone: untilDone, Restarts: restarts,
			})
			if err != nil {
				return fmt.Errorf("%w\n  rig agent start %s --prompt-file <brief>", err, name)
			}
			store, err := operator.For(name)
			if err != nil {
				return err
			}
			brief, err := os.ReadFile(opts.PromptFile)
			if err != nil {
				last, ok, _ := store.Brief()
				if cmd.Flags().Changed("prompt-file") || !ok {
					return err
				}
				note("cannot read the recorded brief (%v); keeping the one last delivered", err)
				brief = []byte(last)
			}

			key, made, err := store.KeyOrCreate()
			if err != nil {
				return err
			}
			if made {
				note("new operator key for %s: %s (it never leaves this host)", name, store.KeyPath())
			}
			pub := agent.MarshalPublicKey(key.Public().(ed25519.PublicKey))

			// A session that already exists is resumed, not replaced. Losing a
			// transcript to a re-run of `agent start` would be exactly the
			// mistake this command exists to prevent.
			//
			// `--new-session` is the deliberate exception: a *second* mission in
			// a VM that already earned its warm nix store and its checkout wants
			// the machine, not the conversation. Resuming there would hand the
			// agent a finished brief and a transcript of work it must not redo.
			// The old transcript stays on disk under agent.Dir, so this abandons
			// a conversation rather than destroying one.
			session, previous := inst.Config[agent.SessionKey], ""
			resuming := session != "" && !newSession
			if !resuming {
				previous = session
				if session, err = agent.NewSessionID(); err != nil {
					return err
				}
				if err := a.c.SetConfigKey(name, agent.SessionKey, session); err != nil {
					return err
				}
			}
			switch {
			case resuming:
				note("resuming session %s", session)
			case previous != "":
				note("new session %s (leaving %s on disk, unresumed)", session, previous)
			default:
				note("new session %s", session)
			}

			for _, dir := range []struct {
				path string
				mode os.FileMode
			}{{agent.Dir, 0o700}, {agent.GuestBinDir, 0o755}, {agent.InboxDir, 0o700}} {
				if err := a.c.Mkdir(name, dir.path, dir.mode); err != nil {
					return err
				}
			}
			// What was queued for the previous session, and its asks, are its
			// own: moved aside, so the new one neither refuses them as
			// addressed elsewhere nor lists their asks as open.
			if previous != "" {
				aside := agent.Dir + "/previous/" + previous
				if _, err := a.exec(name, "cd "+agent.Dir+" && mkdir -p "+shellQuote([]string{aside})+
					" && for f in inbox.d claimed asks answers delivered; do if [ -e $f ]; then mv $f "+
					shellQuote([]string{aside})+"/; fi; done && mkdir -m 700 inbox.d"); err != nil {
					return err
				}
				note("the previous session's queue and asks are kept in %s", aside)
			}

			runner := h.Runner()
			if err := a.put(name, agent.RunnerPath, runner, 0o755); err != nil {
				return err
			}
			if err := a.put(name, agent.GuestBinPath, guestRig, 0o755); err != nil {
				return err
			}
			if err := a.put(name, agent.OperatorPubPath, pub, 0o644); err != nil {
				return err
			}
			delivered, hadBrief, err := store.Brief()
			if err != nil {
				return err
			}
			if err := a.putBrief(name, store, key, session, string(brief)); err != nil {
				return err
			}
			// A resumed session is not sent the brief, so a brief that changed
			// since the agent last had it reaches it as a diff, at launch.
			if resuming && hadBrief && delivered != string(brief) {
				ch := &channel{name: name, session: session, store: store, key: key}
				if _, err := a.deliver(ch, agent.KindBriefUpdate, "brief",
					agent.UnifiedDiff("brief (as delivered)", "brief (now)", delivered, string(brief))); err != nil {
					return err
				}
				note("%s changed since it was last delivered; the agent gets the diff at launch", opts.PromptFile)
			} else if resuming && !hadBrief {
				note("no record of the brief this host last delivered to %s, so no diff: if it changed, say so (rig agent send)", name)
			}
			if err := store.SetBrief(string(brief)); err != nil {
				return err
			}

			// What this start installed, and how it was started, so status can
			// tell a replaced verifier from the one rig wrote, and a bare start
			// can resume the run as it was.
			record := opts.Config()
			for _, f := range agent.Installed {
				switch f.Name {
				case agent.RunnerName:
					record[f.Key] = sha256Hex(runner)
				case agent.BinName:
					record[f.Key] = sha256Hex(guestRig)
				case agent.PubName:
					record[f.Key] = sha256Hex(pub)
				}
			}
			if err := a.c.SetConfigKeys(name, record); err != nil {
				return err
			}

			// Clear a stale failure state, or systemd refuses the unit name.
			_, _ = a.exec(name, "systemctl reset-failed "+agent.Unit+" 2>/dev/null || true")

			// A DONE marker from the previous mission would end this one before
			// its first turn: --until-done reads it as "already finished". The
			// turn_boundary note is the same kind of leftover, and would open
			// this run with a resumption note about a turn that is not this
			// run's. The plain prompt file is an older rig's unsigned brief,
			// which nothing reads now.
			_, _ = a.exec(name, "rm -f "+agent.DonePath+" "+agent.Dir+"/turn_boundary "+agent.Dir+"/prompt")

			argv := agent.SystemdRun(agent.UnitOpts{
				Workdir: opts.Workdir, Session: session, Timeout: opts.Timeout, MemoryMax: opts.MemoryMax,
				Restarts: opts.EffectiveRestarts(), UntilDone: opts.UntilDone, Model: opts.Model,
			})
			out, err := a.exec(name, shellQuote(argv))
			if err != nil {
				return fmt.Errorf("starting %s: %w\n%s", agent.Unit, err, out)
			}
			note("started %s in %s (memory cap %s, %d restarts before it gives up)",
				agent.Unit, opts.Workdir, opts.MemoryMax, opts.EffectiveRestarts())
			if opts.Model != "" {
				note("model: %s", opts.Model)
			}
			if opts.UntilDone {
				note("--until-done: a clean exit resumes instead of stopping, until the agent creates %s",
					agent.DonePath)
			}
			note("watch it:  rig agent status %s   (or: rig agent watch %s)", name, name)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&promptFile, "prompt-file", "", "host file holding the agent's brief; required the first time, then recorded")
	f.StringVar(&workdir, "workdir", "/work", "working directory in the guest")
	f.StringVar(&memMax, "memory-max", "80%", "systemd MemoryMax for the unit; a runaway child is killed before the guest OOMs")
	f.StringVar(&timeout, "timeout", "6h", "kill a single agent run after this long; it restarts and resumes")
	f.IntVar(&restarts, "max-restarts", 0,
		fmt.Sprintf("restarts allowed per hour before systemd gives up and waits for a human (default %d, or %d with --until-done)",
			agent.DefaultRestarts(false), agent.DefaultRestarts(true)))
	f.BoolVar(&newSession, "new-session", false, "start a fresh conversation instead of resuming; for a second mission in the same VM")
	f.StringVar(&model, "model", "",
		"model for the agent — an alias like 'fable' or 'opus' takes the latest of that family; empty uses the guest's default")
	f.BoolVar(&untilDone, "until-done", false,
		"treat a clean exit as a turn boundary, not a result: resume until the agent creates "+agent.DonePath)
	return cmd
}

// startOptions resolves each start option: the flag when it was given now,
// else what the instance recorded, else the flag's default. The prompt file is
// made absolute, since a later start may run from another directory.
func startOptions(changed func(flag string) bool, recorded, given agent.StartOptions) (agent.StartOptions, error) {
	o := given
	pick := func(flag string, v *string, rec string) {
		if !changed(flag) && rec != "" {
			*v = rec
		}
	}
	pick("prompt-file", &o.PromptFile, recorded.PromptFile)
	pick("workdir", &o.Workdir, recorded.Workdir)
	pick("model", &o.Model, recorded.Model)
	pick("timeout", &o.Timeout, recorded.Timeout)
	pick("memory-max", &o.MemoryMax, recorded.MemoryMax)
	if !changed("until-done") {
		o.UntilDone = recorded.UntilDone
	}
	if !changed("max-restarts") {
		o.Restarts = recorded.Restarts
	}
	if o.PromptFile == "" {
		return o, fmt.Errorf("no brief: --prompt-file is required the first time, and none is recorded")
	}
	abs, err := filepath.Abs(manifest.ExpandHome(o.PromptFile))
	if err != nil {
		return o, err
	}
	o.PromptFile = abs
	return o, nil
}

func (a *app) agentStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <vm>",
		Short: "Is it alive, what does it ask, has anything been tampered with, what does it say",
		Long: "One bounded read of the agent's state. Open asks come first, since they are\n" +
			"waiting on you. Then the unit, its restarts and last exit, what is queued\n" +
			"and what was last delivered, anything the guest refused (an item whose\n" +
			"signature failed, an unsigned inbox), and whether the guest build of rig,\n" +
			"the operator key and the runner are still what rig agent start wrote: rig\n" +
			"records their hashes on the instance, and a mismatch means something in the\n" +
			"guest replaced what decides who the operator is. Then STATUS.md.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			inst, h, err := a.harnessOf(name)
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
			ago := func(stamp string) string {
				t, err := time.Parse(time.RFC3339, stamp)
				if err != nil || p.Now == 0 {
					return ""
				}
				return agent.Age(p.Now-t.Unix()) + " ago"
			}

			// What is waiting on the operator goes first.
			if open := p.OpenAsks(); len(open) > 0 {
				fmt.Printf("OPEN ASKS — answer with: rig agent answer %s <id> <text>\n", name)
				for _, q := range open {
					fmt.Printf("  %-16s %-7s %s\n", q.ID, p.AskAge(q), agent.Clean(q.Text, 200))
				}
				fmt.Println()
			}

			row := func(k, v string) {
				if v == "" {
					v = "—"
				}
				fmt.Printf("  %-14s %s\n", k, v)
			}
			row("unit", p.Active)
			row("session", inst.Config[agent.SessionKey])
			row("restarts", p.Restarts)
			row("started with", describeOptions(agent.RecordedOptions(inst.Config)))

			// Whether the agent thinks it is finished, from the marker it sets
			// itself. Under --until-done this is the difference between "it
			// stopped because it is done" and "it stopped because it ran out of
			// restarts", which otherwise look identical from `unit inactive`.
			if p.Done {
				row("mission", "COMPLETE: "+agent.Clean(p.Summary, 200))
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
			// The exit codes with a single cause, named. 127 is the shell
			// saying the agent binary is not there; 77 is the runner refusing
			// to launch on what it cannot verify.
			switch get(agent.Dir + "/last_exit") {
			case "127":
				row("", "127 is \"command not found\": no "+h.Binary()+" on the unit's PATH.  rig agent install "+name)
			case strconv.Itoa(agent.ExitUnverified):
				row("", fmt.Sprintf("%d is a launch refused: the brief or the guest rig did not verify, and %s/stderr.log\n"+
					"                 says which. rig agent stop %s && rig agent start %s reinstalls both.",
					agent.ExitUnverified, agent.Dir, name, name))
			}

			queued := strconv.Itoa(p.Pending)
			if p.Pending > 0 {
				if path, ok := agent.MidTurnPath(h); ok {
					queued += " (signed; reaches the lead " + path.When + ", or at its next launch)"
				} else {
					queued += " (signed; at the agent's next launch, or when it runs rig inbox)"
				}
			}
			row("queued", queued)
			if p.Legacy {
				row("", "an older rig's unsigned inbox ("+agent.LegacyInboxPath+") has text in it; it is reported, never delivered")
			}
			if n := len(p.RecentDeliveries); n > 0 {
				d := p.RecentDeliveries[n-1]
				to := ""
				if d.To != "" {
					to = " to the " + d.To
				}
				row("last delivery", fmt.Sprintf("%s %s via %s%s, %s", d.Kind, d.ID, d.Path, to, ago(d.Time)))
			}
			if len(p.Asks) > 0 {
				row("asks", fmt.Sprintf("%d open, %d answered  (rig agent asks %s)", len(p.OpenAsks()), len(p.Asks)-len(p.OpenAsks()), name))
			}
			if n := len(p.RecentTampers); p.Tampers > 0 && n > 0 {
				t := p.RecentTampers[n-1]
				row("TAMPER", fmt.Sprintf("%d item(s) refused by the guest, never shown to the agent; last %s: %s (%s)",
					p.Tampers, ago(t.Time), agent.Clean(t.Reason, 160), agent.Clean(t.File, 80)))
			}
			row("installed", integrity(p, inst.Config, name))

			// Why it stopped, if it stopped badly. Reading it here means the
			// operator does not have to know events.jsonl exists.
			if p.Active != "active" {
				if raw, err := a.exec(name, h.TailResultsCmd(20000)); err == nil {
					if e := h.LastError(raw); e != "" {
						fmt.Printf("\n  last error   %s\n", h.ExplainError(e))
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

// integrity says whether what decides who the operator is in the guest is
// still what rig agent start wrote there.
func integrity(p agent.Probe, config map[string]string, name string) string {
	recorded := false
	for _, f := range agent.Installed {
		recorded = recorded || config[f.Key] != ""
	}
	switch bad := p.Mismatched(config); {
	case !recorded:
		return "not recorded (started by an older rig); rig agent start records it"
	case len(p.SHA256) == 0:
		return "" // the probe did not answer
	case len(bad) > 0:
		return "MISMATCH: " + strings.Join(bad, ", ") + " in " + agent.Dir + " is not what rig agent start wrote.\n" +
			"                 Something in the guest replaced it. rig agent stop " + name + " && rig agent start " + name + " reinstalls it."
	}
	return "guest rig, operator key and runner as rig agent start wrote them"
}

// describeOptions is the recorded start options in a line, as a bare start
// would use them.
func describeOptions(o agent.StartOptions) string {
	if o.PromptFile == "" {
		return ""
	}
	parts := []string{o.PromptFile}
	if o.UntilDone {
		parts = append(parts, "--until-done")
	}
	if o.Model != "" {
		parts = append(parts, "model "+o.Model)
	}
	for _, kv := range [][2]string{{"workdir", o.Workdir}, {"timeout", o.Timeout}, {"memory", o.MemoryMax}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+" "+kv[1])
		}
	}
	return strings.Join(append(parts, fmt.Sprintf("%d restarts", o.EffectiveRestarts())), ", ")
}

func (a *app) agentLogCmd() *cobra.Command {
	var lines int
	var follow, stderr bool
	cmd := &cobra.Command{
		Use:   "log <vm>",
		Short: "The agent's recent words, without the tool-call noise",
		Long: "Reads a bounded tail of the event stream and prints only what the agent\n" +
			"said. The raw log is megabytes of tool calls; this is the part worth an\n" +
			"operator's attention, and it stays small enough to read often.\n\n" +
			"--stderr prints the agent's stderr and the runner's notes instead: why a\n" +
			"launch was refused, a crash, each resume. -f keeps printing either as it\n" +
			"grows, reading the guest every few seconds, until interrupted.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			if stderr {
				read := func() ([]string, error) {
					out, err := a.exec(name, "tail -n 500 "+agent.StderrPath+" 2>/dev/null || true")
					if out = strings.TrimRight(out, "\n"); out == "" {
						return nil, err
					}
					return strings.Split(out, "\n"), err
				}
				all, err := read()
				if err != nil {
					return err
				}
				show := func(l string) { fmt.Println(l) }
				for _, l := range all[max(0, len(all)-lines):] {
					show(l)
				}
				if !follow {
					return nil
				}
				return followPolled(3*time.Second, all, read, show)
			}
			_, h, err := a.harnessOf(name)
			if err != nil {
				return err
			}
			// Filter and tail in the guest, so a huge log never crosses the
			// wire and the window is spent on speech rather than on the tool
			// output surrounding it.
			read := func(limit int) ([]string, bool, error) {
				out, err := a.exec(name, h.TailSpeechCmd(400000))
				if err != nil {
					return nil, false, err
				}
				return h.AssistantText(out, limit), strings.Contains(out, agent.NoEvents), nil
			}
			window := max(lines, 50)
			all, noEvents, err := read(window)
			if err != nil {
				return err
			}
			show := func(t string) { fmt.Printf("• %s\n\n", t) }
			switch {
			case noEvents && !follow:
				note("no event log yet — the agent has not started writing one.\n"+
					"  rig agent status %s   # is the unit even up", name)
				return nil
			case len(all) == 0 && !follow:
				// Now a real statement about the agent rather than about the
				// read: the window held only the agent's own events, and none
				// of them was speech.
				note("the agent has written events but said nothing yet; it is working in tools.\n"+
					"  rig agent status %s   # unit, restarts, and any last error", name)
				return nil
			}
			for _, t := range all[max(0, len(all)-lines):] {
				show(t)
			}
			if !follow {
				return nil
			}
			return followPolled(5*time.Second, all, func() ([]string, error) {
				texts, _, err := read(window)
				return texts, err
			}, show)
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 10, "how many of the most recent messages, or lines with --stderr, to show")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing as the agent goes on")
	cmd.Flags().BoolVar(&stderr, "stderr", false, "the agent's stderr and the runner's notes, rather than what it said")
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
