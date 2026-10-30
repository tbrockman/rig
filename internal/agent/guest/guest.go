// Package guest is what the guest build of rig does. It takes what the host
// sent from inbox.d, verifies it, and shows the agent only what verified, by
// whichever path reaches the agent first: the runner at launch, Claude Code's
// PostToolUse hook after a tool call, or `rig ask --wait`, `rig wait` and
// `rig inbox`. It also keeps the agent's side of the channel: its questions,
// the brief, and the DONE marker.
//
// "The agent" is the lead: the process that was given the brief and answers
// the operator. Its subagents run the same hooks and the same verbs, and an
// operator message that reaches one of them is lost: it says it will pass it
// on and does not (DESIGN.md). So the hook takes nothing unless its input
// proves the lead (LeadProof), and what reached the agent by a verb is
// recorded as reaching the caller, whoever that was.
//
// Every path takes an envelope the same way: rename it out of inbox.d into
// claimed/, which only one taker can do, then verify and settle it under a
// lock, in sequence order. The lock is what keeps acceptance in order when two
// takers run at once (the hook, and a `rig wait` in a background shell); the
// rename is what lets a taker that died mid-way leave its items where the next
// one finds them.
package guest

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tbrockman/rig/internal/agent"
)

// Box is one agent's directory, and the session it runs.
type Box struct {
	Dir     string
	Session string
	Now     func() time.Time
}

// FromEnv is the box the runner set up: RIG_AGENT_DIR (else agent.Dir) and
// RIG_AGENT_SESSION, which the unit sets and claude and its tools inherit.
func FromEnv() Box {
	return Box{Dir: agent.DirFromEnv(), Session: os.Getenv("RIG_AGENT_SESSION"), Now: time.Now}
}

// ErrNoSession is a verb that needs to know the session run outside the unit.
var ErrNoSession = errors.New("RIG_AGENT_SESSION is not set; this runs under the " + agent.Unit + " unit, which sets it")

func (b Box) path(name ...string) string {
	return filepath.Join(append([]string{b.Dir}, name...)...)
}

func (b Box) now() time.Time {
	if b.Now == nil {
		return time.Now()
	}
	return b.Now()
}

func (b Box) operatorKey() (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(b.path(agent.PubName))
	if err != nil {
		return nil, fmt.Errorf("no operator key (%w); rig agent start installs it", err)
	}
	return agent.ParsePublicKey(raw)
}

// lock serialises everything that reads or advances a sequence number.
func (b Box) lock() (func(), error) {
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(b.path(agent.LockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil // closing the last descriptor releases the lock
}

func (b Box) readSeq(name string) uint64 {
	raw, err := os.ReadFile(b.path(name))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return n
}

func (b Box) writeSeq(name string, n uint64) error {
	return writeAtomic(b.path(name), []byte(strconv.FormatUint(n, 10)+"\n"), 0o600)
}

// writeAtomic writes a temp file beside path and renames it into place, so a
// reader sees the old content or the new and never half of either.
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// pending lists dir's entries in name order. A dotfile is a sender's
// half-written temp file, which it renames into place when it is whole.
func pending(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, agent.MaxEnvelope+1))
	if err == nil && len(b) > agent.MaxEnvelope {
		err = fmt.Errorf("%w: over %d bytes", agent.ErrMalformed, agent.MaxEnvelope)
	}
	return b, err
}

