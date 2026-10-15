package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/devices"
	"github.com/tbrockman/rig/internal/incus"
)

// The handback watcher. rig stop hands a VM's devices back to the host, but a
// VM also stops without rig: the guest powers itself off, or someone runs
// incus stop. Incus then rebinds each device to the driver it had before the
// VM started, with no reset and no module reload, which for a card is the
// dead adapter DESIGN.md describes among Incus's known behaviours; and a unit
// rig start stopped, a display manager most often, stays stopped. So rig
// start leaves a watcher: a transient root unit, like input's, that waits for
// the VM to stop and then does what rig stop would have.

// handbackUnit is the watcher's transient unit. Transient on purpose: nothing
// is installed, and a host reboot, which ends every VM, ends it too.
func handbackUnit(vm string) string { return "rig-handback-" + vm + ".service" }

// handbackActive reports whether vm's watcher is running. Reading a unit's
// state needs no privilege.
func handbackActive(vm string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", handbackUnit(vm)).Run() == nil
}

func (a *app) hostHandbackCmd() *cobra.Command {
	var background, stopIt bool
	var lock string
	cmd := &cobra.Command{
		Use:   "handback <vm>",
		Short: "Hand a VM's devices back to the host when it stops without rig stop (what rig start runs)",
		Long: "Waits for the VM to stop and, unless rig stop stopped it, does what rig stop\n" +
			"would have: detaches its devices and returns each one that has a recipe,\n" +
			"resetting a card and starting the recipe's unit. That covers a guest that\n" +
			"powers itself off, and a plain incus stop. A reboot, from inside or incus\n" +
			"restart, is not a stop.\n\n" +
			"rig start runs it with --background, as the transient unit\n" +
			"rig-handback-<vm>, for a VM whose devices have a return: recipe. It ends\n" +
			"when the VM stops; --stop ends it sooner. Its log:\n" +
			"  journalctl -u rig-handback-<vm>\n\n" +
			"Needs root to return devices, so it re-execs under sudo.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if os.Geteuid() != 0 {
				if stopIt {
					if !handbackActive(name) {
						note("no handback watcher is running for %s", name)
						return nil
					}
					return elevate([]string{"host", "handback", name, "--stop"})
				}
				if err := a.requireInstance(name); err != nil {
					return err
				}
				return elevate(handbackArgs(name, a.cfg.Lock, background))
			}
			if lock != "" {
				a.cfg.Lock = lock
			}
			switch {
			case stopIt:
				return exec.Command("systemctl", "stop", handbackUnit(name)).Run()
			case background:
				return startHandbackUnit(name, a.cfg.Lock)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return a.watchHandback(ctx, name)
		},
	}
	cmd.Flags().BoolVar(&background, "background", false,
		"run as a transient systemd unit that ends when the VM stops, instead of in this terminal")
	cmd.Flags().BoolVar(&stopIt, "stop", false, "end the background watcher for this VM")
	cmd.Flags().StringVar(&lock, "lock", "", "the rig lock, when RIG_LOCK moved it (passed across sudo)")
	_ = cmd.Flags().MarkHidden("lock")
	return cmd
}

// handbackArgs renders the root half's argv. The lock crosses as a flag, like
// every resolved value, because sudo drops RIG_LOCK from the environment.
func handbackArgs(vm, lock string, background bool) []string {
	args := []string{"host", "handback", vm}
	if lock != devices.DefaultLock {
		args = append(args, "--lock="+lock)
	}
	if background {
		args = append(args, "--background")
	}
	return args
}

// startHandbackUnit is the root half of --background. One left from before is
// stopped first, so a VM never has two.
//
// Ordered after Incus's units so that at shutdown it stops before Incus stops
// the VMs, rather than seeing those stops and returning devices to a host
// that is going down.
func startHandbackUnit(vm, lock string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find my own path for the unit to run: %w", err)
	}
	unit := handbackUnit(vm)
	_ = exec.Command("systemctl", "stop", unit).Run()
	_ = exec.Command("systemctl", "reset-failed", unit).Run()
	argv := []string{"--unit", unit, "--collect", "--quiet",
		"--description", "rig: hand " + vm + "'s devices back to this host if it stops without rig stop",
		"-p", "After=incus.service", "-p", "After=incus-startup.service",
		exe}
	argv = append(argv, handbackArgs(vm, lock, false)...)
	if out, err := exec.Command("systemd-run", argv...).CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	note("if %s stops without rig stop, %s hands its devices back; its log: journalctl -u %s", vm, unit, unit)
	return nil
}

// startHandback is rig start's half: leave a watcher for a VM whose devices
// have a recipe. Without one a device stays parked on vfio-pci whoever stops
// the VM, so there is nothing to watch for.
func (a *app) startHandback(name string, wanted []devices.Decl) {
	recipe := false
	for _, d := range wanted {
		recipe = recipe || d.Return != nil
	}
	if !recipe || handbackActive(name) {
		return
	}
	var err error
	if os.Geteuid() == 0 {
		err = startHandbackUnit(name, a.cfg.Lock)
	} else {
		err = elevate(handbackArgs(name, a.cfg.Lock, true))
	}
	if err != nil {
		note("WARNING: if %s stops without rig stop, its devices will not come back by themselves: %v", name, err)
		note("         rig stop %s hands them back. To watch anyway:  rig host handback %s --background", name, name)
	}
}

