package agent

import (
	"os"
	"time"
)

// The channel between operator and agent, as files under Dir. Names are
// relative, because the guest build of rig works under RIG_AGENT_DIR when it
// is set (as the runner does), which is how both are exercised in a temp dir.
//
//	bin/rig            the guest build of rig: rig ask, wait, done, and the hooks
//	operator.pub       the host's public key for this VM; the private half never leaves the host
//	inbox.d/           signed envelopes from the host, one per file, renamed into place
//	claimed/           an envelope a consumer has taken and not yet settled
//	delivered/         accepted messages, after delivery
//	answers/<id>.msg   the accepted answer to ask <id>
//	rejected/          what failed verification, moved aside
//	asks/<id>.json     the agent's questions (Ask)
//	inbox.seq          the last sequence number accepted
//	deliveries.jsonl   one Delivery per item the agent was shown
//	last_launch        when the runner last launched the agent, so the next launch can list what was delivered mid-turn since
//	hook.unproven      left by a hook whose input could not say which process it ran in; removed at launch
//	tamper.jsonl       one Tamper per item refused
//	launch.token       this launch's token, which every delivery carries
//	brief.msg          the brief, sealed like an envelope (KindBrief); the runner refuses to launch without a good one
//	brief.seq          the newest brief accepted, so an older one copied back is refused
//	inbox              the unsigned inbox of an older rig; reported, never delivered
const (
	BinName         = "bin/rig"
	PubName         = "operator.pub"
	InboxName       = "inbox.d"
	ClaimedName     = "claimed"
	DeliveredName   = "delivered"
	AnswersName     = "answers"
	RejectedName    = "rejected"
	AsksName        = "asks"
	SeqName         = "inbox.seq"
	LockName        = "inbox.lock"
	DeliveriesName  = "deliveries.jsonl"
	TamperName      = "tamper.jsonl"
	TokenName       = "launch.token"
	LastLaunchName  = "last_launch"
	UnprovenName    = "hook.unproven"
	LegacyInboxName = "inbox"
	RunnerName      = "run"
	DoneName        = "DONE"
	BudgetName      = "BUDGET"   // a budget fired: its reason on the first line; the unit is not restarted, the session is kept
	WatchdogName    = "watchdog" // a run's limit fired: its reason, read and removed by the next launch, which says so in its note
	BriefName       = "brief.msg"
	BriefSeqName    = "brief.seq"
)

// Paths in the guest, for the host side.
const (
	GuestBinDir     = Dir + "/bin"
	GuestBinPath    = Dir + "/" + BinName
	OperatorPubPath = Dir + "/" + PubName
	InboxDir        = Dir + "/" + InboxName
	AsksDir         = Dir + "/" + AsksName
	LegacyInboxPath = Dir + "/" + LegacyInboxName
	BriefPath       = Dir + "/" + BriefName
)

// EnvelopeExt names a sealed envelope in inbox.d, delivered/ and answers/.
const EnvelopeExt = ".msg"

// The delivery paths a Delivery records.
const (
	ViaLaunch = "launch" // spliced into the prompt by the runner
	ViaHook   = "hook"   // added after a tool call by the PostToolUse hook
	ViaWait   = "wait"   // printed by rig ask --wait or rig wait
	ViaInbox  = "inbox"  // printed by rig inbox, which the brief may tell the lead to run
)

// Whom a delivery reached. The lead is the agent's own process, the one that
// reads the brief and answers the operator; its subagents are not it. A hook
// proves which it ran in from its input (guest.LeadProof); the launch prompt
// is the lead's by construction; a guest verb is run by whichever process
// typed it, and nothing in the guest can tell which (DEV_NOTES).
const (
	ToLead   = "lead"
	ToCaller = "caller"
)

// Ask is a question from the agent, written by `rig ask`.
type Ask struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Time string `json:"time"` // RFC 3339, the guest's clock
}

// Delivery is one line of deliveries.jsonl: an item the agent was shown. The
// stream-json log cannot say this — hook context does not appear in it.
type Delivery struct {
	Time string `json:"time"`
	Seq  uint64 `json:"seq"`
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Path string `json:"path"`         // ViaLaunch, ViaHook, ViaWait or ViaInbox
	To   string `json:"to,omitempty"` // ToLead or ToCaller; empty in a record from an older rig
}

// Tamper is one line of tamper.jsonl: an item refused, and why.
type Tamper struct {
	Time   string `json:"time"`
	File   string `json:"file"` // where it was moved aside to, relative to Dir
	Reason string `json:"reason"`
}

// Installed is what `rig agent start` writes that decides what the agent is
// told is the operator: the runner that wires the hook, the verifier, and the
// key it verifies with. Each one's sha256 is recorded on the instance under
// Key, and `rig agent status` compares the guest's files against it.
var Installed = []struct{ Name, Key string }{
	{RunnerName, "user.rig.agent.sha256.run"},
	{BinName, "user.rig.agent.sha256.bin"},
	{PubName, "user.rig.agent.sha256.pub"},
}

// DirFromEnv is where the guest side works: RIG_AGENT_DIR, else Dir.
func DirFromEnv() string {
	if d := os.Getenv("RIG_AGENT_DIR"); d != "" {
		return d
	}
	return Dir
}

// Stamp is how every record here writes a time.
func Stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