func (b Box) appendJSON(name string, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(b.path(name), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// reject moves path aside into rejected/ and logs why. It is never shown to
// the agent: the operator sees it in `rig agent status` and `watch`.
func (b Box) reject(path string, why error) {
	dir := b.path(agent.RejectedName)
	_ = os.MkdirAll(dir, 0o700)
	name := fmt.Sprintf("%s.%d", filepath.Base(path), b.now().UnixNano())
	file := filepath.ToSlash(filepath.Join(agent.RejectedName, name))
	if err := os.Rename(path, filepath.Join(dir, name)); err != nil {
		file = filepath.Base(path) + " (left in place: " + err.Error() + ")"
	}
	_ = b.appendJSON(agent.TamperName, agent.Tamper{Time: agent.Stamp(b.now()), File: file, Reason: why.Error()})
}

// reportLegacy deals with the inbox an older rig wrote: unsigned text, which
// is exactly what must never reach the agent as the operator's. It is moved
// aside and reported, like anything else that does not verify.
func (b Box) reportLegacy() {
	p := b.path(agent.LegacyInboxName)
	st, err := os.Stat(p)
	switch {
	case err != nil || st.IsDir():
		return
	case st.Size() == 0:
		_ = os.Remove(p)
		return
	}
	b.reject(p, errors.New("unsigned text in the inbox an older rig wrote; not delivered (rig agent send signs messages now)"))
}

// Consume takes everything waiting in inbox.d and returns what verified, in
// sequence order, recording each as delivered by via, to whom (agent.ToLead
// or agent.ToCaller). What fails is moved to rejected/ and logged, and never
// returned. With no operator key or no session it takes nothing, and says so.
func (b Box) Consume(via, to string) ([]agent.Envelope, error) {
	b.reportLegacy()
	inbox, claimed := b.path(agent.InboxName), b.path(agent.ClaimedName)
	if len(pending(inbox)) == 0 && len(pending(claimed)) == 0 {
		return nil, nil // the hook runs after every tool call; this is its usual answer
	}
	if b.Session == "" {
		return nil, ErrNoSession
	}
	pub, err := b.operatorKey()
	if err != nil {
		return nil, err
	}
	unlock, err := b.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	if err := os.MkdirAll(claimed, 0o700); err != nil {
		return nil, err
	}
	for _, n := range pending(inbox) {
		err := os.Rename(filepath.Join(inbox, n), filepath.Join(claimed, n))
		if err != nil && !errors.Is(err, fs.ErrNotExist) { // another taker got it first
			return nil, err
		}
	}

	type item struct {
		path string
		e    agent.Envelope
	}
	var good []item
	for _, n := range pending(claimed) {
		p := filepath.Join(claimed, n)
		data, err := readBounded(p)
		var e agent.Envelope
		if err == nil {
			e, err = agent.Open(pub, data)
		}
		if err == nil && !agent.IsInboxKind(e.Kind) {
			err = fmt.Errorf("%w: a %s does not come through the inbox", agent.ErrMalformed, e.Kind)
		}
		if err != nil {
			b.reject(p, err)
			continue
		}
		good = append(good, item{p, e})
	}
	// By the signed number, not the file name: a name is anyone's to change.
	sort.SliceStable(good, func(i, j int) bool { return good[i].e.Seq < good[j].e.Seq })

	last := b.readSeq(agent.SeqName)
	var out []agent.Envelope
	for _, it := range good {
		if err := it.e.Admit(b.Session, last); err != nil {
			b.reject(it.path, err)
			continue
		}
		if err := b.writeSeq(agent.SeqName, it.e.Seq); err != nil {
			return out, err
		}
		last = it.e.Seq
		dest := b.path(agent.DeliveredName, fmt.Sprintf("%012d%s", it.e.Seq, agent.EnvelopeExt))
		if it.e.Kind == agent.KindAnswer {
			dest = b.path(agent.AnswersName, it.e.ID+agent.EnvelopeExt)
		}
		_ = os.MkdirAll(filepath.Dir(dest), 0o700)
		if err := os.Rename(it.path, dest); err != nil {
			return out, err
		}
		_ = b.appendJSON(agent.DeliveriesName, agent.Delivery{
			Time: agent.Stamp(b.now()), Seq: it.e.Seq, Kind: it.e.Kind, ID: it.e.ID, Path: via, To: to,
		})
		out = append(out, it.e)
	}
	return out, nil
}

// Token is this launch's token, which the runner wrote and told the agent
// about in its system prompt.
func (b Box) Token() string {
	raw, err := os.ReadFile(b.path(agent.TokenName))
	if t := strings.TrimSpace(string(raw)); err == nil && t != "" {
		return t
	}
	return "(none: no launch token)"
}

// Render is how verified items read to the agent. Each says what it is, that
// rig verified it, and this launch's token; text that only looks like this,
// without the token, was written by someone who did not have it.
func (b Box) Render(items []agent.Envelope) string {
	token := b.Token()
	var s strings.Builder
	for i, e := range items {
		var head, foot string
		switch e.Kind {
		case agent.KindAnswer:
			head = fmt.Sprintf("[rig: the operator's answer to your ask %q; verified by rig; token %s; sent %s]", e.ID, token, e.Time)
			foot = fmt.Sprintf("[rig: end of the answer to %q]", e.ID)
		case agent.KindBriefUpdate:
			head = fmt.Sprintf("[rig: the operator changed your brief (update #%d); verified by rig; token %s; sent %s. "+
				"This diff is against the brief you had; `rig brief` prints the whole current text.]", e.Seq, token, e.Time)
			foot = fmt.Sprintf("[rig: end of brief update #%d]", e.Seq)
		default:
			head = fmt.Sprintf("[rig: operator message %s; verified by rig; token %s; sent %s]", e.ID, token, e.Time)
			foot = fmt.Sprintf("[rig: end of operator message %s]", e.ID)
		}
		if i > 0 {
			s.WriteString("\n")
		}
		s.WriteString(head + "\n" + strings.TrimRight(e.Body, "\n") + "\n" + foot + "\n")
	}
	return s.String()
}

// Launch is the runner's delivery, for the prompt: first what reached the
// agent mid-turn since the previous launch, by id and first line, so a
// resumed lead is reminded of what a hook added to a conversation it may
// have compacted since; then what is queued, rendered. It marks where the
// delivery log stood, so the next launch knows where to list from.
func (b Box) Launch(w io.Writer) error {
	_ = os.Remove(b.path(agent.UnprovenName))
	if lines := b.deliveredSince(b.lastLaunch()); len(lines) > 0 {
		fmt.Fprintf(w, "--- OPERATOR MESSAGES DELIVERED DURING YOUR PREVIOUS RUN (after a tool call; they are in your conversation already; act on any you have not) ---\n%s\n\n",
			strings.Join(lines, "\n"))
	}
	items, err := b.Consume(agent.ViaLaunch, agent.ToLead)
	if len(items) > 0 {
		fmt.Fprintf(w, "--- OPERATOR MESSAGES (queued since your last turn; verified by rig; act on them now) ---\n%s",
			b.Render(items))
	}
	mark := fmt.Sprintf("%s %d\n", agent.Stamp(b.now()), b.deliveryLines())
	if werr := writeAtomic(b.path(agent.LastLaunchName), []byte(mark), 0o600); werr != nil && err == nil {
		err = werr
	}
	return err
}

// deliveryLines is how long deliveries.jsonl is, in lines.
func (b Box) deliveryLines() int {
	raw, err := os.ReadFile(b.path(agent.DeliveriesName))
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

// lastLaunch is where deliveries.jsonl stood when the runner last launched
// the agent: the line count the launch recorded, or 0. A count rather than
// a time, so a delivery in the same second as a launch is listed once.
func (b Box) lastLaunch() int {
	raw, err := os.ReadFile(b.path(agent.LastLaunchName))
	if err != nil {
		return 0
	}
	_, count, _ := strings.Cut(strings.TrimSpace(string(raw)), " ")
	n, _ := strconv.Atoi(count)
	return n
}

// deliveredSince lists what reached the agent other than at a launch, from
// line from of deliveries.jsonl on, one line each: id, kind, path, time, and
// the first line of the body, read back from delivered/ or answers/ and
// verified again. A body that cannot be read back is said to be missing
// rather than invented.
func (b Box) deliveredSince(from int) []string {
	raw, err := os.ReadFile(b.path(agent.DeliveriesName))
	if err != nil {
		return nil
	}
	pub, _ := b.operatorKey()
	var out []string
	for i, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var d agent.Delivery
		if i < from || json.Unmarshal([]byte(line), &d) != nil || d.Path == agent.ViaLaunch {
			continue
		}
		file := b.path(agent.DeliveredName, fmt.Sprintf("%012d%s", d.Seq, agent.EnvelopeExt))
		if d.Kind == agent.KindAnswer {
			file = b.path(agent.AnswersName, d.ID+agent.EnvelopeExt)
		}
		first := "(body not on disk)"
		if data, err := readBounded(file); err == nil {
			if e, err := agent.Open(pub, data); err == nil {
				first, _, _ = strings.Cut(strings.TrimSpace(e.Body), "\n")
				first = agent.Clean(first, 160)
			}
		}
		to := d.To
		if to == "" {
			to = "?"
		}
		out = append(out, fmt.Sprintf("%s  %s via %s to %s, %s: %s", d.ID, d.Kind, d.Path, to, d.Time, first))
	}
	return out
}

// HookInput is what Claude Code writes to a hook's stdin, the fields rig
// reads. A subagent's tool call fires the same hook with agent_id and
// agent_type set; the lead's carries neither, and session_id and
// transcript_path are the lead's in both (verified on 2.1.314; DEV_NOTES).
type HookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	HookEventName  string `json:"hook_event_name"`
	AgentID        string `json:"agent_id"`
	AgentType      string `json:"agent_type"`
	ToolName       string `json:"tool_name"`
}

// LeadProof says whether a hook input proves the hook ran in the lead's own
// process for session. The lead is proven, not assumed: the input must name
// the session rig chose and carry no subagent identity. An input that names
// no session is a hook shape rig does not know and proves nothing; the hook
// then holds what is queued for the launch prompt rather than guess.
func LeadProof(in HookInput, session string) (lead bool, reason string) {
	switch {
	case in.SessionID == "":
		return false, "the hook's input names no session_id, so it cannot be placed; holding what is queued for the next launch"
	case session != "" && in.SessionID != session:
		return false, fmt.Sprintf("the hook ran in session %s, not the agent's %s; holding what is queued", in.SessionID, session)
	case in.AgentID != "":
		return false, fmt.Sprintf("the hook ran in subagent %s (%s), not the lead", in.AgentID, in.AgentType)
	}
	return true, ""
}

// noteUnproven logs why a hook took nothing, once per launch: the runner's
// next launch removes the marker. Said in stderr.log, where `rig agent log
// --stderr` reads, since a message that never arrives is otherwise silent.
func (b Box) noteUnproven(reason string) {
	marker := b.path(agent.UnprovenName)
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return // said already this launch
	}
	f.Close()
	if log, err := os.OpenFile(b.path("stderr.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
		fmt.Fprintf(log, "[%s] rig hook post-tool-use: %s\n", agent.Stamp(b.now()), reason)
		log.Close()
	}
}

// HookOutput is what a PostToolUse hook prints to add context for the model.
type HookOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// PostToolUse is the hook's delivery: what arrived while the agent worked,
// as context Claude Code adds after the tool call that just finished, read
// from the hook's input on r. Only when that input proves the lead: a
// subagent's tool call fires the same hook, and a message delivered there is
// lost. With nothing to deliver, or no proof, it prints nothing, which is
// the hook saying nothing; what is queued waits for the lead's next tool
// call, or its next launch.
func (b Box) PostToolUse(r io.Reader, w io.Writer) error {
	var in HookInput
	if r != nil {
		raw, err := io.ReadAll(r) // all of it: a hook that leaves its input unread breaks the pipe
		if err == nil {
			_ = json.Unmarshal(raw, &in) // a shape rig cannot read proves nothing, and LeadProof says so
		}
	}
	if lead, why := LeadProof(in, b.Session); !lead {
		if in.AgentID == "" { // a subagent is the ordinary case; anything else is worth a line
			b.noteUnproven(why)
		}
		return nil
	}
	items, err := b.Consume(agent.ViaHook, agent.ToLead)
	if len(items) > 0 {
		var out HookOutput
		out.HookSpecificOutput.HookEventName = "PostToolUse"
		out.HookSpecificOutput.AdditionalContext = "rig delivered this while you were working; act on it now.\n\n" + b.Render(items)
		if e := json.NewEncoder(w).Encode(out); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// Settings is the --settings JSON that makes exe the PostToolUse hook for
// every tool.
func Settings(exe string) ([]byte, error) {
	type command struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Matcher string    `json:"matcher"`
		Hooks   []command `json:"hooks"`
	}
	quoted := "'" + strings.ReplaceAll(exe, "'", `'\''`) + "'"
	return json.Marshal(map[string]any{"hooks": map[string][]matcher{
		"PostToolUse": {{Matcher: "*", Hooks: []command{{Type: "command", Command: quoted + " hook post-tool-use"}}}},
	}})
}

// Brief is the verified brief: brief.msg, signed for this session, and no
// older than the newest brief already seen here. Anything less is an error,
// logged as tamper; the runner will not launch on it.
func (b Box) Brief() (string, error) {
	if b.Session == "" {
		return "", ErrNoSession
	}
	p := b.path(agent.BriefName)
	fail := func(err error) (string, error) {
		_ = b.appendJSON(agent.TamperName, agent.Tamper{Time: agent.Stamp(b.now()), File: agent.BriefName, Reason: err.Error()})
		return "", fmt.Errorf("the brief (%s) did not verify: %w", p, err)
	}
	pub, err := b.operatorKey()
	if err != nil {
		return "", err
	}
	data, err := readBounded(p)
	if err != nil {
		return "", fmt.Errorf("no brief: %w; rig agent start writes it", err)
	}
	e, err := agent.Open(pub, data)
	switch {
	case err != nil:
		return fail(err)
	case e.Kind != agent.KindBrief:
		return fail(fmt.Errorf("%w: a %s, not a brief", agent.ErrMalformed, e.Kind))
	case e.Session != b.Session:
		return fail(fmt.Errorf("%w: for session %s, this run is %s", agent.ErrSession, e.Session, b.Session))
	}
	unlock, err := b.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	seen := b.readSeq(agent.BriefSeqName)
	if e.Seq < seen {
		return fail(fmt.Errorf("%w: brief #%d is older than #%d, seen here already", agent.ErrReplay, e.Seq, seen))
	}
	if e.Seq > seen {
		if err := b.writeSeq(agent.BriefSeqName, e.Seq); err != nil {
			return "", err
		}
	}
	return e.Body, nil
}

// MaxAsk bounds a question: it is for the operator to read on a phone.
const MaxAsk = 16 << 10

// Ask records a question for the operator as asks/<id>.json. Asking the same
// thing again under the same id is a no-op, so an agent may retry; a
// different question under a used id is refused.
func (b Box) Ask(id, text string) error {
	text = strings.TrimSpace(text)
	switch {
	case !agent.ValidID(id):
		return fmt.Errorf("ask id %q: use letters, digits, '.', '_' and '-', 64 at most, starting with a letter or digit", id)
	case text == "":
		return errors.New("an ask needs a question")
	case len(text) > MaxAsk:
		return fmt.Errorf("the question is %d bytes; keep it under %d", len(text), MaxAsk)
	}
	p := b.path(agent.AsksName, id+".json")
	if raw, err := os.ReadFile(p); err == nil {
		var prev agent.Ask
		if json.Unmarshal(raw, &prev) == nil && prev.Text == text {
			return nil
		}
		return fmt.Errorf("ask %q exists with a different question; use a new id", id)
	}
	rec, err := json.Marshal(agent.Ask{ID: id, Text: text, Time: agent.Stamp(b.now())})
	if err != nil {
		return err
	}
	return writeAtomic(p, append(rec, '\n'), 0o600)
}

// Answer is the verified answer to ask id, if one has been accepted. A file
// there that does not verify is moved aside like any other.
func (b Box) Answer(id string) (agent.Envelope, bool) {
	if !agent.ValidID(id) {
		return agent.Envelope{}, false
	}
	p := b.path(agent.AnswersName, id+agent.EnvelopeExt)
	data, err := readBounded(p)
	if err != nil {
		return agent.Envelope{}, false
	}
	pub, err := b.operatorKey()
	if err != nil {
		return agent.Envelope{}, false
	}
	e, err := agent.Open(pub, data)
	if err == nil && (e.Kind != agent.KindAnswer || e.ID != id) {
		err = fmt.Errorf("%w: a %s for %q filed as the answer to %q", agent.ErrMalformed, e.Kind, e.ID, id)
	}
	if err != nil {
		b.reject(p, err)
		return agent.Envelope{}, false
	}
	// An earlier session's answer to an id used again is not this one's.
	return e, e.Session == b.Session
}

// MaxWait is the longest rig blocks for an answer. The agent's Bash tool
// gives up on a command after ten minutes, and an answer that arrives after
// it has gone still reaches the agent, through the hook.
const MaxWait = 9 * time.Minute

// Wait delivers what arrives until the answer to ask id is among it, or the
// time is up; answered says which. Everything that arrives meanwhile is
// printed to w, rendered, since this is the path that took it.
func (b Box) Wait(ctx context.Context, id string, timeout, poll time.Duration, w io.Writer) (answered bool, err error) {
	if b.Session == "" {
		return false, ErrNoSession
	}
	deadline := time.Now().Add(min(timeout, MaxWait))
	for {
		items, err := b.Consume(agent.ViaWait, agent.ToCaller)
		if len(items) > 0 {
			fmt.Fprint(w, b.Render(items))
		}
		if err != nil {
			return false, err
		}
		for _, e := range items {
			if e.Kind == agent.KindAnswer && e.ID == id {
				return true, nil
			}
		}
		if e, ok := b.Answer(id); ok {
			fmt.Fprint(w, "(rig delivered this answer earlier; here it is again)\n"+b.Render([]agent.Envelope{e}))
			return true, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(min(poll, left)):
		}
	}
}

// Inbox takes and prints what is queued, for a lead whose brief tells it to
// poll: a harness with no hook has no other mid-turn path. Recorded as
// reaching the caller, since a subagent could run it too.
func (b Box) Inbox(w io.Writer) (int, error) {
	items, err := b.Consume(agent.ViaInbox, agent.ToCaller)
	if len(items) > 0 {
		fmt.Fprint(w, b.Render(items))
	}
	return len(items), err
}

// Done creates DONE, the agent's own statement that the brief is finished,
// with its summary as the first line.
func (b Box) Done(summary string) error {
	summary = strings.Join(strings.Fields(summary), " ")
	if summary == "" {
		summary = "finished"
	}
	return writeAtomic(b.path(agent.DoneName), []byte(summary+"\n"+agent.Stamp(b.now())+"\n"), 0o644)
}
