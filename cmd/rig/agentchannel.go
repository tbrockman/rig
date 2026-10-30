package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/agent"
	"github.com/tbrockman/rig/internal/agent/operator"
	"github.com/tbrockman/rig/internal/creds"
	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
)

// The operator's half of the channel to a running agent: what it sends is
// signed here, with a key that never leaves this host, and queued in the
// guest as one file per item; rig's guest build verifies each before the
// agent sees it. internal/agent has the formats, internal/agent/guest the
// other half.

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// put writes data to a guest path whole: a dotfile beside it, renamed into
// place in the guest. A reader there sees the old file or the new one, never
// a half-written one, and a binary that is running is replaced rather than
// written over.
func (a *app) put(name, guestPath string, data []byte, mode os.FileMode) error {
	dir, file := path.Split(guestPath)
	tmp := dir + "." + file + ".tmp"
	if err := a.c.WriteFile(name, tmp, data, mode); err != nil {
		return err
	}
	if out, err := a.exec(name, "mv -f -- "+shellQuote([]string{tmp, guestPath})); err != nil {
		return fmt.Errorf("renaming %s into place: %w\n%s", guestPath, err, out)
	}
	return nil
}

// channel is what a verb that writes to the agent needs: the session, and the
// host's key for the VM, checked against the one the guest was given.
type channel struct {
	name    string
	session string
	store   operator.Store
	key     ed25519.PrivateKey
}

func (a *app) openChannel(name string) (*channel, error) {
	if err := a.requireInstance(name); err != nil {
		return nil, err
	}
	inst, _, err := a.c.Instance(name)
	if err != nil {
		return nil, err
	}
	session := inst.Config[agent.SessionKey]
	if session == "" {
		return nil, fmt.Errorf("%s has no agent session yet.\n  rig agent start %s --prompt-file <brief>", name, name)
	}
	store, err := operator.For(name)
	if err != nil {
		return nil, err
	}
	key, err := store.Key()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no operator key for %s at %s. Is this the user that ran rig agent start?\n"+
			"  rig agent stop %s && rig agent start %s   # makes one and installs it", name, store.KeyPath(), name, name)
	}
	if err != nil {
		return nil, err
	}
	var pubKey string
	for _, f := range agent.Installed {
		if f.Name == agent.PubName {
			pubKey = inst.Config[f.Key]
		}
	}
	switch got := sha256Hex(agent.MarshalPublicKey(key.Public().(ed25519.PublicKey))); {
	case pubKey == "":
		return nil, fmt.Errorf("%s's agent was started by a rig that did not sign what it sends.\n"+
			"  rig agent stop %s && rig agent start %s", name, name, name)
	case got != pubKey:
		return nil, fmt.Errorf("%s is not the key rig agent start gave %s, so the guest would refuse "+
			"everything it signs. Another user (sudo changes the config directory), or a key made since?\n"+
			"  rig agent stop %s && rig agent start %s   # installs this one", store.KeyPath(), name, name, name)
	}
	return &channel{name: name, session: session, store: store, key: key}, nil
}

// deliver signs one item and queues it in the guest's inbox.d.
func (a *app) deliver(ch *channel, kind, id, body string) (agent.Envelope, error) {
	data, e, err := ch.store.Seal(ch.key, ch.session, kind, id, body, time.Now())
	if err != nil {
		return e, err
	}
	if err := a.c.Mkdir(ch.name, agent.InboxDir, 0o700); err != nil {
		return e, err
	}
	return e, a.put(ch.name, fmt.Sprintf("%s/%012d%s", agent.InboxDir, e.Seq, agent.EnvelopeExt), data, 0o600)
}

// putBrief signs the brief and writes it where the runner and `rig brief`
// read it.
func (a *app) putBrief(name string, store operator.Store, key ed25519.PrivateKey, session, text string) error {
	data, _, err := store.Seal(key, session, agent.KindBrief, "brief", text, time.Now())
	if err != nil {
		return err
	}
	return a.put(name, agent.BriefPath, data, 0o600)
}

