package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/input"
	"github.com/tbrockman/rig/internal/manifest"
)

// inputKey records guest.input on the instance. "host" has rig start lend
// the guest this host's keyboard and mouse for as long as it runs, so nobody
// has to remember `rig host input` after every start.
const inputKey = "user.rig.input"

// inputUnit is the transient systemd unit forwarding input to a VM in the
// background. Transient on purpose: nothing is installed, and a host reboot,
// which ends every VM, ends it too.
func inputUnit(vm string) string { return "rig-input-" + vm + ".service" }

// inputActive reports whether vm's background forwarder is running. Reading
// a unit's state needs no privilege.
func inputActive(vm string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", inputUnit(vm)).Run() == nil
}

// lentTo names the VMs whose background forwarder is running.
func lentTo() []string {
	out, _ := exec.Command("systemctl", "list-units", "--plain", "--no-legend",
		"--state=active", "rig-input-*.service").Output()
	return inputUnitVMs(string(out))
}

// inputUnitVMs reads the VM names out of `systemctl list-units --plain
// --no-legend` output.
func inputUnitVMs(listing string) []string {
	var vms []string
	for _, line := range strings.Split(listing, "\n") {
		unit, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if vm, ok := strings.CutPrefix(unit, "rig-input-"); ok {
			vms = append(vms, strings.TrimSuffix(vm, ".service"))
		}
	}
	return vms
}

// hostInputCmd forwards this host's keyboard and mouse into a guest. The
// unprivileged half resolves the VM to its vsock context ID and checks the
// guest is listening; only the forwarding itself, which has to hold the
// host's input devices, runs as root. See internal/input for why this is
// not QEMU's own evdev forwarding or a device passed by identity.
func (a *app) hostInputCmd() *cobra.Command {
	var cid uint32
	var devs []string
	var background, stopIt, follow bool
	cmd := &cobra.Command{
		Use:   "input <vm>",
		Short: "Lend this host's keyboard and mouse to a running guest, toggled with both Ctrl keys",
		Long: "Forwards this host's keyboards and mice into a running guest over vsock, as\n" +
			"events: the guest gets a virtual keyboard and mouse and never the devices,\n" +
			"whose vendor configuration interfaces a guest could otherwise program.\n\n" +
			"Input starts with the host. Press both Ctrl keys together and release them\n" +
			"to move it to the guest (Scroll Lock lights, where the keyboard has one);\n" +
			"the same again brings it back. While with the guest the devices are\n" +
			"grabbed and the host sees nothing. If the guest stops reading, input\n" +
			"falls back to the host on its own. Ctrl-C ends it.\n\n" +
			"--background runs it as a transient systemd unit, rig-input-<vm>, that\n" +
			"ends by itself when the VM stops; --stop ends it sooner. A manifest's\n" +
			"guest.input: host has rig start do that, so this is rarely typed. Input\n" +
			"goes to one VM at a time: a second forwarder is refused while one runs.\n\n" +
			"Needs root to hold the devices, so it re-execs under sudo. The guest needs\n" +
			"rig-input, which rig's desktop module runs (rig.desktop.input.enable).",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if os.Geteuid() != 0 {
				if stopIt {
					if !inputActive(name) {
						note("no input forwarder is running for %s", name)
						return nil
					}
					return elevate([]string{"host", "input", name, "--stop"})
				}
				elevated, err := a.inputArgs(name, devs, background)
				if err != nil {
					return err
				}
				if background {
					elevated = append(elevated, "--background")
				}
				return elevate(elevated)
			}
			switch {
			case stopIt:
				return exec.Command("systemctl", "stop", inputUnit(name)).Run()
			case cid == 0:
				return errors.New("--cid is required when run as root; run it as yourself and it is looked up")
			case background:
				return startInputUnit(name, cid, devs)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if follow {
				if !a.isLentVM(name, cid) {
					note("%s is not running as the VM input was meant for; nothing to do", name)
					return nil
				}
				ctx = a.followVM(ctx, name, cid)
			}
			return input.Run(ctx, input.Config{VM: name, CID: cid, Devices: devs}, func(format string, v ...any) {
				note(format, v...)
			})
		},
	}
	cmd.Flags().BoolVar(&background, "background", false,
		"run as a transient systemd unit that ends when the VM stops, instead of in this terminal")
	cmd.Flags().BoolVar(&stopIt, "stop", false, "end the background forwarder for this VM")
	cmd.Flags().Uint32Var(&cid, "cid", 0, "the guest's vsock context ID (looked up before elevating)")
	_ = cmd.Flags().MarkHidden("cid")
	cmd.Flags().BoolVar(&follow, "follow", false, "exit when the VM stops (what the background unit runs)")
	_ = cmd.Flags().MarkHidden("follow")
	cmd.Flags().StringArrayVar(&devs, "device", nil,
		"an input device to forward, by /dev/input path or by-id link; repeatable (default: every keyboard and mouse)")
	return cmd
}

