package main

import "os/exec"

// forwardUnit is the transient user unit that holds a VM's manifest forwards
// (guest.forwards) on this host's loopback: rig start leaves it, it ends
// with the VM, and the next start leaves it again. User scope, since vsock
// needs no root and the operator's own session is where a preview belongs;
// `rig proxy setup`'s lingering keeps it past a logout, as it does the
// proxies.
func forwardUnit(vm string) string { return "rig-forward-" + vm + ".service" }

// forwardActive reports whether vm's forward holder is running.
func forwardActive(vm string) bool {
	return exec.Command("systemctl", "--user", "is-active", "--quiet", forwardUnit(vm)).Run() == nil
}
