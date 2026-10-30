package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/agent"
	"github.com/tbrockman/rig/internal/agent/claudecode"
)

// The allow list is every rig verb from the command tree, so a new verb is
// covered without anyone remembering; push only from the project directory;
// never overseer itself or raw incus. The confirm list is separate, and
// wins by the harness's precedence.
func TestOverseerAllowComesFromTheCommandTree(t *testing.T) {
	a := &app{}
	root := &cobra.Command{Use: "rig"}
	root.AddCommand(a.startCmd(), a.pushCmd(), a.agentCmd(), a.forwardCmd(), a.rmCmd())
	root.AddCommand(a.overseerCmd(root))
	allow := strings.Join(overseerAllow(root, "/home/me/proj"), "\n")
	for _, want := range []string{"Bash(rig start *)", "Bash(rig agent send *)", "Bash(rig agent status *)", "Bash(rig forward *)",
		"Bash(rig rm *)", "Bash(rig push * /home/me/proj/*)", "Bash(git *)"} {
		if !strings.Contains(allow, want) {
			t.Errorf("allow lacks %s:\n%s", want, allow)
		}
	}
	for _, never := range []string{"Bash(rig push *)\n", "Bash(rig overseer", "Bash(rig agent *)", "incus"} {
		if strings.Contains(allow, never) {
			t.Errorf("allow must not hold %q:\n%s", never, allow)
		}
	}
	ask := strings.Join(overseerAsk, "\n")
	for _, want := range []string{"rig rm *", "rig delete *", "rig creds *", "--to-guest", "rig host *", "rig claim *"} {
		if !strings.Contains(ask, want) {
			t.Errorf("the confirm list lacks %s", want)
		}
	}
	if deny := strings.Join(overseerDeny, "\n"); !strings.Contains(deny, "incus *") || !strings.Contains(deny, "git push *") {
		t.Errorf("deny: %s", deny)
	}
}

// Claude Code is launched with the settings file, the system prompt, and
// the mission after --; unattended adds -p with prompts auto-denied.
func TestClaudeCodeOverseerLaunch(t *testing.T) {
	var h claudecode.Harness
	o := agent.OverseerOpts{SystemPrompt: "you are the overseer", Mission: "--- build it", Model: "opus",
		FilesDir: "/home/me/.local/state/rig/overseer/proj", Allow: []string{"Bash(rig start *)"}, Ask: overseerAsk, Deny: overseerDeny}
	argv, files, ok := h.Overseer(o)
	if !ok {
		t.Fatal("claude-code is launchable")
	}
	got := strings.Join(argv, " ")
	for _, want := range []string{"claude --settings /home/me/.local/state/rig/overseer/proj/overseer-settings.json",
		"--append-system-prompt you are the overseer", "--model opus", "-- --- build it"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, " -p ") || strings.Contains(got, "dontAsk") {
		t.Error("interactive by default")
	}
	var settings struct {
		Permissions struct{ Allow, Ask, Deny []string } `json:"permissions"`
	}
	if err := json.Unmarshal(files["overseer-settings.json"], &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.Permissions.Allow) != 1 || len(settings.Permissions.Ask) != len(overseerAsk) || len(settings.Permissions.Deny) != len(overseerDeny) {
		t.Errorf("settings: %+v", settings)
	}
	o.Unattended = true
	argv, _, _ = h.Overseer(o)
	if got := strings.Join(argv, " "); !strings.Contains(got, " -p --permission-mode dontAsk -- ") {
		t.Errorf("unattended must be -p with prompts denied: %s", got)
	}
}