// queued says where a sent item stands: how many wait, and when the harness
// will take them, from what it says of its own delivery paths rather than a
// promise written for one harness.
func (a *app) queued(name string, h agent.Harness) {
	out, _ := a.exec(name, "echo $(ls -1 "+agent.InboxDir+" 2>/dev/null | grep -c '\\"+agent.EnvelopeExt+"$') "+
		"$(systemctl is-active "+agent.Unit+" 2>/dev/null)")
	n, state, _ := strings.Cut(strings.TrimSpace(out), " ")
	if state != "active" {
		note("%s queued; the agent is not running (%s), and gets it when it next starts", n, state)
		return
	}
	if p, ok := agent.MidTurnPath(h); ok {
		note("%s queued; it reaches the lead %s", n, p.When)
		return
	}
	note("%s queued; the %s harness has no mid-turn delivery, so it waits for the agent's next launch unless the agent runs rig inbox or rig wait", n, h.Name())
}

// messageText is what a send or an answer says: its words, or with --file a
// file's contents (- for stdin), for text with lines and quotes in it.
func messageText(words []string, file string) (string, error) {
	var b []byte
	var err error
	switch {
	case file != "" && len(words) > 0:
		return "", errors.New("give the text as words or with --file, not both")
	case file == "-":
		b, err = io.ReadAll(os.Stdin)
	case file != "":
		b, err = os.ReadFile(file)
	case len(words) == 0:
		return "", errors.New("no text: give it as words, or with --file (- reads stdin)")
	default:
		return strings.Join(words, " "), nil
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("%s is empty", firstNonEmpty(strings.TrimPrefix(file, "-"), "stdin"))
	}
	return strings.TrimRight(string(b), "\n"), nil
}

func (a *app) agentSendCmd() *cobra.Command {
	var file string
	var atLaunch bool
	cmd := &cobra.Command{
		Use:   "send <vm> [<message>...]",
		Short: "Send the agent a message, delivered to the lead after its next tool call",
		Long: "Signs the message with this VM's operator key and queues it in the guest\n" +
			"(" + agent.InboxDir + ", one file per message, renamed into place). The\n" +
			"guest build of rig verifies it and delivers it to the lead, the process\n" +
			"that was given the brief, by the soonest path its harness has: for\n" +
			"claude-code, after the lead's next tool call through its PostToolUse hook\n" +
			"(a subagent's tool call fires the same hook and is refused, so a message\n" +
			"cannot land in a subagent), else at its next launch, in the prompt. A turn\n" +
			"can run for hours, and the message does not wait for it to end. Nothing is\n" +
			"held open, so neither side can hang on the other. `rig agent status` and\n" +
			"`watch` say when it was delivered, and to whom.\n\n" +
			"A harness with no mid-turn path is refused unless --at-launch says that\n" +
			"waiting for the next launch is fine; its brief should tell the agent to run\n" +
			"`rig inbox` at checkpoints instead.\n\n" +
			"--file sends a file's contents instead of words, and --file - reads stdin.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			text, err := messageText(args[1:], file)
			if err != nil {
				return err
			}
			ch, err := a.openChannel(args[0])
			if err != nil {
				return err
			}
			_, h, err := a.harnessOf(ch.name)
			if err != nil {
				return err
			}
			if _, ok := agent.MidTurnPath(h); !ok && !atLaunch {
				return fmt.Errorf("the %s harness has no mid-turn delivery: nothing runs between the agent's tool calls to take a message, "+
					"so this would wait for its next launch. Say so with --at-launch, and have the brief tell the agent to run `rig inbox` at checkpoints.", h.Name())
			}
			e, err := a.deliver(ch, agent.KindMessage, "", text)
			if err != nil {
				return err
			}
			note("sent %s to %s", e.ID, ch.name)
			a.queued(ch.name, h)
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "send this file's contents; - reads stdin")
	cmd.Flags().BoolVar(&atLaunch, "at-launch", false, "queue it for the agent's next launch even when its harness cannot deliver mid-turn")
	return cmd
}

