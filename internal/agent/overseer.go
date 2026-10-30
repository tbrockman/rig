package agent

import (
	"bytes"
	_ "embed"
	"text/template"
)

// An overseer is a coding agent on the host, trusted as the operator's
// deputy, that sets up and runs the inner agent's VM, watches it,
// intervenes through rig's verbs and reports. rig starts one with a prompt
// it assembles (overseer.md) and, for a harness that can be launched from
// here (Harness.Overseer), the settings that make it run without permission
// friction: every rig verb allowed, the confirm list asked, raw incus and
// git push denied.

//go:embed overseer.md
var overseerTemplate string

// OverseerData fills the prompt.
type OverseerData struct {
	Mission    string // the operator's prompt, verbatim
	ProjectDir string // where the manifest and the notes live
	NotesFile  string // the overseer's notes, for the operator
	Unattended bool
	Facts      *OverseerFacts // nil when there is no manifest yet
}

// OverseerFacts is what an existing manifest and its VM say.
type OverseerFacts struct {
	ManifestPath string
	VM           string
	Exists       bool
	Running      bool
	AgentSession string
	AgentActive  bool
	AgentSpec    string   // guest.agent's long form, described
	Forwards     string   // guest.forwards, described
	Missing      []string // the playbook for what is not there yet
}

// RenderOverseer is the system prompt for an overseer.
func RenderOverseer(d OverseerData) (string, error) {
	t, err := template.New("overseer").Parse(overseerTemplate)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}

// OverseerOpts is what a harness needs to launch an overseer on the host.
type OverseerOpts struct {
	SystemPrompt string
	Mission      string
	Model        string
	Unattended   bool
	ProjectDir   string   // the working directory; pushes from elsewhere are the operator's
	FilesDir     string   // where the files the harness returns will be written
	Allow        []string // rig verbs (and a few plain commands) as the harness's own allow patterns take them
	Ask          []string // the confirm list, likewise
	Deny         []string // likewise
}
