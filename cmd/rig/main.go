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
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/tbrockman/rig/internal/cpuset"
	"github.com/tbrockman/rig/internal/creds"
	"github.com/tbrockman/rig/internal/devices"
	"github.com/tbrockman/rig/internal/hostdev"
	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
	"github.com/tbrockman/rig/internal/policy"
	"github.com/tbrockman/rig/internal/ports"
	"github.com/tbrockman/rig/internal/proxy"
	"github.com/tbrockman/rig/internal/volumes"
)

// managedKey marks instances rig created. rm refuses anything without it, so it
// cannot delete something built by hand.
const managedKey = "user.rig.managed"

// defaultImage is the alias `rig image build` writes and `rig new` reads.
const (
	defaultImage = "rig-base"
	gpuImage     = "rig-nvidia" // rig image build --attr guest-nvidia --alias rig-nvidia
)

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,61}$`)

type app struct {
	c   *incus.Client
	cfg devices.Config
}

func main() {
	a := &app{c: incus.New(""), cfg: devices.ConfigFromEnv()}

	root := &cobra.Command{
		Use:   "rig",
		Short: "Isolated NixOS VMs that get only the host resources you grant them",
		Long: "rig creates and runs isolated NixOS VMs on Incus. A VM gets no route to\n" +
			"this host or the LAN and no host devices unless they are granted: in a\n" +
			"rig.yaml, or with a flag. Grants are recorded on the instance, checked\n" +
			"before a start, and handed back on stop.\n\n" +
			"It enforces two invariants: a host device passed through to a VM is\n" +
			"configured on at most one instance and never moves away from a running\n" +
			"one, and no instance starts without network isolation.\n\n" +
			"Typical flow, once the host is set up (docs/RUNBOOK.md):\n" +
			"  rig setup                              # the isolation ACL and the rig profile\n" +
			"  rig image build                        # the guest image, from base/\n" +
			"  rig init myproj                        # a rig.yaml and a guest flake\n" +
			"  rig apply -f myproj/rig.yaml --start   # the VM that file describes\n" +
			"  rig doctor myproj && rig verify myproj # configured, then proven\n" +
			"  rig sh myproj\n\n" +
			"Exit status is 0 or 1 except where a verb says otherwise: verify exits 2\n" +
			"for \"could not be proven\", and exec carries the guest command's status out.\n" +
			"Progress notes go to stdout prefixed \"rig:\"; errors go to stderr.\n\n" +
			"Environment (each has a flag or a default; none is required):\n" +
			"  RIG_PCI           the card --gpu gives, when discovery picks wrong\n" +
			"  RIG_ACL           name of the isolation ACL (vm-isolate)\n" +
			"  RIG_PROFILE       profile that carries the isolation and that new VMs use (rig)\n" +
			"  RIG_LOCK          lock file serialising device moves (/var/lock/rig.lock)\n" +
			"  RIG_IMAGE         image alias new VMs are made from (rig-base)\n" +
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
		&cobra.Group{ID: "vm", Title: "VMs:"},
		&cobra.Group{ID: "guest", Title: "Working inside a guest:"},
		&cobra.Group{ID: "card", Title: "Grants and isolation:"},
	)
	root.AddCommand(
		a.initCmd(), a.schemaCmd(),
		a.newCmd(), a.startCmd(), a.stopCmd(), a.restartCmd(), a.rmCmd(), a.deleteCmd(),
		a.listCmd(), a.statusCmd(), a.doctorCmd(), a.verifyCmd(), a.logsCmd(),
		a.execCmd(), a.shellCmd(), a.pushCmd(), a.pullCmd(), a.agentCmd(),
		a.credsCmd(), a.proxyCmd(),
		a.mountCmd(), a.unmountCmd(), a.forwardCmd(),
		a.claimCmd(), a.releaseCmd(), a.applyCmd(), a.setupCmd(), a.hostCmd(), a.imageCmd(),
	)
	root.AddCommand(a.overseerCmd(root))
	auditVerbs(root)

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
		envFile, image, memory, disk, profile string
		cpus                                  int
		start, gpu                            bool
	)
	cmd := &cobra.Command{
		Use:     "new <vm>",
		GroupID: "vm",
		Short:   "Create an isolated VM",
		Long: "Creates a stopped VM from the base image. Its NIC — and so its network\n" +
			"isolation — and its root disk come from the profile, which is the one\n" +
			"`rig setup` isolates. It is given no host devices unless asked:\n" +
			"--gpu gives it this host's NVIDIA card, and a manifest can give it\n" +
			"anything else. The image alias, the credential file and the devices are\n" +
			"recorded on the instance, so later verbs need none of them repeated.\n\n" +
			"For anything more, write a rig.yaml (`rig init`) and `rig apply -f` it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			// A card needs its driver in the guest, which the plain base does
			// not carry.
			if gpu && !cmd.Flags().Changed("image") && os.Getenv("RIG_IMAGE") == "" {
				image = gpuImage
			}
			if !nameRE.MatchString(name) {
				return fmt.Errorf("not a valid instance name: %s", name)
			}
			if a.c.Exists(name) {
				return fmt.Errorf("%s already exists", name)
			}
			if !a.c.ImageExists(image) {
				build := "rig image build"
				if image == gpuImage {
					build += " --attr guest-nvidia --alias " + gpuImage
				}
				return fmt.Errorf("no such image: %s\n  Build it:  %s", image, build)
			}

			absEnv, err := absEnvFile(envFile)
			if err != nil {
				return err
			}

			// Recorded either way, so what the VM was given is never a
			// question for a later verb.
			var decls []devices.Decl
			if gpu {
				decls = []devices.Decl{{Name: "gpu", Kind: manifest.KindGPU}}
			}
			config := map[string]string{managedKey: "true", imageKey: image, devices.DevicesKey: devices.Encode(decls)}
			if absEnv != "" {
				config[creds.InstanceKey] = absEnv
			}
			if err := a.c.CreateVM(incus.CreateOpts{
				Name: name, Image: image, Profile: profile, CPUs: cpus, Memory: memory,
				DiskSize: disk, Config: config,
			}); err != nil {
				return err
			}

			gpuNote := ", no host devices"
			if gpu {
				gpuNote = ", with the GPU"
			}
			note("created %s (image %s, %d cpus, %s, %s disk%s)", name, image, cpus, memory, disk, gpuNote)
			if absEnv != "" {
				note("credentials will be injected from %s", absEnv)
			}
			return a.afterNew(name, profile, start)
		},
	}
	f := cmd.Flags()
	f.StringVar(&envFile, "env", "", "host file of KEY=VALUE credentials to inject on start")
	f.StringVar(&image, "image", envOr("RIG_IMAGE", defaultImage), "base image alias")
	f.StringVar(&profile, "profile", envOr("RIG_PROFILE", policy.DefaultProfile),
		"Incus profile the VM inherits its NIC and root disk from; rig setup isolates the same one")
	f.IntVar(&cpus, "cpus", envInt("RIG_CPUS", 8), "vCPUs")
	f.StringVar(&memory, "memory", envOr("RIG_MEMORY", "16GiB"), "RAM")
	f.StringVar(&disk, "disk", envOr("RIG_DISK", "40GiB"), "root disk size")
	f.BoolVar(&start, "start", false, "start it once created")
	f.BoolVar(&gpu, "gpu", false,
		"give it this host's NVIDIA card on start (RIG_PCI, else discovered); implies --image "+gpuImage+" unless one is named")
	return cmd
}

// afterNew is what every creation ends with: the isolation check, and the
// start when asked for.
func (a *app) afterNew(name, profile string, start bool) error {
	// Isolation is inherited from the profile. Verify it landed
	// rather than assuming: a new VM with no ACL is the failure this
	// project exists to prevent, and it is silent.
	inst, _, err := a.c.Instance(name)
	if err != nil {
		return err
	}
	if !policy.Isolated(inst, a.cfg.ACL) {
		fix := "rig setup"
		if profile != policy.DefaultProfile {
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
	// The caller has said it was created; this adds what that does not show.
	if len(wanted) > 0 {
		var names []string
		for _, d := range wanted {
			names = append(names, d.Name)
		}
		note("it will claim %s on start", strings.Join(names, ", "))
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
	var noWait, allowUnisolated, killHolders bool
	cmd := &cobra.Command{
		Use:     "start <vm>",
		Short:   "Claim the VM's devices, start it, inject credentials",
		GroupID: "vm",
		Long: "Claims the devices the VM's record declares, stopping any host unit a\n" +
			"device's return: recipe names, then starts the VM, lends it this host's\n" +
			"input when its manifest asks, and injects its credentials.\n\n" +
			"A device whose recipe resets it is parked on vfio-pci before the start,\n" +
			"once its identity has been checked. Incus hands a device back at stop to\n" +
			"the driver it had at start, unreset; parked, that is vfio-pci, and the\n" +
			"return resets it before the host driver loads.\n\n" +
			"For a VM whose devices have a return: recipe it also leaves a watcher,\n" +
			"the transient unit rig-handback-<vm>, that hands them back if the VM\n" +
			"stops without rig stop: the guest powering itself off, or incus stop.\n\n" +
			"The steps that touch this host run under sudo, printing the command first.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			if err := a.setEnvFile(args[0], envFile); err != nil {
				return err
			}
			return a.start(args[0], timeout, !noWait, allowUnisolated, killHolders)
		},
	}
	cmd.Flags().StringVar(&envFile, "env", "",
		"host file of KEY=VALUE credentials; replaces this VM's for good, not just this start")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Minute, "how long to wait for the guest to come up")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return as soon as Incus reports it started")
	cmd.Flags().BoolVar(&allowUnisolated, "allow-unisolated", false,
		"start even with no isolation ACL — the guest will reach this host and the LAN")
	cmd.Flags().BoolVar(&killHolders, "kill-holders", false,
		"end host processes that hold a card the VM needs (SIGTERM, then SIGKILL) and start anyway")
	return cmd
}

func (a *app) startInstance(name string, timeout time.Duration, wait bool) error {
	return a.start(name, timeout, wait, false, false)
}

func (a *app) start(name string, timeout time.Duration, wait, allowUnisolated, killHolders bool) error {
	inst, _, err := a.c.Instance(name)
	if err != nil {
		return err
	}
	wanted, err := devices.Wanted(inst, a.cfg)
	if err != nil {
		return err
	}
	if len(wanted) == 0 {
		note("%s wants no host devices", name)
	}
	// Proxied credentials need witmproxy on this host: found now, with the
	// VM still stopped, rather than once it is up with nothing to work with.
	if err := a.proxyPreflight(inst); err != nil {
		return err
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
	claimAndStart := func() error {
		return devices.Start(a.c, a.cfg, name, allowUnisolated, int(timeout.Seconds()), parkClaimed(killHolders))
	}
	if err := startPastHolders(name, killHolders, claimAndStart); err != nil {
		return err
	}
	a.startHandback(name, wanted)
	if !wait {
		a.startInput(name, wait)
		a.startForwards(name, wait)
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
		a.startForwards(name, wait)
		return nil
	}
	if _, statErr := os.Stat(envFile); statErr != nil {
		note("WARNING: %s expects credentials at %s, which is missing.", name, envFile)
		note("         The guest started without them.")
		a.startForwards(name, wait)
		return nil
	}
	n, err := a.injectCredentials(name, envFile)
	if err != nil {
		return err
	}
	note("injected %d credential(s) into %s (tmpfs; gone on stop)", n, creds.GuestPath)
	a.startForwards(name, wait)
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
			"next restarts. A proxied credential (guest.proxy) reaches it sooner: the\n" +
			"stand-in the guest holds stays, and its proxy takes the new value, so\n" +
			"nothing in the guest changes. Nothing here reads or logs the secret itself.",
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
			n, err := a.injectCredentials(name, abs)
			if err != nil {
				return err
			}
			note("injected %d credential(s) from %s into %s (tmpfs; gone on stop)",
				n, abs, creds.GuestPath)
			if inst.Config[proxy.InstanceKey] != "" {
				note("a running agent keeps its current process, and the swap has reached it already: its stand-in is unchanged and its proxy holds the new value")
			} else {
				note("a running agent keeps its current process; it authenticates fresh on its next restart")
			}
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
			"--keep-devices leaves everything attached, as stop always did before.\n\n" +
			"A VM that stops without rig stop has its devices handed back by the\n" +
			"watcher rig start left (rig host handback). On a VM already stopped,\n" +
			"rig stop hands back whatever is still attached.",
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
	stopForwards(name)
	// Its credentials leave the proxy with it; the next start puts them back.
	if inst, _, err := a.c.Instance(name); err == nil {
		a.proxyRevoke(inst)
	}
	if keep {
		inst, _, err := a.c.Instance(name)
		if err == nil && len(inst.PassthroughDevices()) > 0 {
			note("devices left attached to %s", name)
		}
		return nil
	}
	return a.handBack(name)
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

func (a *app) setupCmd() *cobra.Command {
	var dryRun bool
	var profile string
	cmd := &cobra.Command{
		Use:     "setup",
		Short:   "Set up this host: the isolation ACL and the rig profile",
		GroupID: "card",
		Long: "Declares the isolation policy (the egress reject ranges and the three\n" +
			"NIC keys) and reconciles Incus to it: the ACL, and a profile of rig's\n" +
			"own, created from `default`'s NIC and root disk when it does not exist,\n" +
			"with the ACL on its NIC. rig's VMs use that profile, so nothing else on\n" +
			"this host's Incus changes. Safe to run again; it changes only what\n" +
			"differs.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			changes, err := policy.Apply(a.c, a.cfg.ACL, profile, dryRun)
			if err != nil {
				for _, ch := range changes {
					fmt.Printf("  %s\n", ch)
				}
				return err
			}
			if len(changes) == 0 {
				fmt.Println("already set up; nothing to do.")
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
				if instances[i].Config[managedKey] != "true" {
					continue
				}
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
				fmt.Println("\nrig VMs not covered (an instance's own NIC override wins over the profile; not changed):")
				for _, s := range stragglers {
					fmt.Println(s)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without changing it")
	cmd.Flags().StringVar(&profile, "profile", envOr("RIG_PROFILE", policy.DefaultProfile), "profile to reconcile")
	return cmd
}

func (a *app) applyCmd() *cobra.Command {
	var dryRun, start bool
	var profile, file string
	cmd := &cobra.Command{
		Use:     "apply -f rig.yaml",
		Short:   "Create the VM a manifest describes, or bring it back in line with it",
		GroupID: "vm",
		Long: "Creates the VM rig.yaml names when it does not exist, building its image\n" +
			"from `flake:` when that is missing. When it exists, brings it to what the\n" +
			"file says: devices, credential file, size, ports, network, volumes.\n" +
			"Safe to run again; it changes only what differs.\n\n" +
			"It does not start or stop anything (--start starts it), and it never\n" +
			"recreates a VM: that would wipe its disk. A change that needs a new VM,\n" +
			"such as a new image, is reported, and `rig delete -f` then `rig apply -f`\n" +
			"makes it. A change a running VM cannot take waits for its next start.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if file == "" {
				return errors.New("rig apply needs -f rig.yaml.\n  Setting up the host (the ACL and the rig profile) is:  rig setup")
			}
			m, err := manifest.Load(file)
			if err != nil {
				return err
			}
			name := m.Guest.Name
			inst, _, err := a.c.Instance(name)
			if err != nil {
				// Not there yet: this is a create.
				if dryRun {
					fmt.Printf("%s does not exist; rig apply -f would create it from %s (image %s).\n", name, m.Path, m.ImageAlias())
					return nil
				}
				if err := a.newFromManifest(m, profile); err != nil {
					return err
				}
				note("created %s from %s", name, m.Path)
				return a.afterNew(name, profile, start)
			}
			if dryRun {
				want, err := desired(m)
				if err != nil {
					return err
				}
				drift := configDrift(inst, want)
				netDrift, err := a.reconcileNetwork(inst, m.Guest.Network == manifest.NetworkNone, m.Proxied(), true)
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
					fmt.Printf("%s matches %s; nothing to do.\n", name, m.Path)
				} else {
					fmt.Printf("%s would change:\n", name)
					for _, d := range drift {
						fmt.Printf("  %s\n", d)
					}
				}
				return nil
			}
			changes, err := a.reconcileManifest(m)
			if len(changes) == 0 && err == nil {
				fmt.Printf("%s matches %s; nothing to do.\n", name, m.Path)
			} else if len(changes) > 0 {
				fmt.Printf("%s reconciled to %s:\n", name, m.Path)
				for _, ch := range changes {
					fmt.Printf("  %s\n", ch)
				}
			}
			if err != nil || !start {
				return err
			}
			if inst, _, err = a.c.Instance(name); err == nil && inst.Running() {
				return nil
			}
			return a.startInstance(name, 3*time.Minute, true)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "the manifest (rig.yaml)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without changing it")
	cmd.Flags().BoolVar(&start, "start", false, "start it afterwards, if it is not running")
	cmd.Flags().StringVar(&profile, "profile", envOr("RIG_PROFILE", policy.DefaultProfile),
		"Incus profile a new VM inherits its NIC and root disk from; rig setup isolates the same one")
	return cmd
}

// rmLong is what rm and delete both do, since delete is rm by manifest.
const rmLong = "Deletes the VM and its disk, after asking: everything on that disk is gone\n" +
	"for good, /work included. Only volumes outlive it, and they are kept unless\n" +
	"--volumes says otherwise. A running VM is refused unless --force stops it\n" +
	"first. Without a terminal to ask on, --yes is required."

func (a *app) rmCmd() *cobra.Command {
	var force, dropVolumes, yes bool
	cmd := &cobra.Command{
		Use:     "rm <vm>",
		GroupID: "vm",
		Short:   "Delete a VM that rig created, and its disk (by manifest: rig delete -f)",
		Long:    rmLong,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			// `rig rm -f rig.yaml` was this verb's manifest form, before -f
			// came to mean force here as it does for docker rm.
			if strings.HasSuffix(args[0], ".yaml") || strings.HasSuffix(args[0], ".yml") {
				return fmt.Errorf("%s is a manifest, and rm takes a VM's name. By manifest:\n  rig delete -f %s", args[0], args[0])
			}
			return a.remove(args[0], force, yes, dropVolumes)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "stop it first if it is running")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask; for scripts, which have no terminal to ask on")
	cmd.Flags().BoolVar(&dropVolumes, "volumes", false,
		"also delete the volumes it mounts, and everything on them (by default they are kept)")
	return cmd
}

func (a *app) deleteCmd() *cobra.Command {
	var force, dropVolumes, yes bool
	var file string
	cmd := &cobra.Command{
		Use:     "delete -f rig.yaml",
		GroupID: "vm",
		Short:   "Delete the VM a manifest describes, and its disk (by name: rig rm)",
		Long:    rmLong + "\n\n-f names the manifest, as it does for apply.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("delete takes -f rig.yaml, not a name. By name:\n  rig rm %s", args[0])
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			if file == "" {
				return errors.New("rig delete needs -f rig.yaml. By name:  rig rm <vm>")
			}
			m, err := manifest.Load(file)
			if err != nil {
				return err
			}
			return a.remove(m.Guest.Name, force, yes, dropVolumes)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "the manifest naming the VM")
	cmd.Flags().BoolVar(&force, "force", false, "stop it first if it is running")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask; for scripts, which have no terminal to ask on")
	cmd.Flags().BoolVar(&dropVolumes, "volumes", false,
		"also delete the volumes it mounts, and everything on them (by default they are kept)")
	return cmd
}

// remove is rm and delete: it asks, stops the VM if force allows, and deletes
// it with what it alone owns, keeping its volumes unless dropVolumes.
func (a *app) remove(name string, force, yes, dropVolumes bool) error {
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
	if !inst.Stopped() && !force {
		return fmt.Errorf("%s is %s. Stop it first, or pass --force.",
			name, strings.ToLower(inst.Status))
	}
	if !yes {
		vols := volumes.Held(inst)
		note("this deletes %s's disk for good, and everything on it that is not on a volume:", name)
		note("  /work, /home, and every other change made inside the guest.")
		switch {
		case len(vols) == 0:
			note("it mounts no volumes, so nothing of it is kept.")
		case dropVolumes:
			note("--volumes: its volumes go too: %s", strings.Join(vols, ", "))
		default:
			note("kept: its volumes %s", strings.Join(vols, ", "))
		}
		if !inst.Stopped() {
			note("it is %s; it is stopped first.", strings.ToLower(inst.Status))
		}
		ok, err := confirm("Delete "+name+"?", "--yes")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("kept %s; nothing was deleted", name)
		}
	}
	if !inst.Stopped() {
		if err := a.stop(name, 2*time.Minute, false); err != nil {
			return err
		}
	}
	note("deleting %s and its disk (including /work)", name)
	if err := a.c.DeleteInstance(name); err != nil {
		return err
	}
	note("deleted %s", name)
	a.proxyRemove(inst)
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
}

// --- inspection ----------------------------------------------------------

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

			// Forwards are vsock, not a grant through the ACL, and a port named
			// under both is published to the network and loopback at once:
			// most often one that was meant to be only the latter.
			if fwd, err := recordedForwards(inst); err != nil {
				check("forwards", false, err.Error())
			} else if len(fwd) > 0 {
				recorded, _ := recordedPorts(inst)
				var both []string
				for _, p := range fwd {
					for _, pub := range recorded {
						if lo, hi := pub.TargetRange(); p >= lo && p <= hi {
							both = append(both, strconv.Itoa(p))
						}
					}
				}
				if len(both) > 0 {
					info("forwards", "port(s) "+strings.Join(both, ", ")+" are both forwarded to loopback and published on the network (guest.ports); for a preview of your own, forwards alone is enough")
				}
				switch {
				case !inst.Running():
					info("forwards", fmt.Sprintf("%v held on 127.0.0.1 from the next start (%s)", fwd, forwardUnit(name)))
				case forwardActive(name):
					check("forwards", true, fmt.Sprintf("%v on 127.0.0.1 over vsock (%s)", fwd, forwardUnit(name)))
				default:
					check("forwards", false, fmt.Sprintf("%s is not running; the ports are not held:  rig forward %s --background", forwardUnit(name), name))
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
					if netDrift, err := a.reconcileNetwork(inst, m.Guest.Network == manifest.NetworkNone, m.Proxied(), true); err == nil {
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

			// Proxied credentials: what the record asks, whether the guest's
			// proxy is there and running. The values never print.
			proxied, proxyErr := proxy.Decode(inst.Config[proxy.InstanceKey])
			proxyIP := policy.ProxyIPOf(inst)
			if proxyErr != nil {
				check("proxied credentials", false, proxyErr.Error())
			} else if len(proxied) > 0 {
				asked := proxy.DescribeRecord(proxied)
				check("proxy nic", proxyIP != "", firstNonEmpty(proxyIP, "none on "+policy.ProxyNetwork+":  rig apply -f, with the VM stopped"))
				if inst.Config[proxy.EgressKey] == manifest.EgressInternet {
					info("proxy egress", "internet: everything the isolated NIC may reach, through the proxy too; this host and the LAN refused")
				} else {
					info("proxy egress", "the credentials' hosts only (guest.proxy.egress: internet widens it)")
				}
				if err := proxy.WitmHasUserScope(); err != nil {
					check("witmproxy", false, lastLine(err.Error()))
				} else if proxyIP != "" {
					pi := proxy.InstanceFor(name, proxyIP)
					if on, known := proxy.Linger(); known && !on {
						info("linger", "off: the proxies stop when you log out;  rig proxy setup")
					}
					// The host's firewall sits between a guest and its
					// proxy; its status needs root, the kernel log does not.
					if ports.HostFirewall() {
						if n := proxy.RecentlyBlocked(); n > 0 {
							check("host firewall", false, fmt.Sprintf("ufw dropped %d packet(s) from guests to their proxies in the last 10 minutes;  rig proxy setup admits them:  %s", n, proxy.UFWRule()))
						} else {
							info("host firewall", "ufw is active; no drops from guests in the last 10 minutes (rig proxy setup admits their proxy ports)")
						}
					}
					switch {
					case !pi.Installed() && !inst.Running():
						info("proxy", pi.Name()+" at "+pi.ProxyBind()+" (made at start)")
					case !pi.Installed():
						check("proxy", false, pi.Name()+" was never made:  rig creds "+name+" <env-file>")
					case !pi.Running() && !inst.Running():
						info("proxy", pi.Name()+" at "+pi.ProxyBind()+" (stopped with the VM)")
					case !pi.Running():
						check("proxy", false, pi.Name()+" is not running while the VM is:  rig creds "+name+" <env-file>;  rig proxy log "+name)
					default:
						if c, err := pi.Connect(); err != nil {
							check("proxy", false, lastLine(err.Error()))
						} else {
							check("proxy", true, pi.Name()+" at "+pi.ProxyBind())
							if p, err := c.RigPlugin(); err != nil || p == nil {
								check("proxy plugin", false, proxy.PluginID+" is not in it:  rig creds "+name+" <env-file>")
							} else if !p.Enabled {
								check("proxy plugin", false, proxy.PluginID+" "+p.Version+" is disabled:  rig creds "+name+" <env-file>")
							} else {
								check("proxy plugin", true, proxy.PluginID+" "+p.Version)
							}
							check("proxied credentials", true, asked)
						}
					}
				}
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
				// Configured is not proven: ask the guest to fetch from one
				// proxied host and show whose certificate it saw. The proxy's
				// means the request went through it.
				if len(proxied) > 0 && proxyErr == nil {
					if proxyIP != "" {
						out, err := a.c.Exec(name, proxy.GuestNICProbe(proxyIP), incus.ExecOpts{Timeout: 30 * time.Second})
						check("proxy nic in guest", err == nil, lastLine(strings.TrimSpace(out)))
					}
					probed := map[string]bool{}
					for _, v := range sortedKeys(proxied) {
						e, err := manifest.ParseEndpoint(proxied[v][0])
						if err != nil || strings.HasPrefix(e.Host, "*.") {
							continue // a wildcard names no one host to fetch from
						}
						host := e.Host
						if probed[host] {
							continue
						}
						probed[host] = true
						out, err := a.c.Exec(name, proxy.GuestProbe(policy.ProxyURLFor(proxyIP), host), incus.ExecOpts{Timeout: 45 * time.Second})
						var exit *incus.ExitError
						if errors.As(err, &exit) && exit.Code == 2 {
							info("proxy for "+host, lastLine(out)+" (not proven; rebuild the image for curl)")
							continue
						}
						check("proxy for "+host, err == nil, lastLine(firstNonEmpty(strings.TrimSpace(out), "intercepted")))
					}
					// And what it does with a host the manifest does not
					// name: refuses it, or carries it when egress says so.
					if proxyIP != "" {
						internet := inst.Config[proxy.EgressKey] == manifest.EgressInternet
						label := "proxy refuses the rest"
						if internet {
							label = "proxy to the internet"
						}
						out, err := a.c.Exec(name, proxy.GuestEgressProbe(policy.ProxyURLFor(proxyIP), internet), incus.ExecOpts{Timeout: 45 * time.Second})
						var exit *incus.ExitError
						if errors.As(err, &exit) && exit.Code == 2 {
							info(label, lastLine(out)+" (not proven)")
						} else {
							check(label, err == nil, lastLine(firstNonEmpty(strings.TrimSpace(out), "ok")))
						}
					}
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
	var lines int
	var follow bool
	cmd := &cobra.Command{
		Use:     "logs <vm>",
		GroupID: "vm",
		Short:   "Console log, for when a VM never comes up",
		Long: "Prints what the VM wrote to its console: the boot, and anything after.\n" +
			"-n keeps the last lines; -f keeps printing as the console grows, until\n" +
			"interrupted.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			log, err := a.c.ConsoleLog(name)
			if err != nil {
				return err
			}
			out := tailLines(log, lines)
			fmt.Print(out)
			if !strings.HasSuffix(out, "\n") {
				fmt.Println()
			}
			if !follow {
				return nil
			}
			return followConsole(time.Second, log, func() (string, error) { return a.c.ConsoleLog(name) },
				func(s string) { fmt.Print(s) })
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 0, "only the last this many lines (default: all)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing as the console grows")
	return cmd
}

// --- working inside a guest ----------------------------------------------

func (a *app) execCmd() *cobra.Command {
	var dir string
	var stdin bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:     "exec [flags] <vm> -- <command>...",
		GroupID: "guest",
		Short:   "Run a command in the guest",
		Long: "Runs a command in the guest through a login shell, so the guest's own\n" +
			"PATH applies. The command goes after --, and nothing after it is rig's;\n" +
			"rig's flags go anywhere before it.\n\n" +
			"Several words arrive exactly as given, as with docker exec; one is a\n" +
			"shell script, for pipes, && and the like:\n" +
			"  rig exec myvm --dir /work/foo -- ls -la\n" +
			"  rig exec myvm -- 'cd /work/foo && git log --oneline | head'\n\n" +
			"stdin is not forwarded unless -i says so, as with docker exec: forwarded\n" +
			"by default, a script looping over its own input would lose it to the\n" +
			"guest. The guest command's exit status is rig's.",
		Args:                  cobra.MinimumNArgs(1),
		DisableFlagsInUseLine: true,
		RunE: func(c *cobra.Command, args []string) error {
			switch dash := c.ArgsLenAtDash(); {
			case dash < 0 && len(args) > 1:
				return fmt.Errorf("the guest's command goes after --, where rig reads nothing:\n"+
					"  rig exec %s -- %s", args[0], quoteForDisplay(args[1:]))
			case dash < 0 || len(args) == dash:
				return fmt.Errorf("no command; it goes after --:\n  rig exec %s -- <command>", args[0])
			case dash != 1:
				return fmt.Errorf("name one VM before --, and put the command after it:\n"+
					"  rig exec %s -- %s", args[0], quoteForDisplay(args[dash:]))
			}
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			_, err := a.c.Exec(args[0], guestCommand(args[1:]), incus.ExecOpts{
				Dir: dir, Stdin: stdin, Timeout: timeout, Streaming: true,
			})
			return err
		},
	}
	// Without --, the guest command's own flags reach rig's parser first.
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fmt.Errorf("%w\nThe guest's command goes after --, where rig reads nothing:\n"+
			"  rig exec <vm> -- <command>", err)
	})
	cmd.Flags().StringVar(&dir, "dir", "", "working directory in the guest")
	cmd.Flags().BoolVarP(&stdin, "stdin", "i", false, "forward stdin")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "kill the command after this long")
	return cmd
}

// guestCommand is what the guest's shell runs. One argument is a script, pipes
// and && included; several are a command and its arguments, quoted so each
// arrives as given.
func guestCommand(words []string) string {
	if len(words) == 1 {
		return words[0]
	}
	return shellQuote(words)
}

// quoteForDisplay renders words as they would be typed, quoting only those
// the shell would otherwise split or expand.
func quoteForDisplay(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = w
		if w == "" || strings.ContainsFunc(w, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,@%+", r))
		}) {
			out[i] = shellQuote([]string{w})
		}
	}
	return strings.Join(out, " ")
}

func (a *app) shellCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "sh <vm>",
		Aliases: []string{"shell"},
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

// --- helpers -------------------------------------------------------------

// confirm asks a yes-or-no question on the terminal; anything but yes is no.
// Without a terminal there is no one to ask, so it refuses and names the flag
// that answers in advance, rather than reading a script's stdin as consent.
func confirm(question, flag string) (bool, error) {
	if !isTerminal(os.Stdin) {
		return false, fmt.Errorf("there is no terminal to ask on; pass %s to answer yes in advance", flag)
	}
	fmt.Printf("%s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

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
		// gpu-check comes with rig.nixosModules.nvidia; an image without it
		// has no driver for the card either, which is the thing to say.
		return "command -v gpu-check >/dev/null || { echo 'no NVIDIA driver in this image: build it with rig.nixosModules.nvidia'; exit 1; }; gpu-check",
			"gpu visible in guest"
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
