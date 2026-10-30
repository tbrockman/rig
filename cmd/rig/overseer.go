package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/agent"
	"github.com/tbrockman/rig/internal/manifest"
)

// rig overseer starts a coding agent on this host, trusted as the
// operator's deputy, that sets up and runs the inner agent's VM, watches
// it, intervenes through rig's verbs and reports. Trusted and audited
// rather than sandboxed: it needs every rig verb, including the ones that
// elevate, and a confirm list of the few that are the operator's call;
// everything else it does is on the operator log (audit.go) and in its
// notes.

// notesFile is where the overseer reports, in the project directory.
const notesFile = "OVERSEER.md"

func (a *app) overseerCmd(root *cobra.Command) *cobra.Command {
	var prompt, promptFile, dir, file, harnessName, model string
	var unattended bool
	cmd := &cobra.Command{
		Use:     "overseer (--prompt <mission> | --prompt-file <file>) [-f rig.yaml | --dir <dir>]",
		GroupID: "guest",
		Short:   "Start an overseer: an agent on this host that sets up and runs a VM's agent for you",
		Long: "Launches a coding agent on this host with a prompt that makes it the\n" +
			"operator's deputy: it sets up the VM (or takes over one that exists),\n" +
			"starts the inner agent, keeps it healthy and within budget, answers its\n" +
			"asks, and keeps " + notesFile + " in the project directory for you. Your\n" +
			"--prompt is the mission; it outranks anything written inside the VM.\n\n" +
			"With -f, the manifest's VM is overseen, and the prompt carries its facts\n" +
			"and the agent's state; without it, the overseer makes the project in\n" +
			"--dir (rig init, the image, the VM, the brief) and then runs it.\n\n" +
			"Interactive by default, in this terminal, so the confirm list can be\n" +
			"answered: a forward --to-guest, a host device grant, a credential change,\n" +
			"deleting a VM, a push from outside the project directory. --unattended\n" +
			"runs it as the inner agent runs, with those denied rather than asked.\n" +
			"Every rig verb it runs on the VM is on the operator log:\n" +
			"  rig agent log --operator <vm>\n\n" +
			"rig overseer prompt prints the assembled prompt, for another harness or\n" +
			"by hand.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			s, err := a.overseerSetup(root, prompt, promptFile, dir, file, harnessName, unattended)
			if err != nil {
				return err
			}
			opts := agent.OverseerOpts{
				SystemPrompt: s.system, Mission: s.mission, Model: model, Unattended: unattended,
				ProjectDir: s.projectDir, FilesDir: s.filesDir,
				Allow: overseerAllow(root, s.projectDir), Ask: overseerAsk, Deny: overseerDeny,
			}
			argv, files, ok := s.h.Overseer(opts)
			if !ok {
				return fmt.Errorf("rig cannot launch the %s harness as an overseer; the prompt for one started by hand:\n  rig overseer prompt %s", s.h.Name(), s.promptArgs)
			}
			if err := os.MkdirAll(s.filesDir, 0o700); err != nil {
				return err
			}
			for name, data := range files {
				if err := os.WriteFile(filepath.Join(s.filesDir, name), data, 0o600); err != nil {
					return err
				}
			}
			if s.vm != "" {
				appendAudit(s.vm, "overseer", []string{"--dir=" + s.projectDir, "--unattended=" + fmt.Sprint(unattended)}, nil)
			}
			note("overseer: %s in %s (notes: %s)", s.h.Binary(), s.projectDir, filepath.Join(s.projectDir, notesFile))
			if s.vm != "" {
				note("  its verbs on %s are logged:  rig agent log --operator %s", s.vm, s.vm)
			}
			run := exec.Command(argv[0], argv[1:]...)
			run.Dir = s.projectDir
			run.Stdin, run.Stdout, run.Stderr = os.Stdin, os.Stdout, os.Stderr
			run.Env = append(os.Environ(), actorEnv+"=overseer")
			err = run.Run()
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return &exitCodeError{code: ee.ExitCode(), msg: "the overseer exited " + fmt.Sprint(ee.ExitCode())}
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&prompt, "prompt", "", "the mission, in your words")
	f.StringVar(&promptFile, "prompt-file", "", "the mission, from a file")
	f.StringVar(&dir, "dir", ".", "the project directory to make, when there is no manifest yet")
	f.StringVarP(&file, "file", "f", "", "the manifest of a VM to oversee")
	f.StringVar(&harnessName, "harness", "", "the overseer's harness (default: the manifest's agent, else claude-code)")
	f.StringVar(&model, "model", "", "the overseer's model; empty uses the harness's default")
	f.BoolVar(&unattended, "unattended", false, "run with no terminal: the confirm list is denied with a message, not asked")

	show := &cobra.Command{
		Use:   "prompt (--prompt <mission> | --prompt-file <file>) [-f rig.yaml | --dir <dir>]",
		Short: "Print the overseer's prompt, for another harness or by hand",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			s, err := a.overseerSetup(root, prompt, promptFile, dir, file, harnessName, unattended)
			if err != nil {
				return err
			}
			fmt.Println(s.system)
			return nil
		},
	}
	show.Flags().AddFlagSet(f)
	cmd.AddCommand(show)
	return cmd
}

