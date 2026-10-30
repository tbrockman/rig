package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The holder for a manifest's forwards (guest.forwards). A forward is a host
// process by nature: the vsock listener lives on the host side, and nothing
// in Incus or the guest can hold a port on this host's loopback. So rig
// start leaves one, a transient unit in the operator's user manager, the
// way it leaves the handback watcher and the input forwarder as root units;
// user scope because vsock needs no root and a preview is the operator's
// own, and `rig proxy setup`'s lingering keeps it past a logout as it does
// the proxies. It follows the VM and ends with it, and the next start
// leaves a new one, which is what a forward that died with a rebuilt VM
// needed. Not part of the proxy: that is witmproxy's process, exists only
// for a proxied VM, and carries HTTP, not a raw port.

// forwardUnit is the holder's transient user unit.
func forwardUnit(vm string) string { return "rig-forward-" + vm + ".service" }

// forwardActive reports whether vm's holder is running.
func forwardActive(vm string) bool {
	return exec.Command("systemctl", "--user", "is-active", "--quiet", forwardUnit(vm)).Run() == nil
}

// startForwardUnit leaves the holder: `rig forward <vm> <ports> --follow`
// under systemd-run --user. One left from before is stopped first, so a VM
// never has two; a crash is restarted, the VM stopping is a clean exit.
func startForwardUnit(vm string, ports []int, toGuest bool) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find my own path for the unit to run: %w", err)
	}
	unit := forwardUnit(vm)
	_ = exec.Command("systemctl", "--user", "stop", unit).Run()
	_ = exec.Command("systemctl", "--user", "reset-failed", unit).Run()
	argv := []string{"--user", "--unit", unit, "--collect", "--quiet",
		"--description", "rig: " + vm + "'s forwards on this host's loopback, over vsock",
		"-p", "Restart=on-failure", "-p", "RestartSec=5"}
	// The user manager's environment is not this shell's; what the Incus
	// client reads has to be carried across.
	if sock := os.Getenv("INCUS_SOCKET"); sock != "" {
		argv = append(argv, "--setenv=INCUS_SOCKET="+sock)
	}
	argv = append(argv, exe, "forward", vm, "--follow")
	if toGuest {
		argv = append(argv, "--to-guest")
	}
	for _, p := range ports {
		argv = append(argv, strconv.Itoa(p))
	}
	if out, err := exec.Command("systemd-run", argv...).CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-run --user: %v: %s", err, strings.TrimSpace(string(out)))
	}
	var where []string
	for _, p := range ports {
		where = append(where, fmt.Sprintf("127.0.0.1:%d", p))
	}
	note("forwards for %s held in the background (%s): %s", vm, unit, strings.Join(where, ", "))
	note("  it ends when %s stops. Sooner: rig forward %s --stop. Its log: journalctl --user -u %s", vm, vm, unit)
	return nil
}

func stopForwardUnit(vm string) error {
	if out, err := exec.Command("systemctl", "--user", "stop", forwardUnit(vm)).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user stop %s: %v: %s", forwardUnit(vm), err, strings.TrimSpace(string(out)))
	}
	note("stopped %s", forwardUnit(vm))
	return nil
}

// startForwards is rig start's half: hold the manifest's forwards once the
// guest is up. Failing here does not fail the start, since the VM is up and
// useful, but it is said loudly, with the command that does it by hand.
func (a *app) startForwards(name string, wait bool) {
	inst, _, err := a.c.Instance(name)
	if err != nil {
		return
	}
	ports, err := recordedForwards(inst)
	if err != nil || len(ports) == 0 {
		return
	}
	if !wait {
		note("%s has forwards to hold; once it is up:  rig forward %s --background", name, name)
		return
	}
	if err := startForwardUnit(name, ports, false); err != nil {
		note("WARNING: %s's forwards are not held: %v", name, err)
		note("         By hand, once that is fixed:  rig forward %s --background", name)
	}
}

// stopForwards waits for the holder to notice its VM has stopped, and ends
// it when it has not: a listener on a port nobody can answer is worse than
// none.
func stopForwards(name string) {
	if !forwardActive(name) {
		return
	}
	for deadline := time.Now().Add(10 * time.Second); forwardActive(name) && time.Now().Before(deadline); {
		time.Sleep(500 * time.Millisecond)
	}
	if forwardActive(name) {
		_ = exec.Command("systemctl", "--user", "stop", forwardUnit(name)).Run()
	}
	note("%s's forwards are released", name)
}