// handBack detaches every device an instance holds and returns each to the
// host by its recipe: rig stop's second half, and the watcher's whole job.
func (a *app) handBack(name string) error {
	detached, err := devices.Detach(a.c, a.cfg, name, false)
	if err != nil {
		return err
	}
	return returnDetached(detached)
}

// watchHandback waits for name to stop and hands its devices back, unless rig
// stopped it.
func (a *app) watchHandback(ctx context.Context, name string) error {
	note("watching %s: if it stops without rig stop, its devices come back to this host", name)
	if !awaitStop(ctx, a.c, name, 2*time.Second) {
		return nil
	}
	inst, _, err := a.c.Instance(name)
	if err != nil {
		return err
	}
	if inst.Config[devices.StopKey] != "" {
		note("%s was stopped by rig; handing its devices back is rig stop's job", name)
		return nil
	}
	note("%s stopped without rig stop; handing its devices back", name)
	return a.handBack(name)
}

// stopWatch is what the watcher asks Incus: an interface so that its rules
// can be tested against a script instead of a daemon.
type stopWatch interface {
	Instance(name string) (*incus.Instance, string, error)
	Busy(name string) (bool, error)
	LifecycleEvents(ctx context.Context) (<-chan incus.Lifecycle, error)
}

// awaitStop blocks until name has stopped, and reports whether it did: false
// means ctx ended or the instance is gone.
//
// Incus's stop events decide it, not the status. A VM rebooting from inside
// reads as Stopped until it is running again, and an incus restart does too
// between its halves. A reboot sends no stop event; an incus restart sends
// instance-stopped but keeps an operation running on the instance until it
// is back up.
func awaitStop(ctx context.Context, w stopWatch, name string, poll time.Duration) bool {
	for ctx.Err() == nil {
		subCtx, cancel := context.WithCancel(ctx)
		events, err := w.LifecycleEvents(subCtx)
		if err != nil {
			cancel()
			note("cannot follow Incus's events (%v); trying again", err)
			pause(ctx, 5*poll)
			continue
		}
		// A stop while nothing was listening is only in the state, where a
		// reboot looks the same; so a stop found this way has to last.
		switch readState(w, name) {
		case stateGone:
			cancel()
			return false
		case stateStopped:
			if lasts(ctx, w, name, poll, 15) {
				cancel()
				return true
			}
		}
		for ev := range events {
			if ev.Instance() != name {
				continue
			}
			switch ev.Action {
			case "instance-deleted":
				cancel()
				return false
			case "instance-shutdown", "instance-stopped":
				switch settle(ctx, w, name, poll) {
				case stateStopped:
					cancel()
					return true
				case stateGone:
					cancel()
					return false
				}
			}
		}
		// The stream ended: Incus restarting, most likely.
		cancel()
		pause(ctx, poll)
	}
	return false
}

// vmState is what one look at an instance can tell the watcher.
type vmState int

const (
	stateUnknown vmState = iota // unreadable, or an operation is under way on it
	stateStopped                // Stopped, with nothing under way
	stateRunning                // Running, with nothing under way
	stateGone                   // deleted
)

func readState(w stopWatch, name string) vmState {
	inst, _, err := w.Instance(name)
	var apiErr *incus.APIError
	if errors.As(err, &apiErr) && apiErr.NotFound() {
		return stateGone
	}
	if err != nil {
		return stateUnknown
	}
	if busy, err := w.Busy(name); err != nil || busy {
		return stateUnknown
	}
	switch {
	case inst.Stopped():
		return stateStopped
	case inst.Running():
		return stateRunning
	}
	return stateUnknown
}

// lasts reports whether name stays stopped for n polls.
func lasts(ctx context.Context, w stopWatch, name string, poll time.Duration, n int) bool {
	for i := 0; i < n; i++ {
		if pause(ctx, poll); ctx.Err() != nil || readState(w, name) != stateStopped {
			return false
		}
	}
	return true
}

// settle follows a stop event to its end: stopped, gone, or running again.
// The event comes a moment before Incus reports the instance Stopped, and an
// incus restart sends one too, so running counts only once it has lasted a
// few polls with nothing under way.
func settle(ctx context.Context, w stopWatch, name string, poll time.Duration) vmState {
	running := 0
	for ctx.Err() == nil {
		switch s := readState(w, name); s {
		case stateStopped, stateGone:
			return s
		case stateRunning:
			if running++; running >= 3 {
				return s
			}
		default:
			running = 0
		}
		pause(ctx, poll)
	}
	return stateUnknown
}

func pause(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