func (a *app) agentAnswerCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "answer <vm> <id> [<text>...]",
		Short: "Answer one of the agent's asks",
		Long: "Answers ask <id>, which `rig agent asks` and `status` list. Signed and\n" +
			"queued like a message: an agent blocked in `rig ask --wait` or `rig wait`\n" +
			"prints it, and otherwise it arrives after the agent's next tool call.\n" +
			"Answering again replaces the earlier answer for an agent that has not\n" +
			"read it yet, and reaches one that has as a second answer.\n\n" +
			"--file answers with a file's contents, and --file - reads stdin.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			name, id := args[0], args[1]
			if !agent.ValidID(id) {
				return fmt.Errorf("%q is not an ask id", id)
			}
			text, err := messageText(args[2:], file)
			if err != nil {
				return err
			}
			ch, err := a.openChannel(name)
			if err != nil {
				return err
			}
			out, _ := a.exec(name, fmt.Sprintf("test -e %[1]s/%[2]s.json && echo asked; test -e %[3]s/%[2]s%[4]s && echo answered",
				agent.AsksDir, id, agent.Dir+"/"+agent.AnswersName, agent.EnvelopeExt))
			if !strings.Contains(out, "asked") {
				return fmt.Errorf("%s has no ask %q.\n  rig agent asks %s", name, id, name)
			}
			if strings.Contains(out, "answered") {
				note("%q was answered already; sending this one too", id)
			}
			if _, err := a.deliver(ch, agent.KindAnswer, id, text); err != nil {
				return err
			}
			_, h, err := a.harnessOf(name)
			if err != nil {
				return err
			}
			note("answered %q on %s", id, name)
			a.queued(name, h)
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "answer with this file's contents; - reads stdin")
	return cmd
}

func (a *app) agentAsksCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "asks <vm>",
		Short: "The agent's questions: open ones first, then answered",
		Long: "Lists what the agent asked with `rig ask`, newest first within each group,\n" +
			"the open ones first. Answer one with `rig agent answer <vm> <id> <text>`.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			raw, err := a.exec(name, agent.StatusProbe())
			if err != nil {
				return err
			}
			p := agent.ParseProbe(raw)
			if len(p.Asks) == 0 {
				note("no asks from the agent in %s", name)
				return nil
			}
			for _, q := range p.OpenAsks() {
				fmt.Printf("OPEN      %-16s %-7s %s\n", q.ID, p.AskAge(q), agent.Clean(q.Text, 300))
			}
			for _, q := range p.Asks {
				ans, ok := p.Answers[q.ID]
				if !ok {
					continue
				}
				fmt.Printf("answered  %-16s %-7s %s\n", q.ID, p.AskAge(q), agent.Clean(q.Text, 300))
				fmt.Printf("          %-16s %-7s -> %s\n", "", "", agent.Clean(ans.Body, 300))
			}
			return nil
		},
	}
}

func (a *app) agentBriefCmd() *cobra.Command {
	var promptFile string
	cmd := &cobra.Command{
		Use:   "brief <vm>",
		Short: "Change the running agent's brief: it gets the diff, with no restart",
		Long: "Re-reads the brief (--prompt-file, else the file rig agent start recorded),\n" +
			"diffs it against the brief last delivered from this host, writes the new one\n" +
			"to the guest signed, and sends the agent the diff as a signed message, which\n" +
			"arrives after its next tool call, in order with everything else you sent.\n" +
			"The agent's `rig brief` prints the whole new text. Persistent rules belong in\n" +
			"the brief, and this is how they change.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			ch, err := a.openChannel(name)
			if err != nil {
				return err
			}
			inst, _, err := a.c.Instance(name)
			if err != nil {
				return err
			}
			given := agent.StartOptions{PromptFile: promptFile}
			rec := agent.RecordedOptions(inst.Config)
			spec, err := recordedAgent(inst)
			if err != nil {
				return err
			}
			o, err := startOptions(cmd.Flags().Changed, agent.StartOptions{PromptFile: rec.PromptFile},
				agent.StartOptions{PromptFile: spec.PromptFile}, given)
			if err != nil {
				return err
			}
			text, err := os.ReadFile(o.PromptFile)
			if err != nil {
				return err
			}
			delivered, ok, err := ch.store.Brief()
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no brief was delivered to %s from this host, so there is nothing to diff against.\n"+
					"  rig agent stop %s && rig agent start %s --prompt-file %s", name, name, name, o.PromptFile)
			}
			if string(text) == delivered {
				note("%s is the brief %s already has; nothing sent", o.PromptFile, name)
				return nil
			}
			diff := agent.UnifiedDiff("brief (as delivered)", "brief (now)", delivered, string(text))
			// The whole text first, then the diff that announces it, so an
			// agent that reads the diff and runs `rig brief` finds the new one.
			if err := a.putBrief(name, ch.store, ch.key, ch.session, string(text)); err != nil {
				return err
			}
			if _, err := a.deliver(ch, agent.KindBriefUpdate, "brief", diff); err != nil {
				return err
			}
			if err := ch.store.SetBrief(string(text)); err != nil {
				return err
			}
			if o.PromptFile != rec.PromptFile {
				if err := a.c.SetConfigKey(name, agent.OptionKey("prompt-file"), o.PromptFile); err != nil {
					return err
				}
			}
			added, removed := diffStat(diff)
			note("brief updated on %s (+%d -%d lines)", name, added, removed)
			_, h, err := a.harnessOf(name)
			if err != nil {
				return err
			}
			a.queued(name, h)
			return nil
		},
	}
	cmd.Flags().StringVar(&promptFile, "prompt-file", "", "host file holding the new brief; recorded (default: the one rig agent start recorded)")
	return cmd
}