// inputArgs checks, unprivileged, that input can go to vm now, and returns
// the arguments the root half runs with. Two forwarders would both grab the
// keyboard and both answer the Ctrl toggle, so one already running is
// refused; a background one for this same VM is replaced, when replacing is
// what was asked for.
func (a *app) inputArgs(name string, devs []string, background bool) ([]string, error) {
	if err := a.requireInstance(name); err != nil {
		return nil, err
	}
	inst, _, err := a.c.Instance(name)
	if err != nil {
		return nil, err
	}
	if !inst.Running() {
		return nil, fmt.Errorf("%s is %s; input goes to a running guest", name, strings.ToLower(inst.Status))
	}
	for _, vm := range lentTo() {
		if vm == name && background {
			continue
		}
		return nil, fmt.Errorf("input is already lent to %s in the background.\n  End that first:  rig host input %s --stop", vm, vm)
	}
	if pids := terminalForwarders("/proc"); len(pids) > 0 {
		return nil, fmt.Errorf("a forwarder is already running in a terminal (pid %s); end it with Ctrl-C there first", strings.Join(pids, ", "))
	}
	id, err := guestCID(inst)
	if err != nil {
		return nil, err
	}
	if !a.guestInputReady(name) {
		return nil, fmt.Errorf("rig-input is not running in %s, so there is nothing to send input to.\n"+
			"It comes with rig's desktop module (rig.desktop.input.enable, on by default);\n"+
			"a guest built from an image before it existed wants rebuilding:\n"+
			"  rig image build ..., then recreate the VM", name)
	}
	out := []string{"host", "input", name, "--cid", strconv.FormatUint(uint64(id), 10)}
	for _, d := range devs {
		out = append(out, "--device", d)
	}
	return out, nil
}

func (a *app) guestInputReady(name string) bool {
	out, err := a.exec(name, "systemctl is-active rig-input")
	return err == nil && strings.TrimSpace(out) == "active"
}

// startInputUnit is the root half of --background. A unit left from a stop
// rig did not see is stopped first, so a VM never has two.
func startInputUnit(vm string, cid uint32, devs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find my own path for the unit to run: %w", err)
	}
	unit := inputUnit(vm)
	_ = exec.Command("systemctl", "stop", unit).Run()
	_ = exec.Command("systemctl", "reset-failed", unit).Run()
	argv := []string{"--unit", unit, "--collect", "--quiet",
		"--description", "rig: this host's keyboard and mouse, lent to " + vm,
		// A crash would otherwise end input for the rest of the session. The
		// VM stopping is a clean exit, so it is not restarted.
		"-p", "Restart=on-failure", "-p", "RestartSec=2",
		exe, "host", "input", vm, "--cid", strconv.FormatUint(uint64(cid), 10), "--follow"}
	for _, d := range devs {
		argv = append(argv, "--device", d)
	}
	if out, err := exec.Command("systemd-run", argv...).CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	note("input for %s runs in the background (%s); both Ctrl keys move it to the guest and back", vm, unit)
	note("  it ends when %s stops. Sooner: rig host input %s --stop. Its log: journalctl -u %s", vm, vm, unit)
	return nil
}

// isLentVM reports whether vm is running with the vsock context ID input
// was resolved against. A different ID is a different boot, which the
// forwarder must not follow into.
func (a *app) isLentVM(vm string, cid uint32) bool {
	inst, _, err := a.c.Instance(vm)
	return err == nil && inst.Running() && inst.Config["volatile.vsock_id"] == strconv.FormatUint(uint64(cid), 10)
}

// followVM ends ctx once vm stops, so a background forwarder ends with the
// VM it was lent to rather than waiting, grab-ready, for a guest that is
// gone. A few failed reads in a row are needed, not one: Incus restarting
// under a running VM is not the VM stopping.
func (a *app) followVM(ctx context.Context, vm string, cid uint32) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		misses := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if a.isLentVM(vm, cid) {
				misses = 0
				continue
			}
			if misses++; misses >= 3 {
				note("%s stopped; input stays with this host", vm)
				return
			}
		}
	}()
	return ctx
}

// startInput is rig start's half: lend input when the instance asks for it.
// Failing here does not fail the start, since the VM is up and useful, but it
// is said loudly, with the command that does it by hand.
func (a *app) startInput(name string, wait bool) {
	inst, _, err := a.c.Instance(name)
	if err != nil || inst.Config[inputKey] != manifest.InputHost {
		return
	}
	if !wait {
		note("%s asks for this host's input; once it is up:  rig host input %s --background", name, name)
		return
	}
	// The agent answers before the guest's own services are all up.
	for deadline := time.Now().Add(30 * time.Second); !a.guestInputReady(name) && time.Now().Before(deadline); {
		time.Sleep(time.Second)
	}
	args, err := a.inputArgs(name, nil, true)
	if err == nil {
		err = elevate(append(args, "--background"))
	}
	if err != nil {
		note("WARNING: this host's input was not lent to %s: %v", name, err)
		note("         By hand, once that is fixed:  rig host input %s --background", name)
	}
}

// stopInput waits for a background forwarder to notice its VM has stopped.
// It ends by itself; this only makes rig stop report it, and names the way
// to end it when it has not.
func stopInput(name string) {
	if !inputActive(name) {
		return
	}
	for deadline := time.Now().Add(10 * time.Second); inputActive(name) && time.Now().Before(deadline); {
		time.Sleep(500 * time.Millisecond)
	}
	if inputActive(name) {
		note("WARNING: the input forwarder for %s is still running:  rig host input %s --stop", name, name)
		return
	}
	note("input is back with this host")
}

// terminalForwarders finds `rig host input` root halves started by hand in a
// terminal: they carry --cid, and only the background unit's carry --follow.
// Command lines are world-readable, so this needs no privilege.
func terminalForwarders(proc string) []string {
	dirs, _ := filepath.Glob(filepath.Join(proc, "[0-9]*"))
	var pids []string
	for _, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if isTerminalForwarder(args) {
			pids = append(pids, filepath.Base(dir))
		}
	}
	return pids
}

func isTerminalForwarder(args []string) bool {
	var host, cid, follow bool
	for i, a := range args {
		switch {
		case a == "host" && i+1 < len(args) && args[i+1] == "input":
			host = true
		case a == "--cid" || strings.HasPrefix(a, "--cid="):
			cid = true
		case a == "--follow":
			follow = true
		}
	}
	return host && cid && !follow
}
