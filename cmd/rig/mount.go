package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Mounting a guest directory on the host, live.
//
// This exists because every network route into a guest is closed on purpose:
// the NIC carries ingress.action=reject, so ssh, vnc and network sshfs are all
// refused. What remains open is the Incus API socket — a unix socket on this
// host, not a network path — which is the same channel `rig exec` uses and the
// reason it keeps working.
//
// `incus file mount` tunnels SFTP over that socket, so a guest directory can be
// read live on the host without copying it out and without opening a hole. rig
// wraps it for the two things that are not obvious: sshfs is required and is
// usually not installed, and git refuses a tree whose files are root-owned.

// isMounted answers from /proc/mounts rather than from a command's exit code.
// Both verbs care about the same thing — is this path a mount right now — and
// asking the kernel is more honest than inferring it from fusermount's opinion.
func isMounted(path string) bool {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	// /proc/mounts escapes spaces as \040; compare against the same encoding.
	want := strings.ReplaceAll(path, " ", `\040`)
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[1] == want {
			return true
		}
	}
	return false
}

// sshfsHint is the error for a missing sshfs. It names the nix route because
// this host has nix and installing a system package for one command is a poor
// trade.
const sshfsHint = `sshfs is required and is not on PATH.

  With nix, no install needed:
    nix shell nixpkgs#sshfs --command rig mount %s

  Or system-wide:
    sudo apt install sshfs`

// gitSafeHint is printed when the mounted tree is a git repository. Guest files
// are owned by root, and git refuses to operate on a tree it thinks belongs to
// someone else — a confusing failure if you have not seen it before.
func gitSafeHint(at string) string {
	return fmt.Sprintf("this is a git repo, and its files are root-owned. For git to read it:\n"+
		"         git config --global --add safe.directory %s", at)
}

func (a *app) mountCmd() *cobra.Command {
	var guestPath, at string
	cmd := &cobra.Command{
		Use:     "mount <name>",
		GroupID: "guest",
		Short:   "Mount a guest directory on this host, live",
		Long: "Mounts a directory from the guest onto the host over the Incus API\n" +
			"socket, so you can read what the guest is doing without copying it out\n" +
			"and without opening the network isolation. Nothing is transferred; the\n" +
			"view is live.\n\n" +
			"This blocks, holding the mount. Ctrl-C unmounts. Run it in its own\n" +
			"terminal, or background it and use `rig unmount` to clean up.\n\n" +
			"Treat the mount as read-only: it is writable, and writing into a tree\n" +
			"an agent is editing races that agent.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			inst, _, err := a.c.Instance(name)
			if err != nil {
				return err
			}
			if !inst.Running() {
				return fmt.Errorf("%s is not running; there is nothing to mount", name)
			}
			if _, err := exec.LookPath("sshfs"); err != nil {
				return fmt.Errorf(sshfsHint, name)
			}

			if at == "" {
				at = name + "-live"
			}
			abs, err := filepath.Abs(at)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return err
			}
			if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
				return fmt.Errorf("%s is not empty; mounting over it would hide what is there", abs)
			}

			// Ask the guest before mounting, so the hint appears only when it
			// applies. Printing it always would train the reader to skip it.
			if out, err := a.exec(name, "test -d "+guestPath+"/.git && echo yes || true"); err == nil &&
				strings.TrimSpace(out) == "yes" {
				note("NOTE: %s", gitSafeHint(abs))
			}

			note("mounting %s:%s at %s", name, guestPath, abs)
			note("this blocks; Ctrl-C unmounts")

			target := name + strings.TrimSuffix(guestPath, "/")
			c := exec.Command("incus", "file", "mount", target, abs)
			c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
			runErr := c.Run()

			// Ctrl-C is the documented way to stop this, so it is not a
			// failure. The interrupt reaches the whole process group, sshfs
			// disconnects, and the command we wrapped exits non-zero — which
			// would otherwise be reported as "Error: exit status 1" directly
			// after telling the operator to press Ctrl-C.
			//
			// Judge the outcome instead of the exit code: if the path is no
			// longer a mount, the unmount is exactly what was asked for.
			if !isMounted(abs) {
				note("unmounted %s", abs)
				return nil
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&guestPath, "guest", "/work", "directory inside the guest")
	cmd.Flags().StringVar(&at, "at", "", "host directory to mount onto (default ./<name>-live)")
	return cmd
}

func (a *app) unmountCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "unmount <host-path>",
		GroupID: "guest",
		Short:   "Release a mount left behind by a backgrounded rig mount",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			abs, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			// Idempotent. A mount released by Ctrl-C is the normal case, and
			// reporting fusermount's "not found in /etc/mtab" for it turns a
			// tidy-up into something that looks broken.
			if !isMounted(abs) {
				note("%s is not mounted; nothing to do", abs)
				return nil
			}
			if out, err := exec.Command("fusermount", "-u", abs).CombinedOutput(); err != nil {
				return fmt.Errorf("unmounting %s: %w\n%s", abs, err, out)
			}
			note("unmounted %s", abs)
			return nil
		},
	}
}