func diffStat(diff string) (added, removed int) {
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			added++
		case strings.HasPrefix(l, "-"):
			removed++
		}
	}
	return added, removed
}

func (a *app) agentWatchCmd() *cobra.Command {
	var interval time.Duration
	var run string
	cmd := &cobra.Command{
		Use:   "watch <vm>",
		Short: "One line per change in the agent's state, for a person or a notifier",
		Long: "Polls the guest every --interval with the same bounded read as status, and\n" +
			"prints one line per change, until interrupted:\n\n" +
			"  <time> <vm> <event> key=value ...\n\n" +
			"time is RFC 3339 UTC; values are quoted in Go syntax when they need it, and\n" +
			"text from the guest is cut and cleaned to one line. The events:\n" +
			"  watching        first: unit, restarts, open-asks, pending\n" +
			"  ask-opened      id, text   (also for each open ask when watch starts)\n" +
			"  ask-answered    id\n" +
			"  delivered       seq, kind, id, path (launch, hook, wait or inbox), to (lead, or caller for a verb)\n" +
			"  tamper          file, reason: something the guest refused to deliver\n" +
			"  integrity       file: an installed file is no longer what rig agent start wrote\n" +
			"  done            summary: the agent ran rig done\n" +
			"  budget          reason: a budget stopped the agent (session kept)\n" +
			"  watchdog        reason: a run's limit cut it; it resumes with a note\n" +
			"  unit            from, to: the unit's state\n" +
			"  restarts        from, to\n" +
			"  status-changed  STATUS.md changed\n" +
			"  unreachable     error: the read failed (once, until it works again: reachable)\n\n" +
			"The manifest's guest.agent.watch thresholds, each when crossed and when\n" +
			"cleared (state=crossed|cleared), with the value and the limit:\n" +
			"  idle            age, limit: no new commit on the workdir's branch\n" +
			"  long-unit       unit, age, limit: a transient unit the agent started, still running\n" +
			"  stale-status    age, limit: STATUS.md untouched\n" +
			"  usage           window, utilization, limit: a subscription window (read every 5 minutes)\n" +
			"  forward-down    port, reason: a declared forward with no holder here or no listener there (forward-up when it is back)\n" +
			"  sent            threshold, id: the message guest.agent.watch.on named for a crossing was queued\n" +
			"A usage budget (guest.agent.budgets.usage) is judged here too, since it\n" +
			"needs the credential: crossing it stops the agent as rig agent stop would.\n\n" +
			"--exec runs a command for each event, through sh -c, with the line in\n" +
			"RIG_EVENT, the event name in RIG_EVENT_TYPE and the VM in RIG_VM; for\n" +
			"example, a desktop notification:\n" +
			"  rig agent watch myproj --exec 'notify-send \"rig\" \"$RIG_EVENT\"'",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			if interval < time.Second {
				return fmt.Errorf("--interval %s: once a second is the most often", interval)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			emit := func(e agent.Event) {
				line := e.Line(time.Now(), name)
				fmt.Println(line)
				if run != "" {
					runForEvent(ctx, run, name, e, line)
				}
			}
			w, err := a.newWatcher(name)
			if err != nil {
				return err
			}
			var prev *agent.Probe
			var readings []agent.Reading
			down := false
			for {
				inst, _, err := a.c.Instance(name)
				raw := ""
				if err == nil {
					raw, err = a.exec(name, agent.StatusProbeWith(w.probe))
				}
				switch {
				case err != nil && !down:
					emit(agent.NewEvent(agent.EvUnreachable, "error", err.Error()))
					down = true
				case err == nil:
					if down {
						emit(agent.NewEvent(agent.EvReachable))
						down = false
					}
					cur := agent.ParseProbe(raw)
					for _, e := range agent.Changes(prev, cur, inst.Config) {
						emit(e)
					}
					prev = &cur
					// The thresholds, and the one budget this host judges.
					quota := w.quota(inst, emit)
					now := agent.Evaluate(w.thresholds, cur, quota, forwardActive(name))
					for _, e := range agent.ThresholdEvents(readings, now) {
						emit(e)
						if id, ok := w.react(e); ok {
							emit(agent.NewEvent(agent.EvSent, "threshold", e.Type, "id", id))
						}
					}
					readings = now
					if reason, over := w.usageBudgetOver(quota); over && cur.Active == "active" && cur.Budget == "" {
						if err := a.stopForBudget(name, reason); err != nil {
							emit(agent.NewEvent(agent.EvUnreachable, "error", "stopping for the usage budget: "+err.Error()))
						}
					}
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(interval):
				}
			}
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Second, "how often to read the guest")
	cmd.Flags().StringVar(&run, "exec", "", "a command to run for each event, with it in RIG_EVENT")
	return cmd
}

// watcher is what `rig agent watch` carries across polls beyond the probe:
// the manifest's thresholds, the quota it reads now and then, and the
// messages a crossing sends.
type watcher struct {
	a          *app
	name       string
	harness    agent.Harness
	spec       manifest.Agent
	probe      agent.ProbeOpts
	thresholds agent.Thresholds
	envFile    string
	lastQuota  time.Time
	cached     *agent.Quota
	quotaErr   string // said once, until it changes
}

func (a *app) newWatcher(name string) (*watcher, error) {
	inst, h, err := a.harnessOf(name)
	if err != nil {
		return nil, err
	}
	spec, err := recordedAgent(inst)
	if err != nil {
		return nil, err
	}
	forwards, err := recordedForwards(inst)
	if err != nil {
		return nil, err
	}
	return &watcher{
		a: a, name: name, harness: h, spec: spec,
		probe:      probeOpts(inst, spec),
		thresholds: thresholdsOf(spec, forwards),
		envFile:    inst.Config[creds.InstanceKey],
	}, nil
}

// probeOpts is what the probe needs of the run: the workdir the last start
// recorded, else the file's, else the default; and the units glob.
func probeOpts(inst *incus.Instance, spec manifest.Agent) agent.ProbeOpts {
	o := agent.ProbeOpts{Workdir: agent.RecordedOptions(inst.Config).Workdir}
	if o.Workdir == "" {
		o.Workdir = spec.Workdir
	}
	if o.Workdir == "" {
		o.Workdir = "/work"
	}
	if spec.Watch != nil {
		o.Units = spec.Watch.Units
	}
	return o
}

// thresholdsOf is the manifest's watch block in the watch's terms, plus the
// declared forwards, which are watched whether or not a watch block exists.
func thresholdsOf(spec manifest.Agent, forwards []int) agent.Thresholds {
	t := agent.Thresholds{Forwards: forwards}
	if w := spec.Watch; w != nil {
		t.Idle = time.Duration(w.IdleMinutes) * time.Minute
		t.Unit = time.Duration(w.UnitMinutes) * time.Minute
		t.Status = time.Duration(w.StatusMinutes) * time.Minute
		if w.Usage != nil {
			t.Usage = &agent.UsageLimit{Window: w.Usage.Window, Max: w.Usage.Max}
		}
	}
	return t
}

// wantsQuota is whether anything in the manifest asks about a window.
func (w *watcher) wantsQuota() bool {
	return w.thresholds.Usage != nil || (w.spec.Budgets != nil && w.spec.Budgets.Usage != nil)
}

// quota is the credential's windows, read every five minutes when something
// asks about them: the endpoint is the provider's, not to be polled every
// few seconds. A read that fails is said once.
func (w *watcher) quota(inst *incus.Instance, emit func(agent.Event)) *agent.Quota {
	if !w.wantsQuota() || time.Since(w.lastQuota) < 5*time.Minute {
		return w.cached
	}
	w.lastQuota = time.Now()
	q, ok, err := w.a.quotaOf(inst.Config[creds.InstanceKey], w.harness)
	switch {
	case err != nil && err.Error() != w.quotaErr:
		w.quotaErr = err.Error()
		emit(agent.NewEvent(agent.EvUnreachable, "error", "quota: "+err.Error()))
	case err == nil:
		w.quotaErr = ""
		if ok {
			w.cached = q
		}
	}
	return w.cached
}

// react sends the message guest.agent.watch.on names for a threshold that
// was just crossed, and returns the message's id.
func (w *watcher) react(e agent.Event) (string, bool) {
	if w.spec.Watch == nil || e.Value("state") != "crossed" {
		return "", false
	}
	name := map[string]string{agent.EvIdle: "idle", agent.EvLongUnit: "unit", agent.EvStaleStatus: "status", agent.EvUsage: "usage"}[e.Type]
	action, ok := w.spec.Watch.On[name]
	if !ok || action.Send == "" {
		return "", false
	}
	ch, err := w.a.openChannel(w.name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rig: cannot send for %s: %v\n", name, err)
		return "", false
	}
	env, err := w.a.deliver(ch, agent.KindMessage, "", action.Send)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rig: cannot send for %s: %v\n", name, err)
		return "", false
	}
	return env.ID, true
}

