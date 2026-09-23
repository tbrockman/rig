// Command rig creates and runs isolated project VMs with the GPU attached.
//
// The verbs that can break an invariant — release, forced detach, apply,
// starting a VM with no network isolation — are grouped separately in help but
// live here too. They were a second binary once, which only ever encoded that
// distinction more loudly than it needed to be.
//
// rig restricts nothing by itself: incus is still on PATH. It makes the safe
// path the easy one. The boundary that matters is the VM and the network ACL.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/cpuset"
	"github.com/tbrockman/rig/internal/creds"
	"github.com/tbrockman/rig/internal/devices"
	"github.com/tbrockman/rig/internal/hostdev"
	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
	"github.com/tbrockman/rig/internal/policy"
	"github.com/tbrockman/rig/internal/ports"
	"github.com/tbrockman/rig/internal/pushguard"
	"github.com/tbrockman/rig/internal/volumes"
)

// managedKey marks instances rig created. rm refuses anything without it, so it
// cannot delete something built by hand.
const managedKey = "user.rig.managed"

// defaultImage is the alias `rig image build` writes and `rig new` reads.
const defaultImage = "nixos-gpu-base"

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,61}$`)

type app struct {
	c   *incus.Client
	cfg devices.Config
}

func main() {
	a := &app{c: incus.New(""), cfg: devices.ConfigFromEnv()}

	root := &cobra.Command{
		Use:   "rig",
		Short: "Isolated project VMs with the GPU attached",
		Long: "rig creates and runs isolated project VMs with the GPU attached, and runs\n" +
			"an unattended coding agent inside one.\n\n" +
			"It enforces two invariants: a host device passed through to a VM — the\n" +
			"card, a USB controller — is configured on at most one instance and never\n" +
			"moves away from a running one, and no instance starts without network\n" +
			"isolation.\n\n" +
			"Typical flow, once the host is set up (docs/RUNBOOK.md):\n" +
			"  rig image build                        # the NixOS guest image, from base/\n" +
			"  rig apply                              # the isolation ACL, onto the profile\n" +
			"  rig new myproj --env ~/.config/rig/myproj.env --start\n" +
			"  rig new -f rig.yaml --start            # or everything from a manifest\n" +
			"  rig doctor myproj && rig verify myproj # configured, then proven\n" +
			"  rig push myproj ./project              # -> /work/project in the guest\n" +
			"  rig agent start myproj --prompt-file brief.md --until-done\n" +
			"  rig agent status myproj                # bounded and cheap; check often\n\n" +
			"Exit status is 0 or 1 except where a verb says otherwise: verify exits 2\n" +
			"for \"could not be proven\", and exec carries the guest command's status out.\n" +
			"Progress notes go to stdout prefixed \"rig:\"; errors go to stderr.\n\n" +
			"Environment (each has a flag or a default; none is required):\n" +
			"  RIG_PCI           the card's PCI address, when discovery picks wrong\n" +
			"  RIG_DEVICE        name of the GPU device rig puts on an instance (gpu0)\n" +
			"  RIG_ACL           name of the isolation ACL (vm-isolate)\n" +
			"  RIG_PROFILE       profile that carries the isolation and that new VMs use (default)\n" +
			"  RIG_LOCK          lock file serialising card moves (/var/lock/rig.lock)\n" +
			"  RIG_IMAGE         image alias new VMs are made from (nixos-gpu-base)\n" +
			"  RIG_CPUS, RIG_MEMORY, RIG_DISK    defaults for rig new\n" +
			"  RIG_FLAKE, RIG_FLAKE_ATTR         what rig image build builds\n" +
			"  INCUS_SOCKET      the daemon's unix socket (/var/lib/incus/unix.socket)",
		Version:       version(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Grouped so the sharp verbs stay visibly separate in help. They used to be
	// a second binary; that only ever encoded this distinction.
	root.AddGroup(
		&cobra.Group{ID: "vm", Title: "Project VMs:"},
		&cobra.Group{ID: "guest", Title: "Working inside a guest:"},
		&cobra.Group{ID: "card", Title: "The card and the policy:"},
	)
	root.AddCommand(
		a.initCmd(),
		a.newCmd(), a.startCmd(), a.stopCmd(), a.restartCmd(), a.rmCmd(),
		a.statusCmd(), a.doctorCmd(), a.verifyCmd(), a.logsCmd(),
		a.execCmd(), a.shellCmd(), a.pushCmd(), a.pullCmd(), a.agentCmd(),
		a.credsCmd(),
		a.mountCmd(), a.unmountCmd(), a.forwardCmd(),
		a.claimCmd(), a.releaseCmd(), a.applyCmd(), a.hostCmd(), a.imageCmd(),
	)

	if err := root.Execute(); err != nil {
		var exit *incus.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code) // propagate the guest command's status
		}
		var coded *exitCodeError
		if errors.As(err, &coded) {
			os.Exit(coded.code) // the command already reported the detail
		}
		fmt.Fprintf(os.Stderr, "rig: %v\n", err)
		os.Exit(1)
	}
}

// build is what the toolchain recorded about this binary: its module path, the
// module version (a tag for a release `go install`, a pseudo-version or
// "(devel)" otherwise), and the commit a `make` ran at.
type build struct {
	module, version, rev string
	dirty                bool
}

func readBuild() build {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return build{version: "unknown"}
	}
	b := build{module: info.Main.Path, version: info.Main.Version}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.rev = s.Value
		case "vcs.modified":
			b.dirty = s.Value == "true"
		}
	}
	if len(b.rev) > 12 {
		b.rev = b.rev[:12]
	}
	return b
}

// version renders the build for --version: the tag when there is one, and the
// commit, marked when the tree was dirty.
func version() string {
	b := readBuild()
	dirty := ""
	if b.dirty {
		dirty = "-dirty"
	}
	switch {
	case b.rev == "":
		return b.version
	case b.version == "" || b.version == "(devel)":
		return b.rev + dirty
	}
	return b.version + " (" + b.rev + dirty + ")"
}

// exitCodeError carries a specific process exit status out of a command whose
// result is more than pass/fail. Its message is not reprinted: the command has
// already said more than one line could.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

func note(format string, args ...any) { fmt.Printf("rig: "+format+"\n", args...) }

func (a *app) requireInstance(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("not a valid instance name: %s", name)
	}
	if !a.c.Exists(name) {
		return fmt.Errorf("no such instance: %s", name)
	}
	return nil
}

// --- lifecycle -----------------------------------------------------------

// imageKey records which image alias this VM was created from.
//
// Without it, `rig doctor` compares every instance against the default alias,
// which is only right while there is one image on the host. A project with its
// own guest image would be reported as drifted from a base it was never built
// from — a check naming one thing and measuring another, which is the failure
// this project keeps finding. Absent means "compare against the default", so
// instances created before this existed behave as they did.
const imageKey = "user.rig.image"

// absEnvFile resolves and validates a --env argument. Empty in, empty out: not
// passing --env is not an error anywhere, it just means "leave it alone".
func absEnvFile(envFile string) (string, error) {
	if envFile == "" {
		return "", nil
	}
	abs, err := filepath.Abs(manifest.ExpandHome(envFile))
	if err != nil {
		return "", err
	}
	if err := creds.Validate(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// setEnvFile points an existing instance at a credential file.
//
// It records the *path*, never the secret — same as `rig new --env`, and the
// same key, so where credentials come from does not depend on which verb
// attached them. The file is validated before it is recorded: storing a path
// that `start` will later refuse turns a clear error into a confusing one two
// commands later.
func (a *app) setEnvFile(name, envFile string) error {
	abs, err := absEnvFile(envFile)
	if err != nil || abs == "" {
		return err
	}
	if err := a.c.SetConfigKey(name, creds.InstanceKey, abs); err != nil {
		return err
	}
	note("credentials for %s now come from %s", name, abs)
	return nil
}

func (a *app) newCmd() *cobra.Command {
	var (
		envFile, image, memory, disk, profile, file string
		cpus                                        int
		start, noGPU                                bool
	)
	cmd := &cobra.Command{
		Use:     "new <vm> | new -f rig.yaml",
		GroupID: "vm",
		Short:   "Create a project VM (isolated, GPU-ready)",
		Long: "Creates a stopped VM from the base image. Its NIC — and so its network\n" +
			"isolation — and its root disk come from the profile, which is the one\n" +
			"`rig apply` reconciles. The image alias, the credential file and which\n" +
			"host devices the VM wants are all recorded on the instance, so later\n" +
			"verbs need none of them repeated.\n\n" +
			"With -f, everything comes from the manifest: the name, the image (built\n" +
			"from its `build:` directory when the alias is missing), the size, the\n" +
			"credential file, the devices and the published ports. `rig init` writes\n" +
			"one to start from.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if file != "" {
				for _, f := range []string{"env", "image", "cpus", "memory", "disk", "no-gpu"} {
					if cmd.Flags().Changed(f) {
						return fmt.Errorf("--%s and -f together: with a manifest, that comes from the file", f)
					}
				}
				m, err := manifest.Load(file)
				if err != nil {
					return err
				}
				if len(args) == 1 && args[0] != m.Guest.Name {
					return fmt.Errorf("%s names the VM %q, not %q", file, m.Guest.Name, args[0])
				}
				if err := a.newFromManifest(m, profile); err != nil {
					return err
				}
				return a.afterNew(m.Guest.Name, profile, start)
			}
			if len(args) != 1 {
				return errors.New("name the VM, or pass -f rig.yaml")
			}
			name := args[0]
			if !nameRE.MatchString(name) {
				return fmt.Errorf("not a valid instance name: %s", name)
			}
			if a.c.Exists(name) {
				return fmt.Errorf("%s already exists", name)
			}
			if !a.c.ImageExists(image) {
				return fmt.Errorf("no such image: %s\n  Build it:  rig image build", image)
			}

			absEnv, err := absEnvFile(envFile)
			if err != nil {
				return err
			}

			config := map[string]string{managedKey: "true", imageKey: image}
			if noGPU {
				config[devices.DevicesKey] = devices.Encode(nil)
			}
			if absEnv != "" {
				config[creds.InstanceKey] = absEnv
			}
			if err := a.c.CreateVM(incus.CreateOpts{
				Name: name, Image: image, Profile: profile, CPUs: cpus, Memory: memory,
				DiskSize: disk, Config: config,
			}); err != nil {
				return err
			}

			gpuNote := ""
			if noGPU {
				gpuNote = ", no GPU"
			}
			note("created %s (image %s, %d cpus, %s, %s disk%s)", name, image, cpus, memory, disk, gpuNote)
			if absEnv != "" {
				note("credentials will be injected from %s", absEnv)
			}
			return a.afterNew(name, profile, start)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&file, "file", "f", "", "manifest to create the VM from (rig.yaml)")
	f.StringVar(&envFile, "env", "", "host file of KEY=VALUE credentials to inject on start")
	f.StringVar(&image, "image", envOr("RIG_IMAGE", defaultImage), "base image alias")
	f.StringVar(&profile, "profile", envOr("RIG_PROFILE", "default"),
		"Incus profile the VM inherits its NIC and root disk from; rig apply isolates the same one")
	f.IntVar(&cpus, "cpus", envInt("RIG_CPUS", 8), "vCPUs")
	f.StringVar(&memory, "memory", envOr("RIG_MEMORY", "16GiB"), "RAM")
	f.StringVar(&disk, "disk", envOr("RIG_DISK", "40GiB"), "root disk size")
	f.BoolVar(&start, "start", false, "start it once created")
	f.BoolVar(&noGPU, "no-gpu", false,
		"never claim the GPU for this VM — for CPU-only work, and so starting it cannot take the card from this host's desktop")
	return cmd
}

// afterNew is what every creation ends with: the isolation check, and the
// start when asked for.
func (a *app) afterNew(name, profile string, start bool) error {
	// Isolation is inherited from the default profile. Verify it landed
	// rather than assuming: a new VM with no ACL is the failure this
	// project exists to prevent, and it is silent.
	inst, _, err := a.c.Instance(name)
	if err != nil {
		return err
	}
	if !policy.Isolated(inst, a.cfg.ACL) {
		fix := "rig apply"
		if profile != "default" {
			fix += " --profile " + profile
		}
		note("WARNING: %s did NOT inherit network isolation.", name)
		note("         Fix the profile:  %s", fix)
		note("         rig start will refuse it until then.")
	}
	wanted, err := devices.Wanted(inst, a.cfg)
	if err != nil {
		return err
	}
	if len(wanted) > 0 {
		var names []string
		for _, d := range wanted {
			names = append(names, d.Name)
		}
		note("created %s; it will claim %s on start", name, strings.Join(names, ", "))
	} else {
		note("created %s (no host devices)", name)
	}
	if start {
		return a.startInstance(name, 3*time.Minute, true)
	}
	note("next:  rig start %s", name)
	return nil
}

func (a *app) startCmd() *cobra.Command {
	var timeout time.Duration
	var envFile string
	var noWait, allowUnisolated bool
	cmd := &cobra.Command{
		Use:     "start <vm>",
		Short:   "Claim the card, start the VM, inject credentials",
		GroupID: "vm",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			if err := a.setEnvFile(args[0], envFile); err != nil {
				return err
			}
			return a.start(args[0], timeout, !noWait, allowUnisolated)
		},
	}
	cmd.Flags().StringVar(&envFile, "env", "",
		"host file of KEY=VALUE credentials; replaces this VM's for good, not just this start")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Minute, "how long to wait for the guest to come up")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return as soon as Incus reports it started")
	cmd.Flags().BoolVar(&allowUnisolated, "allow-unisolated", false,
		"start even with no isolation ACL — the guest will reach this host and the LAN")
	return cmd
}

func (a *app) startInstance(name string, timeout time.Duration, wait bool) error {
	return a.start(name, timeout, wait, false)
}

func (a *app) start(name string, timeout time.Duration, wait, allowUnisolated bool) error {
	inst, _, err := a.c.Instance(name)
	if err != nil {
		return err
	}
	wanted, err := devices.Wanted(inst, a.cfg)
	if err != nil {
		return err
	}
	if len(wanted) == 0 {
		note("%s wants no host devices; leaving the card where it is", name)
	}
	// A pinned CPU set is checked against this host before anything moves:
	// it must name CPUs that exist and leave the host a core of its own.
	if limit := inst.Config["limits.cpu"]; cpuset.IsSet(limit) {
		warnings, err := cpuset.Check(limit)
		if err != nil {
			return fmt.Errorf("limits.cpu: %w\nNothing was claimed; %s was not started.", err, name)
		}
		for _, w := range warnings {
			note("WARNING: limits.cpu: %s", w)
		}
	}
	// A device the host is using has to be given up first: a display manager
	// on the card, most often. Done here, before the claim, so a VM that
	// starts on a host with a desktop on the card takes it deliberately.
	if err := freeWanted(wanted); err != nil {
		return err
	}
	if err := devices.Start(a.c, a.cfg, name, allowUnisolated, int(timeout.Seconds())); err != nil {
		return err
	}
	if !wait {
		a.startInput(name, wait)
		return nil
	}
	if err := a.c.WaitAgent(name, timeout); err != nil {
		note("WARNING: %v", err)
		note("         Look at:  rig logs %s", name)
		return err
	}
	// Wait for an address too: the agent answers several seconds before DHCP
	// finishes, and anything touching the network in that window fails oddly.
	// A VM with no network device never gets one, and waiting out the whole
	// timeout for it only delays the credential injection below.
	if len(inst.NICs()) == 0 {
		note("%s is up; it has no network device, so there is no address to wait for", name)
	} else if addr, err := a.c.WaitAddress(name, timeout); err != nil {
		note("WARNING: %v", err)
	} else {
		note("up at %s", addr)
	}
	a.startInput(name, wait)

	// Re-read: the instance changed when it started.
	inst, _, err = a.c.Instance(name)
	if err != nil {
		return err
	}
	envFile := inst.Config[creds.InstanceKey]
	if envFile == "" {
		return nil
	}
	if _, statErr := os.Stat(envFile); statErr != nil {
		note("WARNING: %s expects credentials at %s, which is missing.", name, envFile)
		note("         The guest started without them.")
		return nil
	}
	n, err := creds.Inject(a.c, name, envFile)
	if err != nil {
		return err
	}
	note("injected %d credential(s) into %s (tmpfs; gone on stop)", n, creds.GuestPath)
	return nil
}

// credsCmd re-injects credentials into a running VM.
//
// The gap this fills: /run/rig/env is written on start, and until now the only
// way to change it was `rig start --env` or `rig restart --env`, both of which
// cycle the VM. That is a heavy instrument for the failure this project hits
// most — an OAuth snapshot going stale because something on the host refreshed
// the session and rotated the refresh token. The credential is wrong, nothing
// else is, and restarting the VM to fix it kills whatever the guest was doing.
//
// An unattended agent makes that cost concrete. It survives its own crashes by
// design — the session UUID is fixed, so systemd restarts it and the
// conversation resumes — but a VM restart takes the whole machine out from
// under it mid-edit. Re-injecting leaves the running process alone: its next
// restart sources the new file and authenticates, and if it is still working
// on an unexpired token it never notices.
//
// Deliberately does not restart the agent. Re-injecting a credential and
// deciding a running agent should be interrupted are two different judgements,
// and this verb only makes the first.
//
// The credential file is a required argument rather than an optional flag over
// a remembered path. Injecting a secret is not the place to guess: the first
// version defaulted to whatever `user.rig.env` happened to hold, so
// `rig creds <vm>` named neither the file it read nor the directory it read it
// from, and would cheerfully report success for a path recorded weeks earlier
// by someone else. Naming the file costs one word and makes the command say
// what it did.
func (a *app) credsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "creds <vm> <env-file>",
		GroupID: "guest",
		Short:   "Re-inject credentials into a running VM, without restarting it",
		Long: "Writes <env-file> to " + creds.GuestPath + " in a running guest, and\n" +
			"records the path for later starts.\n\n" +
			"For a credential that went stale under a VM that is otherwise fine —\n" +
			"an OAuth snapshot invalidated by a refresh on the host, most often.\n\n" +
			"The file is named explicitly, never inferred from what the instance\n" +
			"already had recorded: injecting a secret is not a place to guess. A\n" +
			"relative path resolves against your current directory, and the\n" +
			"absolute path is what gets recorded and printed.\n\n" +
			"A running agent is left alone: it picks the new credential up when it\n" +
			"next restarts. Nothing here reads or logs the secret itself.",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			name, envFile := args[0], args[1]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			// Resolve and validate before touching the instance, so a typo in
			// the path is one clear error rather than a recorded value that
			// the next `rig start` will choke on.
			abs, err := absEnvFile(envFile)
			if err != nil {
				return err
			}
			inst, _, err := a.c.Instance(name)
			if err != nil {
				return err
			}
			// Injecting into a stopped guest would write to a tmpfs that is
			// about to be discarded, and report success for a credential that
			// will not exist a moment later.
			if inst.Status != "Running" {
				return fmt.Errorf("%s is %s; credentials live on tmpfs and only exist while it runs.\n"+
					"  rig start %s --env %s", name, strings.ToLower(inst.Status), name, envFile)
			}
			if err := a.setEnvFile(name, envFile); err != nil {
				return err
			}
			n, err := creds.Inject(a.c, name, abs)
			if err != nil {
				return err
			}
			note("injected %d credential(s) from %s into %s (tmpfs; gone on stop)",
				n, abs, creds.GuestPath)
			note("a running agent keeps its current process; it authenticates fresh on its next restart")
			return nil
		},
	}
	return cmd
}

func (a *app) stopCmd() *cobra.Command {
	var timeout time.Duration
	var keep, force bool
	cmd := &cobra.Command{
		Use:     "stop <vm>",
		GroupID: "vm",
		Short:   "Stop the VM and hand its devices back to the host",
		Long: "Stops the VM, detaches every host device it held, and returns each one\n" +
			"that has a recipe recorded — from the manifest's `return:` block — to\n" +
			"the host's own driver. That last step writes to sysfs, so it runs under\n" +
			"sudo. A device with no recipe stays parked on vfio-pci, which is right\n" +
			"for hardware the host never uses itself.\n\n" +
			"The guest is asked to shut down first. If it is still running when\n" +
			"--timeout expires — a desktop session swallows the power button — the\n" +
			"plug is pulled, because a VM left running with the host's devices\n" +
			"inside it is worse than lost guest state. --force skips the asking.\n\n" +
			"--keep-devices leaves everything attached, as stop always did before.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			return a.stopForce(args[0], timeout, keep, force)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "how long to allow for shutdown")
	cmd.Flags().BoolVar(&keep, "keep-devices", false, "leave the VM's devices attached to it instead of returning them to the host")
	cmd.Flags().BoolVar(&force, "force", false, "pull the plug instead of asking the guest to shut down (unsaved guest state is lost)")
	return cmd
}

// stop is the stop verb's body: stop, detach, return. Restart uses it with
// keep, because returning a device only to claim it again a second later is
// two sudo prompts for nothing.
func (a *app) stop(name string, timeout time.Duration, keep bool) error {
	return a.stopForce(name, timeout, keep, false)
}

func (a *app) stopForce(name string, timeout time.Duration, keep, force bool) error {
	if err := devices.StopForce(a.c, a.cfg, name, int(timeout.Seconds()), force); err != nil {
		return err
	}
	stopInput(name)
	if keep {
		inst, _, err := a.c.Instance(name)
		if err == nil && len(inst.PassthroughDevices()) > 0 {
			note("devices left attached to %s", name)
		}
		return nil
	}
	detached, err := devices.Detach(a.c, a.cfg, name, false)
	if err != nil {
		return err
	}
	return returnDetached(detached)
}

func (a *app) restartCmd() *cobra.Command {
	var timeout time.Duration
	var envFile string
	cmd := &cobra.Command{
		Use:     "restart <vm>",
		GroupID: "vm",
		Short:   "Stop and start, re-injecting credentials",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			// Before the stop, so a bad path costs nothing: the VM is still up
			// and the operator still has a shell in it.
			if err := a.setEnvFile(args[0], envFile); err != nil {
				return err
			}
			if err := a.stop(args[0], timeout, true); err != nil {
				return err
			}
			return a.startInstance(args[0], timeout, true)
		},
	}
	cmd.Flags().StringVar(&envFile, "env", "",
		"host file of KEY=VALUE credentials; replaces this VM's for good, not just this restart")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Minute, "how long to wait for the guest to come up")
	return cmd
}

// --- the card and the policy ---------------------------------------------

func (a *app) claimCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "claim <vm>",
		Short:   "Give a stopped VM the devices it wants, without starting it",
		GroupID: "card",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			return devices.Claim(a.c, a.cfg, args[0])
		},
	}
}

func (a *app) releaseCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "release",
		Short:   "Detach every exclusive device from whoever holds it, and return it to the host",
		GroupID: "card",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			detached, err := devices.Release(a.c, a.cfg, force)
			if err != nil {
				return err
			}
			return returnDetached(detached)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"hot-unplug from a running instance — this WILL break its workload")
	return cmd
}

func (a *app) applyCmd() *cobra.Command {
	var dryRun bool
	var profile, file string
	cmd := &cobra.Command{
		Use:     "apply [-f rig.yaml]",
		Short:   "Reconcile the isolation ACL and the profile NIC, and a VM to its manifest",
		GroupID: "card",
		Long: "Declares the policy — the egress reject ranges and the three NIC keys —\n" +
			"and reconciles Incus to it. `incus admin init --preseed` does not cover\n" +
			"network ACLs, so this is the only way to get them onto a clean host.\n\n" +
			"With -f, also brings the VM the manifest names to what the file says:\n" +
			"its devices, credential file, size and ports. The VM has to exist\n" +
			"already; `rig new -f` is what creates one.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var m *manifest.Manifest
			if file != "" {
				var err error
				if m, err = manifest.Load(file); err != nil {
					return err
				}
			}
			changes, err := policy.Apply(a.c, a.cfg.ACL, profile, dryRun)
			if err != nil {
				for _, ch := range changes {
					fmt.Printf("  %s\n", ch)
				}
				return err
			}
			if len(changes) == 0 {
				fmt.Println("already reconciled; nothing to do.")
			} else {
				if dryRun {
					fmt.Println("dry run, nothing applied:")
				} else {
					fmt.Println("applied:")
				}
				for _, ch := range changes {
					fmt.Printf("  %s\n", ch)
				}
			}

			// Instance-level NIC overrides win over the profile, so reconciling
			// the profile does not necessarily isolate everything. Those are left
			// alone — an override may be deliberate — but they are worth naming.
			instances, err := a.c.Instances()
			if err != nil {
				return err
			}
			var stragglers []string
			for i := range instances {
				unisolated, noEgress := policy.Report(&instances[i], a.cfg.ACL)
				if len(unisolated) > 0 {
					stragglers = append(stragglers,
						fmt.Sprintf("  %s: no ACL on %s", instances[i].Name, strings.Join(unisolated, ", ")))
				} else if len(noEgress) > 0 {
					stragglers = append(stragglers,
						fmt.Sprintf("  %s: no egress on %s", instances[i].Name, strings.Join(noEgress, ", ")))
				}
			}
			if len(stragglers) > 0 {
				fmt.Println("\ninstances not covered (an instance's own NIC override wins over the profile; not changed):")
				for _, s := range stragglers {
					fmt.Println(s)
				}
			}
			if m == nil {
				return nil
			}
			if dryRun {
				inst, _, err := a.c.Instance(m.Guest.Name)
				if err != nil {
					return fmt.Errorf("no instance %s yet.\n  Create it from the file:  rig new -f %s", m.Guest.Name, m.Path)
				}
				want, err := desired(m)
				if err != nil {
					return err
				}
				drift := configDrift(inst, want)
				netDrift, err := a.reconcileNetwork(inst, m.Guest.Network == manifest.NetworkNone, true)
				if err != nil {
					return err
				}
				drift = append(drift, netDrift...)
				volDrift, err := volumes.Reconcile(a.c, inst, m.Guest.Volumes, true)
				if err != nil {
					return err
				}
				drift = append(drift, volDrift...)
				if len(drift) == 0 {
					fmt.Printf("\n%s matches %s; nothing to do.\n", m.Guest.Name, m.Path)
				} else {
					fmt.Printf("\n%s would change:\n", m.Guest.Name)
					for _, d := range drift {
						fmt.Printf("  %s\n", d)
					}
				}
				return nil
			}
			changes, err = a.reconcileManifest(m)
			if len(changes) == 0 && err == nil {
				fmt.Printf("\n%s matches %s; nothing to do.\n", m.Guest.Name, m.Path)
			} else if len(changes) > 0 {
				fmt.Printf("\n%s reconciled to %s:\n", m.Guest.Name, m.Path)
				for _, ch := range changes {
					fmt.Printf("  %s\n", ch)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without changing it")
	cmd.Flags().StringVar(&profile, "profile", envOr("RIG_PROFILE", "default"), "profile to reconcile")
	cmd.Flags().StringVarP(&file, "file", "f", "", "manifest whose VM to reconcile as well")
	return cmd
}

func (a *app) rmCmd() *cobra.Command {
	var force, dropVolumes bool
	cmd := &cobra.Command{
		Use:     "rm <vm>",
		GroupID: "vm",
		Short:   "Delete a stopped VM that rig created",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			inst, _, err := a.c.Instance(name)
			if err != nil {
				return err
			}
			if inst.Config[managedKey] != "true" {
				return fmt.Errorf("%s was not created by rig (no %s marker).\n"+
					"Delete it deliberately with incus if that is what you want.", name, managedKey)
			}
			if !inst.Stopped() {
				if !force {
					return fmt.Errorf("%s is %s. Stop it first, or pass --force.",
						name, strings.ToLower(inst.Status))
				}
				if err := a.stop(name, 2*time.Minute, false); err != nil {
					return err
				}
			}
			note("deleting %s and its disk (including /work)", name)
			if err := a.c.DeleteInstance(name); err != nil {
				return err
			}
			note("deleted %s", name)
			// Volumes outlive the VM on purpose, unless asked otherwise; either
			// way, say which, so none is forgotten or mistaken for deleted.
			pool := volumes.Pool(inst)
			for _, v := range volumes.Held(inst) {
				if !dropVolumes {
					note("kept volume %s (it outlives the VM; a new VM's manifest can mount it again)", v)
					continue
				}
				if err := a.c.DeleteCustomVolume(pool, v); err != nil {
					note("WARNING: could not delete volume %s: %v", v, err)
					continue
				}
				note("deleted volume %s and everything on it", v)
			}
			// The per-instance ACL for its published ports is not part of the
			// instance, so Incus does not take it along.
			if err := ports.Cleanup(a.c, name); err != nil {
				note("WARNING: could not delete %s's port ACL: %v", name, err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop it first if it is running")
	cmd.Flags().BoolVar(&dropVolumes, "volumes", false,
		"also delete the volumes it mounts, and everything on them (by default they are kept)")
	return cmd
}

// --- inspection ----------------------------------------------------------

func (a *app) statusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "status",
		GroupID: "vm",
		Short:   "Who holds which host device, and which VMs exist",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			instances, err := a.c.Instances()
			if err != nil {
				return err
			}
			if err := devices.CheckProfiles(a.c, instances); err != nil {
				return err
			}
			holders := devices.Holders(instances)

			type row struct {
				Name     string   `json:"name"`
				Status   string   `json:"status"`
				HasGPU   bool     `json:"has_gpu"`
				Devices  []string `json:"devices"`
				Isolated bool     `json:"isolated"`
				Managed  bool     `json:"managed"`
			}
			rows := []row{}
			for i := range instances {
				inst := &instances[i]
				hasGPU := false
				held := []string{}
				for _, h := range holders {
					if h.Instance != inst.Name {
						continue
					}
					held = append(held, h.Device)
					if h.Kind == manifest.KindGPU {
						hasGPU = true
					}
				}
				unisolated, _ := policy.Report(inst, a.cfg.ACL)
				rows = append(rows, row{inst.Name, inst.Status, hasGPU, held,
					len(unisolated) == 0, inst.Config[managedKey] == "true"})
			}

			if asJSON {
				if holders == nil {
					holders = []devices.Holder{}
				}
				// "card" is the address the next claim would use, discovered
				// even when nothing holds it, so callers need not re-implement
				// the lookup.
				card, _ := devices.DiscoverPCI(a.c, a.cfg)
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"card": card, "holders": holders, "instances": rows,
				})
			}

			// One line per exclusive device, and a loud one for an address
			// configured twice: that is the state where the next start
			// hot-unplugs hardware from a running guest, silently.
			byAddr := map[string][]devices.Holder{}
			for _, h := range holders {
				if h.Exclusive() {
					byAddr[h.PCI] = append(byAddr[h.PCI], h)
				}
			}
			if len(byAddr) == 0 {
				fmt.Println("no host device is assigned to a VM.")
			}
			for _, addr := range sortedKeys(byAddr) {
				hs := byAddr[addr]
				if len(hs) == 1 {
					fmt.Printf("%s %s held by %s (%s) as %s\n", hs[0].Kind, addr, hs[0].Instance, hs[0].Status, hs[0].Device)
					continue
				}
				var who []string
				for _, h := range hs {
					who = append(who, h.Instance)
				}
				fmt.Printf("!! %s %s is configured on MULTIPLE instances (%s) — starting a second\n", hs[0].Kind, addr, strings.Join(who, ", "))
				fmt.Println("   one will hot-unplug it from the first. Fix with `rig release`.")
			}
			fmt.Println()
			for _, r := range rows {
				mark := " "
				if len(r.Devices) > 0 {
					mark = "*"
				}
				flags := ""
				if !r.Isolated {
					flags = "  !! NO ISOLATION"
				}
				fmt.Printf(" %s %-20s %-10s%s\n", mark, r.Name, r.Status, flags)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// doctor answers "is this VM what I think it is" in one command, so the answer
// does not depend on remembering six checks in the right order.
func (a *app) doctorCmd() *cobra.Command {
	var image string
	cmd := &cobra.Command{
		Use:     "doctor <vm>",
		GroupID: "vm",
		Short:   "Check a VM is what you think it is",
		Long: "Reads configuration and asks the guest a few questions. It reports\n" +
			"that the isolation is configured; `rig verify` proves it holds.\n\n" +
			"Image drift is measured against the alias the VM was created from,\n" +
			"not the default one, so a project with its own guest image is not\n" +
			"reported as drifted from a base it was never built from.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			inst, _, err := a.c.Instance(name)
			if err != nil {
				return err
			}
			image = driftAlias(inst.Config[imageKey], image, cmd.Flags().Changed("image"))

			failed := 0
			check := func(label string, ok bool, detail string) {
				status := "ok  "
				if !ok {
					status = "FAIL"
					failed++
				}
				fmt.Printf("  %s  %-22s %s\n", status, label, detail)
			}
			// Drift is worth knowing, not worth failing on: a VM created before
			// the last image build is perfectly serviceable, it is just not what
			// a new one would be.
			info := func(label, detail string) {
				fmt.Printf("  note  %-22s %s\n", label, detail)
			}

			unisolated, noEgress := policy.Report(inst, a.cfg.ACL)
			check("network isolation", len(unisolated) == 0, isolationDetail(a.cfg.ACL, len(inst.NICs()), unisolated, noEgress))
			if inst.Config[inputKey] == manifest.InputHost {
				switch {
				case !inst.Running():
					info("host input", "lent from start to stop ("+inputUnit(name)+")")
				case inputActive(name):
					check("host input", true, inputUnit(name)+" is running; both Ctrl keys toggle")
				default:
					check("host input", false, "not running:  rig host input "+name+" --background")
				}
			}
			if limit := inst.Config["limits.cpu"]; cpuset.IsSet(limit) {
				warnings, err := cpuset.Check(limit)
				check("cpu pinning", err == nil, errText(err, limit+" (the host keeps a core of its own)"))
				for _, w := range warnings {
					info("cpu pinning", w)
				}
			}

			// Every device the VM wants, configured the way it was declared.
			// A stopped VM holds nothing after `rig stop`, so what is checked
			// there is the record; on a running one, the device itself.
			wanted, err := devices.Wanted(inst, a.cfg)
			if err != nil {
				return err
			}
			held := inst.PassthroughDevices()
			if len(wanted) == 0 {
				info("host devices", "none wanted")
			}
			for _, d := range wanted {
				label := "device " + d.Name
				detail := d.Kind + " " + firstNonEmpty(d.Address(), "(discovered at start)")
				if d.PCI != "" && d.ID != "" {
					detail += " (" + d.ID + ")"
				}
				if d.Return != nil {
					detail += ", returned on stop"
				}
				// What is at the address now, stopped or not: a renumbered bus
				// is found here, before a start hands the VM the wrong device.
				if err := devices.CheckIdentity(d); err != nil {
					check(label, false, err.Error())
					continue
				}
				switch {
				case inst.Running():
					dev, ok := held[d.Name]
					check(label, ok && (d.Address() == "" || dev != nil && sameDevice(dev, d)), detail)
				default:
					check(label, true, detail+" (attached at start)")
				}
			}
			for name, dev := range held {
				wantedToo := false
				for _, d := range wanted {
					wantedToo = wantedToo || d.Name == name
				}
				if !wantedToo {
					info("device "+name, dev.Type()+" attached but not in this VM's record; rig stop detaches it")
				}
			}

			// Published ports: what the record says against what Incus has. A
			// dry-run reconcile is exactly that diff.
			if recorded, err := recordedPorts(inst); err != nil {
				check("published ports", false, err.Error())
			} else if len(recorded) > 0 || len(inst.Config[portsKey]) > 0 {
				pending, err := ports.Reconcile(a.c, a.cfg.ACL, inst, recorded, true)
				switch {
				case err != nil:
					check("published ports", false, err.Error())
				case len(pending) > 0:
					check("published ports", false, "not as recorded; rig apply -f reconciles:")
					for _, line := range pending {
						fmt.Printf("        %s\n", line)
					}
				case len(recorded) == 0:
					// nothing published, nothing recorded: not worth a line
				default:
					var names []string
					for _, p := range recorded {
						names = append(names, p.String())
					}
					check("published ports", true, strings.Join(names, " ")+" ("+ports.Reach(recorded)+" -> guest; guest firewall is the image's)")
					// The host's own firewall is the layer a test from the host
					// cannot see. ufw's status needs root; the kernel log does
					// not, and a client failing right now is in it.
					if ports.HostFirewall() {
						guestIP := ""
						for _, nic := range inst.NICs() {
							guestIP = nic["ipv4.address"]
						}
						rules := ports.UFWRules(guestIP, recorded)
						if n := ports.RecentlyBlocked(guestIP); n > 0 {
							check("host firewall", false, fmt.Sprintf("ufw dropped %d forwarded packet(s) to %s in the last 10 minutes; admit them:", n, guestIP))
						} else {
							info("host firewall", "ufw is active; if a LAN client cannot connect, admit the forwards:")
						}
						for _, r := range rules {
							fmt.Printf("        %s\n", r)
						}
					}
				}
			}

			if m, where := loadRecordedManifest(inst); where != "" {
				switch {
				case m == nil:
					info("manifest", where)
				default:
					want, err := desired(m)
					if err != nil {
						info("manifest", where+" ("+err.Error()+")")
						break
					}
					drift := configDrift(inst, want)
					if netDrift, err := a.reconcileNetwork(inst, m.Guest.Network == manifest.NetworkNone, true); err == nil {
						drift = append(drift, netDrift...)
					}
					if volDrift, err := volumes.Reconcile(a.c, inst, m.Guest.Volumes, true); err == nil {
						drift = append(drift, volDrift...)
					}
					if len(drift) > 0 {
						info("manifest", where+" differs; rig apply -f reconciles:")
						for _, line := range drift {
							fmt.Printf("        %s\n", line)
						}
					} else {
						check("manifest", true, where)
					}
				}
			}

			if drifted, detail := a.imageDrift(inst, image); drifted {
				info("base image", detail)
			} else {
				check("base image", true, detail)
			}

			if !inst.Running() {
				fmt.Printf("  --    %-22s %s\n", "guest checks", "skipped: instance is "+inst.Status)
				if failed > 0 {
					return fmt.Errorf("%d check(s) failed", failed)
				}
				return nil
			}

			agentErr := a.c.WaitAgent(name, 15*time.Second)
			check("guest agent", agentErr == nil, errText(agentErr, "responds"))

			addr, addrErr := a.c.GlobalIPv4(name)
			check("ipv4 address", addrErr == nil && addr != "",
				firstNonEmpty(addr, "none — DHCP may still be running"))

			if agentErr == nil {
				for _, d := range wanted {
					probe, label := guestProbe(d)
					if probe == "" {
						continue
					}
					// A USB device that is not on this host cannot be in the
					// guest either, and that is not a fault: a monitor's KVM
					// on another input has it. Say so instead of failing.
					if d.Kind == manifest.KindUSB && !hostdev.USBPresent(d.ID) {
						info(label, d.ID+" is not plugged into this host right now (a KVM on another input?); it joins the guest when it appears")
						continue
					}
					out, err := a.c.Exec(name, probe, incus.ExecOpts{Timeout: 30 * time.Second})
					check(label, err == nil, lastLine(firstNonEmpty(strings.TrimSpace(out), d.Address())))
				}

				if inst.Config[creds.InstanceKey] != "" {
					_, err := a.c.Exec(name, "test -r "+creds.GuestPath, incus.ExecOpts{Timeout: 15 * time.Second})
					check("credentials", err == nil, creds.GuestPath)
				}
			}

			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			fmt.Printf("\nIsolation is configured. To prove it holds:  rig verify %s\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&image, "image", envOr("RIG_IMAGE", defaultImage),
		"alias to compare this VM's base image against")
	return cmd
}

func (a *app) logsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "logs <vm>",
		GroupID: "vm",
		Short:   "Console log, for when a VM never comes up",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			log, err := a.c.ConsoleLog(args[0])
			if err != nil {
				return err
			}
			fmt.Println(log)
			return nil
		},
	}
}

// --- working inside a guest ----------------------------------------------

func (a *app) execCmd() *cobra.Command {
	var dir string
	var stdin bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:     "exec <vm> <command>...",
		GroupID: "guest",
		Short:   "Run a command in the guest",
		Long: "Run a command in the guest through a login shell, so the guest's own\n" +
			"PATH applies.\n\n" +
			"rig's own flags must come before the instance name, because everything\n" +
			"after it belongs to the guest command:\n" +
			"  rig exec --dir /work/foo myvm ls -la",
		Args:                  cobra.MinimumNArgs(2),
		DisableFlagsInUseLine: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			_, err := a.c.Exec(args[0], strings.Join(args[1:], " "), incus.ExecOpts{
				Dir: dir, Stdin: stdin, Timeout: timeout, Streaming: true,
			})
			return err
		},
	}
	// Everything after the instance name is the guest's, flags included.
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVar(&dir, "dir", "", "working directory in the guest")
	cmd.Flags().BoolVar(&stdin, "stdin", false, "forward stdin")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "kill the command after this long")
	return cmd
}

func (a *app) shellCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "shell <vm>",
		GroupID: "guest",
		Short:   "Interactive login shell in the guest",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			return a.c.Shell(args[0])
		},
	}
}

func (a *app) pushCmd() *cobra.Command {
	var dest string
	var force bool
	cmd := &cobra.Command{
		Use:     "push <vm> <src-dir>",
		GroupID: "guest",
		Short:   "Copy a host directory's contents into the guest",
		Long: "Copies the *contents* of <src-dir> to <dest>, which defaults to\n" +
			"/work/<basename>. Unlike `incus file push -r`, the destination is\n" +
			"exactly the destination — no directory named after the source appears.",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			name, src := args[0], strings.TrimRight(args[1], "/")
			if err := a.requireInstance(name); err != nil {
				return err
			}
			target := dest
			if target == "" {
				abs, err := filepath.Abs(src)
				if err != nil {
					return err
				}
				target = path.Join("/work", filepath.Base(abs))
			}
			// Refuse to destroy anything rig did not itself write. /work lives
			// inside the instance, so a clobbered guest file may have been the
			// only copy.
			if !force {
				conflicts, err := pushguard.Check(a.c, name, src, target)
				if err != nil {
					return err
				}
				if len(conflicts) > 0 {
					return pushguard.Error(conflicts, name, target)
				}
			}

			n, err := a.c.PushDir(name, src, target)
			if err != nil {
				return err
			}

			// Record what we wrote, so the next push can tell our own writes
			// from someone else's. A failure here is not fatal — the push
			// already happened — but it must be visible, because the next push
			// will refuse rather than clobber and that needs explaining.
			contents, readErr := pushguard.ReadAll(src)
			if readErr == nil {
				readErr = pushguard.Record(a.c, name, src, target, contents)
			}
			if readErr != nil {
				note("WARNING: pushed, but could not record what was written: %v", readErr)
				note("         The next push to %s will ask for --force.", target)
			}

			note("pushed %d file(s) to %s:%s", n, name, target)
			return nil
		},
	}
	cmd.Flags().StringVar(&dest, "dest", "", "guest directory (default /work/<source basename>)")
	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite guest files rig did not write — read them first, the guest copy may be the only one")
	return cmd
}

func (a *app) pullCmd() *cobra.Command {
	var out string
	var force bool
	cmd := &cobra.Command{
		Use:     "pull <vm> <guest-path>",
		GroupID: "guest",
		Short:   "Read a file out of the guest",
		Args:    cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			content, err := a.c.Pull(args[0], args[1])
			if err != nil {
				return err
			}
			if out == "" {
				_, err = os.Stdout.Write(content)
				return err
			}
			// Same hazard in the other direction, and cheaper to guard: --out
			// names one file, so an accidental overwrite is a whole host file.
			if _, err := os.Stat(out); err == nil && !force {
				return fmt.Errorf("%s already exists.\n"+
					"  Look at it first, then:  rig pull --force --out %s %s %s",
					out, out, args[0], args[1])
			}
			return os.WriteFile(out, content, 0o644)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write to this file instead of stdout")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite --out if it already exists")
	return cmd
}

// --- helpers -------------------------------------------------------------

// driftAlias picks the image alias `doctor` measures an instance against.
//
// The recorded alias wins over the default, because "has this VM drifted from
// the image it was built from" is the question worth asking; comparing a
// project's guest image against the base alias reports drift that does not
// exist. An explicit --image still wins over both: that is the operator asking
// a different question on purpose.
func driftAlias(recorded, flagValue string, flagChanged bool) string {
	if flagChanged || recorded == "" {
		return flagValue
	}
	return recorded
}

// sameDevice reports whether an Incus device is the one a declaration asks
// for: same type and same identity, whatever else Incus added to it.
func sameDevice(dev incus.Device, d devices.Decl) bool {
	want := d.IncusDevice()
	for k, v := range want {
		if dev[k] != v {
			return false
		}
	}
	return true
}

// guestProbe is the command that proves a device reached the guest, and the
// label to report it under. A card is asked to compute; anything else is
// looked for on the guest's bus by the ids the host reads from sysfs, since
// the address is different inside a VM.
func guestProbe(d devices.Decl) (cmd, label string) {
	switch d.Kind {
	case manifest.KindGPU:
		return "gpu-check", "gpu visible in guest"
	case manifest.KindPCI:
		ids := hostdev.IDs(d.PCI)
		if ids == "" {
			return "", ""
		}
		return fmt.Sprintf("lspci -nn -d %s | grep -q . && lspci -nn -d %s", ids, ids), d.Name + " visible in guest"
	case manifest.KindUSB:
		vendor, product, _ := strings.Cut(d.ID, ":")
		return fmt.Sprintf("for d in /sys/bus/usb/devices/*; do "+
			"[ \"$(cat $d/idVendor 2>/dev/null)\" = %s ] && [ \"$(cat $d/idProduct 2>/dev/null)\" = %s ] && echo \"$(cat $d/product 2>/dev/null) (%s)\" && exit 0; "+
			"done; echo \"%s not on the guest's USB bus\"; exit 1", vendor, product, d.ID, d.ID), d.Name + " visible in guest"
	}
	return "", ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isolationDetail(acl string, nics int, unisolated, noEgress []string) string {
	if nics == 0 {
		// Isolated by construction: the profile's NIC is masked, so there is
		// no interface for the ACL to be on. Naming the ACL here would suggest
		// it is doing the work.
		return "no network device"
	}
	if len(unisolated) > 0 {
		return "no " + acl + " on " + strings.Join(unisolated, ", ")
	}
	if len(noEgress) > 0 {
		return acl + " attached but no egress on " + strings.Join(noEgress, ", ")
	}
	return acl
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	var n int
	if _, err := fmt.Sscanf(os.Getenv(key), "%d", &n); err == nil && n > 0 {
		return n
	}
	return fallback
}

func errText(err error, ok string) string {
	if err == nil {
		return ok
	}
	return err.Error()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
