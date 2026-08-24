// Command rig creates and runs isolated project VMs with the GPU attached.
//
// This is the whole routine surface, covering the full lifecycle. The verbs that
// can break an invariant — release, --force detach, apply, starting a VM with no
// network isolation — live in gpuctl instead, and are not reachable from here.
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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"rig/internal/creds"
	"rig/internal/gpu"
	"rig/internal/incus"
	"rig/internal/policy"
)

// managedKey marks instances rig created. rm refuses anything without it, so it
// cannot delete something built by hand.
const managedKey = "user.rig.managed"

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,61}$`)

type app struct {
	c   *incus.Client
	cfg gpu.Config
}

func main() {
	a := &app{c: incus.New(""), cfg: gpu.ConfigFromEnv()}

	root := &cobra.Command{
		Use:   "rig",
		Short: "Isolated project VMs with the GPU attached",
		Long: "rig creates and runs isolated project VMs with the GPU attached.\n\n" +
			"Privileged verbs — release, apply, forced detach, starting an unisolated\n" +
			"VM — live in gpuctl, not here.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		a.newCmd(), a.startCmd(), a.stopCmd(), a.restartCmd(), a.statusCmd(),
		a.claimCmd(), a.rmCmd(), a.doctorCmd(),
		a.execCmd(), a.shellCmd(), a.pushCmd(), a.pullCmd(), a.logsCmd(),
	)

	if err := root.Execute(); err != nil {
		var exit *incus.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code) // propagate the guest command's status
		}
		fmt.Fprintf(os.Stderr, "rig: %v\n", err)
		os.Exit(1)
	}
}

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

func (a *app) newCmd() *cobra.Command {
	var (
		envFile, image, memory, disk string
		cpus                         int
		start                        bool
	)
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a project VM (isolated, GPU-ready)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if !nameRE.MatchString(name) {
				return fmt.Errorf("not a valid instance name: %s", name)
			}
			if a.c.Exists(name) {
				return fmt.Errorf("%s already exists", name)
			}
			if !a.c.ImageExists(image) {
				return fmt.Errorf("no such image: %s (build it with ./01-build-image.sh)", image)
			}

			var absEnv string
			if envFile != "" {
				var err error
				if absEnv, err = filepath.Abs(envFile); err != nil {
					return err
				}
				if err := creds.Validate(absEnv); err != nil {
					return err
				}
			}

			config := map[string]string{managedKey: "true"}
			if absEnv != "" {
				config[creds.InstanceKey] = absEnv
			}
			if err := a.c.CreateVM(incus.CreateOpts{
				Name: name, Image: image, CPUs: cpus, Memory: memory,
				DiskSize: disk, Config: config,
			}); err != nil {
				return err
			}

			// Isolation is inherited from the default profile. Verify it landed
			// rather than assuming: a new VM with no ACL is the failure this
			// project exists to prevent, and it is silent.
			inst, _, err := a.c.Instance(name)
			if err != nil {
				return err
			}
			if !policy.Isolated(inst, a.cfg.ACL) {
				note("WARNING: %s did NOT inherit network isolation.", name)
				note("         Fix the profile:  gpuctl apply")
				note("         rig start will refuse it until then.")
			}

			note("created %s (image %s, %d cpus, %s, %s disk)", name, image, cpus, memory, disk)
			if absEnv != "" {
				note("credentials will be injected from %s", absEnv)
			}
			if start {
				return a.startInstance(name, 3*time.Minute, true)
			}
			note("next:  rig start %s", name)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&envFile, "env", "", "host file of KEY=VALUE credentials to inject on start")
	f.StringVar(&image, "image", envOr("RIG_IMAGE", "nixos-gpu-base"), "base image alias")
	f.IntVar(&cpus, "cpus", envInt("RIG_CPUS", 8), "vCPUs")
	f.StringVar(&memory, "memory", envOr("RIG_MEMORY", "16GiB"), "RAM")
	f.StringVar(&disk, "disk", envOr("RIG_DISK", "40GiB"), "root disk size")
	f.BoolVar(&start, "start", false, "start it once created")
	return cmd
}

func (a *app) startCmd() *cobra.Command {
	var timeout time.Duration
	var noWait bool
	cmd := &cobra.Command{
		Use:   "start <name>",
		Short: "Claim the card, start the VM, inject credentials",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			return a.startInstance(args[0], timeout, !noWait)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Minute, "how long to wait for the guest to come up")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return as soon as Incus reports it started")
	return cmd
}

func (a *app) startInstance(name string, timeout time.Duration, wait bool) error {
	if err := gpu.Start(a.c, a.cfg, name, false, int(timeout.Seconds())); err != nil {
		return err
	}
	if !wait {
		return nil
	}
	if err := a.c.WaitAgent(name, timeout); err != nil {
		note("WARNING: %v", err)
		note("         Look at:  rig logs %s", name)
		return err
	}
	// Wait for an address too: the agent answers several seconds before DHCP
	// finishes, and anything touching the network in that window fails oddly.
	addr, err := a.c.WaitAddress(name, timeout)
	if err != nil {
		note("WARNING: %v", err)
	} else {
		note("up at %s", addr)
	}

	inst, _, err := a.c.Instance(name)
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

func (a *app) stopCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop the VM (the card stays attached to it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			return gpu.Stop(a.c, a.cfg, args[0], int(timeout.Seconds()))
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "how long to allow for shutdown")
	return cmd
}

func (a *app) restartCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "restart <name>",
		Short: "Stop and start, re-injecting credentials",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			if err := gpu.Stop(a.c, a.cfg, args[0], int(timeout.Seconds())); err != nil {
				return err
			}
			return a.startInstance(args[0], timeout, true)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Minute, "how long to wait for the guest to come up")
	return cmd
}

func (a *app) claimCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "claim <name>",
		Short: "Move the card to a stopped VM without starting it",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			return gpu.Claim(a.c, a.cfg, args[0])
		},
	}
}

func (a *app) rmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a stopped VM that rig created",
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
			if inst.Config[managedKey] != "true" {
				return fmt.Errorf("%s was not created by rig (no %s marker).\n"+
					"Delete it deliberately with incus if that is what you want.", name, managedKey)
			}
			if !inst.Stopped() {
				if !force {
					return fmt.Errorf("%s is %s. Stop it first, or pass --force.",
						name, strings.ToLower(inst.Status))
				}
				if err := gpu.Stop(a.c, a.cfg, name, 120); err != nil {
					return err
				}
			}
			note("deleting %s and its disk (including /work)", name)
			if err := a.c.DeleteInstance(name); err != nil {
				return err
			}
			note("deleted %s", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop it first if it is running")
	return cmd
}

// --- inspection ----------------------------------------------------------

func (a *app) statusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Who holds the card, and which VMs exist",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			instances, err := a.c.Instances()
			if err != nil {
				return err
			}
			if err := gpu.CheckProfiles(a.c, instances); err != nil {
				return err
			}
			holders := gpu.Holders(instances)

			type row struct {
				Name     string `json:"name"`
				Status   string `json:"status"`
				HasGPU   bool   `json:"has_gpu"`
				Isolated bool   `json:"isolated"`
				Managed  bool   `json:"managed"`
			}
			rows := []row{}
			for i := range instances {
				inst := &instances[i]
				hasGPU := false
				for _, h := range holders {
					if h.Instance == inst.Name {
						hasGPU = true
					}
				}
				unisolated, _ := policy.Report(inst, a.cfg.ACL)
				rows = append(rows, row{inst.Name, inst.Status, hasGPU,
					len(unisolated) == 0, inst.Config[managedKey] == "true"})
			}

			if asJSON {
				if holders == nil {
					holders = []gpu.Holder{}
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"holders": holders, "instances": rows})
			}

			switch len(holders) {
			case 0:
				fmt.Println("GPU is unassigned.")
			case 1:
				fmt.Printf("GPU held by %s (%s) at %s\n", holders[0].Instance, holders[0].Status, holders[0].PCI)
			default:
				fmt.Println("!! GPU device is configured on MULTIPLE instances — starting a second")
				fmt.Println("   one will hot-unplug the card from the first. Fix with `gpuctl release`.")
			}
			fmt.Println()
			for _, r := range rows {
				mark := " "
				if r.HasGPU {
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
	return &cobra.Command{
		Use:   "doctor <name>",
		Short: "Check a VM is what you think it is",
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

			failed := 0
			check := func(label string, ok bool, detail string) {
				status := "ok  "
				if !ok {
					status = "FAIL"
					failed++
				}
				fmt.Printf("  %s  %-22s %s\n", status, label, detail)
			}

			unisolated, noEgress := policy.Report(inst, a.cfg.ACL)
			check("network isolation", len(unisolated) == 0, isolationDetail(a.cfg.ACL, unisolated, noEgress))

			gpus := inst.GPUDevices()
			gpuDetail := "no GPU device configured"
			for _, dev := range gpus {
				gpuDetail = dev["pci"]
			}
			check("gpu device", len(gpus) == 1, gpuDetail)

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
				out, err := a.c.Exec(name, "gpu-check", incus.ExecOpts{Timeout: 30 * time.Second})
				check("gpu visible in guest", err == nil, lastLine(out))

				if inst.Config[creds.InstanceKey] != "" {
					_, err := a.c.Exec(name, "test -r "+creds.GuestPath, incus.ExecOpts{Timeout: 15 * time.Second})
					check("credentials", err == nil, creds.GuestPath)
				}
			}

			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			return nil
		},
	}
}

func (a *app) logsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logs <name>",
		Short: "Console log, for when a VM never comes up",
		Args:  cobra.ExactArgs(1),
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
		Use:   "exec <name> <command>...",
		Short: "Run a command in the guest",
		Long: "Run a command in the guest through a login shell, so the guest's own\n" +
			"PATH applies.\n\n" +
			"rig's own flags must come before the instance name, because everything\n" +
			"after it belongs to the guest command:\n" +
			"  rig exec --dir /work/foo myvm ls -la",
		Args:               cobra.MinimumNArgs(2),
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
		Use:   "shell <name>",
		Short: "Interactive login shell in the guest",
		Args:  cobra.ExactArgs(1),
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
	cmd := &cobra.Command{
		Use:   "push <name> <src-dir>",
		Short: "Copy a host directory's contents into the guest",
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
			n, err := a.c.PushDir(name, src, target)
			if err != nil {
				return err
			}
			note("pushed %d file(s) to %s:%s", n, name, target)
			return nil
		},
	}
	cmd.Flags().StringVar(&dest, "dest", "", "guest directory (default /work/<source basename>)")
	return cmd
}

func (a *app) pullCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "pull <name> <guest-path>",
		Short: "Read a file out of the guest",
		Args:  cobra.ExactArgs(2),
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
			return os.WriteFile(out, content, 0o644)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write to this file instead of stdout")
	return cmd
}

// --- helpers -------------------------------------------------------------

func isolationDetail(acl string, unisolated, noEgress []string) string {
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