// usageBudgetOver is whether the manifest's usage budget is crossed.
func (w *watcher) usageBudgetOver(q *agent.Quota) (string, bool) {
	if w.spec.Budgets == nil || w.spec.Budgets.Usage == nil {
		return "", false
	}
	u := w.spec.Budgets.Usage
	win, ok := q.Window(u.Window)
	if !ok || win.Utilization <= u.Max {
		return "", false
	}
	return fmt.Sprintf("usage: %s window at %.0f%% of %.0f%%", u.Window, win.Utilization*100, u.Max*100), true
}

// quotaOf reads the credential file's variables, has the harness name the
// request, and makes it: the credential goes to the provider and nowhere
// else, and is never printed.
func (a *app) quotaOf(envFile string, h agent.Harness) (*agent.Quota, bool, error) {
	if envFile == "" {
		return nil, false, nil
	}
	vars, err := creds.Values(envFile)
	if err != nil {
		return nil, true, err
	}
	reqs, err := h.QuotaRequests(vars)
	if err != nil || len(reqs) == 0 {
		return nil, false, err
	}
	// Best source first; the next is tried when the provider refuses one
	// (a setup-token lacks the scope), and the last refusal is the error.
	for _, req := range reqs {
		q, err := fetchQuota(req)
		if err == nil {
			q.Source = req.Source
			return q, true, nil
		}
		if !errors.Is(err, errRefused) {
			return nil, true, err
		}
		if req.Source == reqs[len(reqs)-1].Source {
			return nil, true, fmt.Errorf("%s: %w", req.Source, err)
		}
	}
	return nil, true, nil
}