// overseerSetup is everything the launch and the prompt share.
type overseerSetup struct {
	h          agent.Harness
	mission    string
	system     string
	projectDir string
	filesDir   string
	vm         string
	promptArgs string
}

func (a *app) overseerSetup(root *cobra.Command, prompt, promptFile, dir, file, harnessName string, unattended bool) (*overseerSetup, error) {
	mission, err := missionText(prompt, promptFile)
	if err != nil {
		return nil, err
	}
	s := &overseerSetup{mission: mission}
	data := agent.OverseerData{Mission: mission, Unattended: unattended}
	switch {
	case file != "":
		m, err := manifest.Load(file)
		if err != nil {
			return nil, err
		}
		s.projectDir, s.vm = m.Dir, m.Guest.Name
		if harnessName == "" {
			harnessName = m.AgentHarness()
		}
		data.Facts = a.overseerFacts(m)
		s.promptArgs = "-f " + file
	default:
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return nil, err
		}
		s.projectDir = abs
		s.promptArgs = "--dir " + dir
	}
	if s.h, err = agent.Lookup(harnessName); err != nil {
		return nil, err
	}
	data.ProjectDir = s.projectDir
	data.NotesFile = filepath.Join(s.projectDir, notesFile)
	if s.system, err = agent.RenderOverseer(data); err != nil {
		return nil, err
	}
	state, err := stateDir()
	if err != nil {
		return nil, err
	}
	name := s.vm
	if name == "" {
		name = projectName(s.projectDir)
	}
	s.filesDir = filepath.Join(state, "overseer", name)
	return s, nil
}

// missionText is --prompt or --prompt-file, one of them.
func missionText(prompt, promptFile string) (string, error) {
	switch {
	case prompt != "" && promptFile != "":
		return "", errors.New("give the mission with --prompt or --prompt-file, not both")
	case promptFile != "":
		b, err := os.ReadFile(promptFile)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(b)) == "" {
			return "", fmt.Errorf("%s is empty", promptFile)
		}
		return strings.TrimRight(string(b), "\n"), nil
	case strings.TrimSpace(prompt) == "":
		return "", errors.New("no mission: --prompt <text> or --prompt-file <file>")
	}
	return prompt, nil
}

