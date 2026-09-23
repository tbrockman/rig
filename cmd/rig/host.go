package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/devices"
	"github.com/tbrockman/rig/internal/hostdev"
)

// The host verbs. This was a second binary, `hostgpu`, because it is the only
// code here that touches the host itself and the only code that needs root.
// That is a real distinction and it is still visible — the verbs sit in the
// sharp group, and most re-exec under sudo — but it was the wrong reason for
// a separate program: reclaiming a device is an ordinary step in the rig
// lifecycle, and a tool you have to remember exists separately is one you
// forget at the moment it matters.
//
// `desktop` and `headless` are the card-and-display-manager case, spelled the
// way it has always been. `return` and `free` are the generic routine those
// two are made of, driven by a device's recorded recipe; `rig stop` and
// `rig start` run them for every device a VM's manifest declares.
//
// The split that remains is between privilege levels rather than binaries.
// Discovery, and the guard that refuses while a VM still holds the device,
// run unprivileged here; only sysfs writes and systemctl run as root, with
// every resolved value passed across as an explicit flag so the elevated
// command reads exactly as what it will do.
func (a *app) hostCmd() *cobra.Command {
	var dm, pci string

	cmd := &cobra.Command{
		Use:     "host",
		GroupID: "card",
		Short:   "Move devices and input between this host and VMs",
		Long: "The verbs that touch this host rather than a guest. `return` and `free`\n" +
			"are what rig stop and rig start run for each device a manifest declares.\n" +
			"`desktop`, `headless` and `status` are for a host whose own desktop uses\n" +
			"the NVIDIA card a VM is given, with another GPU (an iGPU, typically)\n" +
			"driving the console. `input` lends the keyboard and mouse.\n\n" +
			"`desktop`, `headless`, `return` and `free` write to sysfs or stop units\n" +
			"and need root: they re-exec themselves under sudo, printing the command\n" +
			"first. `status` does not.\n\n" +
			"Returning a device RESETS it before the driver loads, then checks the\n" +
			"recipe's sign of life appeared rather than trusting that the driver\n" +
			"bound — without the reset a display comes back dark, and without the\n" +
			"check rig reports success anyway.\n\n" +
			"Environment: RIG_PCI, when discovery picks the wrong card.",
	}
	f := cmd.PersistentFlags()
	f.StringVar(&dm, "display-manager", "display-manager",
		"the desktop's display manager unit; the display-manager alias resolves to whichever one is enabled (gdm, sddm, lightdm)")
	f.StringVar(&pci, "pci", "", "the card's PCI address (default: RIG_PCI, else discovered)")

	// card resolves the desktop case: the NVIDIA recipe on the discovered
	// card. Runs unprivileged, before any elevation, so the root half never
	// has to discover anything.
	card := func() (hostdev.Spec, error) {
		addr := pci
		if addr == "" {
			found, err := devices.DiscoverPCI(a.c, a.cfg)
			if err != nil {
				return hostdev.Spec{}, err
			}
			addr = found
		}
		return spec(addr, devices.NvidiaReturn(dm)), nil
	}

	// held refuses while any instance still has the device configured: the
	// rebind would race Incus and both sides lose. Checked before elevation,
	// so the refusal costs no password prompt.
	held := func(addr string) error {
		instances, err := a.c.Instances()
		if err != nil {
			return nil
		}
		for _, h := range devices.Holders(instances) {
			if h.PCI == addr {
				return fmt.Errorf("%s still holds %s. Run:  rig stop %s   (or rig release)",
					h.Instance, addr, h.Instance)
			}
		}
		return nil
	}

	var retSpec hostdev.Spec
	var retModules string
	ret := &cobra.Command{
		Use:   "return",
		Short: "Hand one device back to the host by its recipe (what rig stop runs)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if retSpec.PCI == "" {
				return errors.New("--pci is required")
			}
			if retModules != "" {
				retSpec.Modules = strings.Split(retModules, ",")
			}
			if err := held(retSpec.PCI); err != nil {
				return err
			}
			if os.Geteuid() != 0 {
				return elevate(returnArgs(retSpec))
			}
			return retSpec.Return()
		},
	}
	ret.Flags().StringVar(&retSpec.PCI, "pci", "", "device to return")
	ret.Flags().StringVar(&retModules, "modules", "", "kernel modules to cycle, comma-separated, unload order")
	ret.Flags().BoolVar(&retSpec.Reset, "reset", false, "function-level reset before the host driver binds")
	ret.Flags().StringVar(&retSpec.Alive, "alive", "", "sysfs pattern under the device that proves it came up")
	ret.Flags().StringVar(&retSpec.Unit, "unit", "", "host unit to start afterwards")

	var freeSpec hostdev.Spec
	var freeModules string
	free := &cobra.Command{
		Use:   "free",
		Short: "Stop the host unit that uses a device, so a VM can claim it (what rig start runs)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if freeSpec.PCI == "" || freeSpec.Unit == "" {
				return errors.New("--pci and --unit are required")
			}
			if freeModules != "" {
				freeSpec.Modules = strings.Split(freeModules, ",")
			}
			if os.Geteuid() != 0 {
				return elevate(freeArgs(freeSpec))
			}
			return freeSpec.Free()
		},
	}
	free.Flags().StringVar(&freeSpec.PCI, "pci", "", "device to free")
	free.Flags().StringVar(&freeSpec.Unit, "unit", "", "host unit to stop")
	free.Flags().StringVar(&freeModules, "modules", "", "kernel modules the host driver uses, for naming what holds the device")

	cmd.AddCommand(a.hostInputCmd())
	cmd.AddCommand(
		&cobra.Command{
			Use: "status", Short: "Where the NVIDIA card is, and which VMs hold devices", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				s, err := card()
				if err != nil {
					return err
				}
				s.Status()
				return a.printHolders()
			},
		},
		&cobra.Command{
			Use:   "desktop",
			Short: "Reclaim the card for this host and start the desktop",
			Args:  cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				s, err := card()
				if err != nil {
					return err
				}
				if err := held(s.PCI); err != nil {
					return err
				}
				if os.Geteuid() != 0 {
					return elevate(returnArgs(s))
				}
				return s.Return()
			},
		},
		&cobra.Command{
			Use:   "headless",
			Short: "Stop the desktop and free the card for VMs",
			Long: "Stops the display manager and leaves the card free for a VM to claim.\n\n" +
				"This ends the session on the desktop, not just its display. Anything\n" +
				"holding the card is listed first, because being told which session was\n" +
				"killed only after it is gone is not a warning.",
			Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				s, err := card()
				if err != nil {
					return err
				}
				// Before the stop, and before elevation: as the invoking user we
				// can read that user's own processes, which is exactly whose
				// session is about to end. After sudo it would be too late to
				// matter and no more accurate.
				if holders := s.Holders(); len(holders) > 0 {
					fmt.Fprintf(os.Stderr, "these hold the card and will lose it:\n")
					for _, h := range holders {
						fmt.Fprintf(os.Stderr, "  %s\n", h)
					}
				}
				if os.Geteuid() != 0 {
					return elevate(freeArgs(s))
				}
				return s.Free()
			},
		},
		ret, free,
	)
	return cmd
}