// errRefused is the provider answering, and saying no: the next source may
// do better.
var errRefused = errors.New("refused")

// quotaClient makes the quota request; a test points it elsewhere.
var quotaClient = &http.Client{Timeout: 20 * time.Second}

func fetchQuota(req agent.QuotaRequest) (*agent.Quota, error) {
	r, err := http.NewRequest(http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range req.Headers {
		r.Header.Set(k, v)
	}
	resp, err := quotaClient.Do(r)
	if err != nil {
		return nil, fmt.Errorf("reading the subscription's usage: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: the usage endpoint answered %s: %s", errRefused, resp.Status, agent.Clean(string(body), 200))
	}
	return req.Parse(body, time.Now())
}

// stopForBudget does in the guest what the runner does for a budget it
// judges itself: the marker, then the stop. The next probe reports it.
func (a *app) stopForBudget(name, reason string) error {
	cmd := fmt.Sprintf("printf '%%s\\n%%s\\n' %s \"$(date -Is)\" > %s/%s && systemctl stop %s",
		shellQuote([]string{reason}), agent.Dir, agent.BudgetName, agent.Unit)
	_, err := a.exec(name, cmd)
	return err
}

// runForEvent runs --exec's command for one event. Its output goes to stderr,
// so stdout stays the event lines; a command that hangs is stopped after
// thirty seconds rather than stopping the watch.
func runForEvent(ctx context.Context, command, vm string, e agent.Event, line string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", command)
	c.Env = append(os.Environ(), "RIG_EVENT="+line, "RIG_EVENT_TYPE="+e.Type, "RIG_VM="+vm)
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	if err := c.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "rig: --exec for %s: %v\n", e.Type, err)
	}
}
