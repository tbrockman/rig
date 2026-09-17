package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/gpu"
	"github.com/tbrockman/rig/internal/hostgpu"
)

// The host verbs. This was a second binary, `hostgpu`, because it is the only
// code here that touches the host itself and the only code that needs root.
// That is a real distinction and it is still visible — the verbs sit in the
// sharp group, and two of the three re-exec under sudo — but it was the wrong
// reason for a separate program: reclaiming the card is an ordinary step in the
// rig lifecycle, and a tool you have to remember exists separately is one you
// forget at the moment it matters.
//
// The split that remains is between privilege levels rather than binaries.
// Discovery, and the guard that refuses while a VM still holds the card, run
// unprivileged here; only sysfs writes and systemctl run as root, with every
// resolved value passed across as an explicit flag so the elevated command
// reads exactly as what it will do.
func (a *app) hostCmd() *cobra.Command {
	var dm, pci, audioPCI string

	cmd := &cobra.Command{
		Use:     "host",
		GroupID: "card",
		Short:   "Move the card between this host's desktop and VMs",
		Long: "For a host that also uses the card for its own desktop. Assumes another\n" +
			"GPU — an iGPU, typically — drives the console, so the card can leave for\n" +
			"a VM and come back without taking the only display with it.\n\n" +
			"`desktop` and `headless` write to sysfs and need root: they re-exec\n" +
			"themselves under sudo, printing the command first. `status` does not.\n\n" +
			"Reclaiming the card RESETS it before the driver loads, then checks a DRM\n" +
			"node appeared rather than trusting that the driver bound — without the\n" +
			"reset the desktop comes back with no display, and without the check rig\n" +
			"reports success anyway.\n\n" +
			"Environment: RIG_PCI, when discovery picks the wrong card.",
	}
	f := cmd.PersistentFlags()
	f.StringVar(&dm, "display-manager", "display-manager",
		"the desktop's display manager unit; the display-manager alias resolves to whichever one is enabled (gdm, sddm, lightdm)")
	f.StringVar(&pci, "pci", "", "card's PCI address (default: RIG_PCI, else discovered)")
	f.StringVar(&audioPCI, "audio-pci", "", "the card's audio function (default: the GPU's address at function 1)")

	// resolve fills in whatever was not given. Runs unprivileged, before any
	// elevation, so the root half never has to discover anything.
	resolve := func() (hostgpu.Conf, error) {
		conf := hostgpu.Conf{GPUPCI: pci, AudioPCI: audioPCI, DM: dm}
		if conf.GPUPCI == "" {
			found, err := gpu.DiscoverPCI(a.c, a.cfg)
			if err != nil {
				return conf, err
			}
			conf.GPUPCI = found
		}
		if conf.AudioPCI == "" {
			conf.AudioPCI = hostgpu.AudioFunction(conf.GPUPCI)
		}
		return conf, nil
	}

	cmd.AddCommand(
		&cobra.Command{
			Use: "status", Short: "Where the card is", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				conf, err := resolve()
				if err != nil {
					return err
				}
				conf.Status()
				return a.printCardHolders()
			},
		},
		&cobra.Command{
			Use:   "desktop",
			Short: "Reclaim the card for this host and start the desktop",
			Args:  cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				conf, err := resolve()
				if err != nil {
					return err
				}
				// Refuse while a VM still has the card configured: the rebind
				// would race Incus and both sides lose. Checked here, before
				// elevation, so the refusal costs no password prompt.
				if instances, err := a.c.Instances(); err == nil {
					if holders := gpu.Holders(instances); len(holders) > 0 {
						return fmt.Errorf("%s still holds the GPU. Run:  rig stop %s && rig release",
							holders[0].Instance, holders[0].Instance)
					}
				}
				if os.Geteuid() != 0 {
					return elevate("desktop", conf)
				}
				return conf.Desktop()
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
				conf, err := resolve()
				if err != nil {
					return err
				}
				// Before the stop, and before elevation: as the invoking user we
				// can read that user's own processes, which is exactly whose
				// session is about to end. After sudo it would be too late to
				// matter and no more accurate.
				if holders := hostgpu.Holders(); len(holders) > 0 {
					fmt.Fprintf(os.Stderr, "these hold the card and will lose it:\n")
					for _, h := range holders {
						fmt.Fprintf(os.Stderr, "  %s\n", h)
					}
				}
				if os.Geteuid() != 0 {
					return elevate("headless", conf)
				}
				return conf.Headless()
			},
		},
	)
	return cmd
}

// printCardHolders reports which instances have the card configured. Separate
// from `rig status` because this is the host's view of one question, not the
// fleet's view of everything.
func (a *app) printCardHolders() error {
	instances, err := a.c.Instances()
	if err != nil {
		fmt.Println("instances: (incus unreachable)")
		return nil
	}
	holders := gpu.Holders(instances)
	if len(holders) == 0 {
		fmt.Println("instances holding a gpu device: none")
		return nil
	}
	fmt.Println("instances holding a gpu device:")
	for _, h := range holders {
		fmt.Printf("  %s (%s)\n", h.Instance, h.Status)
	}
	return nil
}

// elevate re-execs this verb under sudo.
//
// Every resolved value crosses as an explicit flag rather than in the
// environment: sudo resets the environment by default, and --preserve-env is
// not guaranteed by every sudoers policy, so a discovered address would
// silently fail to arrive and the root half would rediscover it — possibly
// differently. Flags also make the printed command the whole truth about what
// is about to run as root.
func elevate(verb string, c hostgpu.Conf) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find my own path to re-exec under sudo: %w", err)
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("`rig host %s` writes to sysfs and needs root, but sudo is not on PATH.\n"+
			"  Run it as root:  %s host %s --pci=%s --audio-pci=%s --display-manager=%s",
			verb, exe, verb, c.GPUPCI, c.AudioPCI, c.DM)
	}

	argv := []string{exe, "host", verb,
		"--pci=" + c.GPUPCI,
		"--audio-pci=" + c.AudioPCI,
		"--display-manager=" + c.DM,
	}
	fmt.Fprintf(os.Stderr, "rig: this needs root. running: sudo %s\n", strings.Join(argv, " "))

	sudo := exec.Command("sudo", append([]string{"--"}, argv...)...)
	sudo.Stdin, sudo.Stdout, sudo.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := sudo.Run(); err != nil {
		// The child already said why. Carry its status out rather than wrapping
		// a second, vaguer message around it.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &exitCodeError{code: ee.ExitCode(), msg: "sudo rig host " + verb}
		}
		return err
	}
	return nil
}