// printHolders reports which instances have passthrough devices configured.
// Separate from `rig status` because this is the host's view of one question,
// not the fleet's view of everything.
func (a *app) printHolders() error {
	instances, err := a.c.Instances()
	if err != nil {
		fmt.Println("instances: (incus unreachable)")
		return nil
	}
	holders := devices.Holders(instances)
	if len(holders) == 0 {
		fmt.Println("instances holding a passthrough device: none")
		return nil
	}
	fmt.Println("instances holding a passthrough device:")
	for _, h := range holders {
		fmt.Printf("  %s (%s): %s %s %s\n", h.Instance, h.Status, h.Device, h.Kind, h.PCI)
	}
	return nil
}

// returnArgs and freeArgs render a spec as the argv of the root half, so the
// printed sudo line is the whole truth about what is about to run as root.
func returnArgs(s hostdev.Spec) []string {
	args := []string{"host", "return", "--pci=" + s.PCI}
	if len(s.Modules) > 0 {
		args = append(args, "--modules="+strings.Join(s.Modules, ","))
	}
	if s.Reset {
		args = append(args, "--reset")
	}
	if s.Alive != "" {
		args = append(args, "--alive="+s.Alive)
	}
	if s.Unit != "" {
		args = append(args, "--unit="+s.Unit)
	}
	return args
}

func freeArgs(s hostdev.Spec) []string {
	args := []string{"host", "free", "--pci=" + s.PCI, "--unit=" + s.Unit}
	if len(s.Modules) > 0 {
		args = append(args, "--modules="+strings.Join(s.Modules, ","))
	}
	return args
}

// elevate re-execs a verb under sudo.
//
// Every resolved value crosses as an explicit flag rather than in the
// environment: sudo resets the environment by default, and --preserve-env is
// not guaranteed by every sudoers policy, so a discovered address would
// silently fail to arrive and the root half would rediscover it — possibly
// differently. Flags also make the printed command the whole truth about what
// is about to run as root.
func elevate(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find my own path to re-exec under sudo: %w", err)
	}
	argv := append([]string{exe}, args...)
	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("`rig %s` needs root, but sudo is not on PATH.\n"+
			"  Run it as root:  %s", strings.Join(args[:2], " "), strings.Join(argv, " "))
	}
	fmt.Fprintf(os.Stderr, "rig: this needs root. running: sudo %s\n", strings.Join(argv, " "))

	sudo := exec.Command("sudo", append([]string{"--"}, argv...)...)
	sudo.Stdin, sudo.Stdout, sudo.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := sudo.Run(); err != nil {
		// The child already said why. Carry its status out rather than wrapping
		// a second, vaguer message around it.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &exitCodeError{code: ee.ExitCode(), msg: "sudo " + strings.Join(argv, " ")}
		}
		return err
	}
	return nil
}