// The prompt differs by state: a playbook with no manifest, the facts and
// what is missing with one; the mission is in it verbatim.
func TestOverseerPromptCarriesTheStateAndTheMission(t *testing.T) {
	fresh, err := agent.RenderOverseer(agent.OverseerData{Mission: "Port the parser to Rust.", ProjectDir: "/home/me/proj", NotesFile: "/home/me/proj/OVERSEER.md"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Port the parser to Rust.", "rig init /home/me/proj", "rig apply -f rig.yaml --start", "rig verify",
		"OVERSEER.md", "You will be asked before each", "rig agent log --operator", "Never run `incus`"} {
		if !strings.Contains(fresh, want) {
			t.Errorf("the fresh prompt lacks %q", want)
		}
	}
	existing, err := agent.RenderOverseer(agent.OverseerData{Mission: "Oversee.", ProjectDir: "/home/me/proj", NotesFile: "x", Unattended: true,
		Facts: &agent.OverseerFacts{ManifestPath: "/home/me/proj/rig.yaml", VM: "proj", Exists: true, Running: true,
			AgentSession: "sess", AgentActive: true, AgentSpec: "model fable; budgets: 24h", Forwards: "127.0.0.1:5173",
			Missing: []string{"the brief at /home/me/proj/brief.md"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"guest.name: proj", "It exists and is running", "which is running", "model fable; budgets: 24h",
		"127.0.0.1:5173", "- the brief at /home/me/proj/brief.md", "These are denied for you"} {
		if !strings.Contains(existing, want) {
			t.Errorf("the prompt for an existing VM lacks %q", want)
		}
	}
	if strings.Contains(existing, "1. `rig init") {
		t.Error("an existing VM gets no setup playbook")
	}
}

func TestMissionTextIsOneOfPromptOrFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "m.md")
	os.WriteFile(f, []byte("Do it.\n"), 0o644)
	if got, err := missionText("", f); err != nil || got != "Do it." {
		t.Errorf("file: %q %v", got, err)
	}
	if got, err := missionText("words", ""); err != nil || got != "words" {
		t.Errorf("prompt: %q %v", got, err)
	}
	for _, c := range [][2]string{{"", ""}, {"x", f}, {"  ", ""}} {
		if _, err := missionText(c[0], c[1]); err == nil {
			t.Errorf("%q, %q must be refused", c[0], c[1])
		}
	}
}

// One line per verb: when, who, what, with what, and how it went. The
// wrapper records after the verb ran, whatever its outcome, under the VM
// the verb named.
func TestAuditRecordsEachVerbOnItsVM(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(actorEnv, "overseer")
	root := &cobra.Command{Use: "rig"}
	var ran []string
	verb := func(name string, fail bool) *cobra.Command {
		c := &cobra.Command{Use: name + " <vm>", Args: cobra.MinimumNArgs(1), RunE: func(_ *cobra.Command, args []string) error {
			ran = append(ran, name)
			if fail {
				return os.ErrPermission
			}
			return nil
		}}
		c.Flags().String("file", "", "")
		return c
	}
	agentCmd := &cobra.Command{Use: "agent"}
	agentCmd.AddCommand(verb("send", false))
	root.AddCommand(verb("start", false), verb("stop", true), verb("status", false), agentCmd)
	auditVerbs(root)

	for _, args := range [][]string{{"start", "myvm", "--file", "x"}, {"stop", "myvm"}, {"status", "myvm"}, {"agent", "send", "myvm", "hello", "there"}} {
		root.SetArgs(args)
		_ = root.Execute()
	}
	path, _ := auditPath("myvm")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 || len(ran) != 4 {
		t.Fatalf("got %d lines for 4 verbs (status is not audited):\n%s", len(lines), raw)
	}
	for i, want := range []string{" overseer start --file=x myvm -> ok", " overseer stop myvm -> error: permission denied", " overseer agent send myvm hello there -> ok"} {
		if !strings.HasSuffix(lines[i], want) || !strings.HasPrefix(lines[i], "20") {
			t.Errorf("line %d: %q, want a time then %q", i, lines[i], want)
		}
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil || !strings.HasSuffix(filepath.Dir(path), "/rig/audit") {
		t.Errorf("the log lives under the state directory, got %s", path)
	}
}