// overseerFacts is what the manifest and Incus say of the VM, and what is
// still missing for the mission to start.
func (a *app) overseerFacts(m *manifest.Manifest) *agent.OverseerFacts {
	f := &agent.OverseerFacts{ManifestPath: m.Path, VM: m.Guest.Name}
	if spec := m.ResolvedAgent(); !spec.IsZero() {
		f.AgentSpec = describeAgentSpec(spec)
	}
	if len(m.Guest.Forwards) > 0 {
		var ports []string
		for _, p := range m.Guest.Forwards {
			ports = append(ports, fmt.Sprintf("127.0.0.1:%d", p))
		}
		f.Forwards = strings.Join(ports, ", ")
	}
	brief := m.ResolvedAgent().PromptFile
	inst, _, err := a.c.Instance(m.Guest.Name)
	if err == nil {
		f.Exists, f.Running = true, inst.Running()
		f.AgentSession = inst.Config[agent.SessionKey]
		if f.Running && f.AgentSession != "" {
			out, _ := a.exec(m.Guest.Name, "systemctl is-active "+agent.Unit)
			f.AgentActive = strings.TrimSpace(out) == "active"
		}
		// A start given --prompt-file recorded it; that brief is as real as
		// one the manifest names.
		if rec := agent.RecordedOptions(inst.Config).PromptFile; rec != "" {
			brief = rec
		}
	}
	switch {
	case !f.Exists && !a.c.ImageExists(m.ImageAlias()):
		f.Missing = append(f.Missing, "the image and the VM: rig apply -f "+m.Path+" --start builds and creates them")
	case !f.Exists:
		f.Missing = append(f.Missing, "the VM: rig apply -f "+m.Path+" --start")
	case !f.Running:
		f.Missing = append(f.Missing, "the VM is stopped: rig start "+m.Guest.Name)
	}
	if brief != "" {
		if _, err := os.Stat(brief); err != nil {
			f.Missing = append(f.Missing, "the brief at "+brief+", which rig agent start reads")
		}
	} else {
		f.Missing = append(f.Missing, "a brief: guest.agent.prompt_file in the manifest, or --prompt-file to rig agent start")
	}
	if f.AgentSession == "" {
		f.Missing = append(f.Missing, "rig doctor "+m.Guest.Name+" && rig verify "+m.Guest.Name+", then rig agent start "+m.Guest.Name)
	}
	return f
}

// The confirm list and the denials, as Claude Code's rules take them; a
// harness with another syntax translates. Ask rules prompt even where a
// narrower allow matches, so these win over the allow list.
var (
	overseerAsk = []string{
		"Bash(rig rm *)", "Bash(rig delete *)", "Bash(rig creds *)",
		"Bash(rig forward --to-guest *)", "Bash(rig forward * --to-guest)", "Bash(rig forward * --to-guest *)",
		"Bash(rig claim *)", "Bash(rig release *)", "Bash(rig host *)",
		"Bash(rig new --gpu *)", "Bash(rig new * --gpu)", "Bash(rig new * --gpu *)",
		"Bash(rig start * --allow-unisolated*)", "Bash(rig start * --kill-holders*)", "Bash(rig apply * --allow-unisolated*)",
	}
	overseerDeny = []string{"Bash(incus *)", "Bash(git push *)", "Bash(sudo *)"}
	// Plain commands an overseer needs around rig: files, git for its notes
	// and bundles, and waiting.
	overseerPlain = []string{"git *", "mkdir *", "ls *", "cat *", "head *", "tail *", "grep *", "sleep *", "jq *",
		"date *", "wc *", "diff *", "cp *", "mv *", "sed *", "awk *", "find *", "df *", "free *", "nproc", "uptime", "python3 -I *"}
)

// overseerAllow is every rig verb, from the command tree so a new verb is
// covered, except push, which is allowed only from the project directory,
// and overseer itself. The confirm list is asked over these by precedence.
func overseerAllow(root *cobra.Command, projectDir string) []string {
	var allow []string
	for _, c := range root.Commands() {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" || c.Name() == "overseer" || c.Name() == "push" {
			continue
		}
		if subs := c.Commands(); len(subs) > 0 {
			for _, sub := range subs {
				if !sub.Hidden {
					allow = append(allow, fmt.Sprintf("Bash(rig %s %s *)", c.Name(), sub.Name()))
				}
			}
			continue
		}
		allow = append(allow, fmt.Sprintf("Bash(rig %s *)", c.Name()))
	}
	allow = append(allow, "Bash(rig push * "+projectDir+"/*)", "Bash(rig push * "+projectDir+")")
	for _, p := range overseerPlain {
		allow = append(allow, "Bash("+p+")")
	}
	return allow
}
