package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/pullguard"
	"github.com/tbrockman/rig/internal/pushguard"
)

// Copying files in and out. push and pull take the same shape — the VM, where
// to read, and optionally where to write — and the same rules: a directory's
// contents land at exactly the destination, and neither overwrites a file it
// did not write.

// destination is the optional third operand, or the flag it replaced. Both at
// once is ambiguous.
func destination(c *cobra.Command, args []string, flag string) (string, error) {
	fromFlag, _ := c.Flags().GetString(flag)
	if len(args) < 3 {
		return fromFlag, nil
	}
	if fromFlag != "" {
		return "", fmt.Errorf("give the destination once: as the third argument, not also as --%s", flag)
	}
	return args[2], nil
}

func (a *app) pushCmd() *cobra.Command {
	var dest string
	var force bool
	cmd := &cobra.Command{
		Use:     "push <vm> <host-path> [<guest-path>]",
		GroupID: "guest",
		Short:   "Copy a host file or directory into the guest",
		Long: "A directory's *contents* go to <guest-path>, which defaults to\n" +
			"/work/<basename>. Unlike `incus file push -r`, the destination is\n" +
			"exactly the destination — no directory named after the source appears.\n" +
			"A file goes to <guest-path>, or into it when it is a directory.\n\n" +
			"rig records what it writes, and refuses to overwrite a guest file that\n" +
			"changed since or that it did not write: /work exists only inside the\n" +
			"instance, so that copy may be the only one. --force overwrites them.",
		Args: cobra.RangeArgs(2, 3),
		RunE: func(c *cobra.Command, args []string) error {
			name, src := args[0], strings.TrimRight(args[1], "/")
			target, err := destination(c, args, "dest")
			if err != nil {
				return err
			}
			if target != "" && !path.IsAbs(target) {
				return fmt.Errorf("%s: a guest path is absolute", target)
			}
			if err := a.requireInstance(name); err != nil {
				return err
			}
			st, err := os.Stat(src)
			if err != nil {
				return err
			}
			if target == "" {
				abs, err := filepath.Abs(src)
				if err != nil {
					return err
				}
				target = path.Join("/work", filepath.Base(abs))
			} else if !st.IsDir() {
				// cp's rule: a file sent to a directory lands inside it.
				if info, err := a.c.Stat(name, target); err == nil && info.Type == "directory" {
					target = path.Join(target, filepath.Base(src))
				}
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

			n := 1
			if st.IsDir() {
				n, err = a.c.PushDir(name, src, target)
			} else {
				err = a.c.PushFile(name, src, target, st.Mode().Perm())
			}
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
	cmd.Flags().StringVar(&dest, "dest", "", "guest path")
	_ = cmd.Flags().MarkDeprecated("dest", "give the guest path as the third argument")
	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite guest files rig did not write — read them first, the guest copy may be the only one")
	return cmd
}

func (a *app) pullCmd() *cobra.Command {
	var out string
	var force bool
	cmd := &cobra.Command{
		Use:     "pull <vm> <guest-path> [<host-path>]",
		GroupID: "guest",
		Short:   "Copy a guest file or directory out to the host",
		Long: "A directory's *contents* go to <host-path>, which defaults to\n" +
			"./<basename>, relative to your current directory; as with push, no\n" +
			"directory named after the source appears. A file goes to stdout, or to\n" +
			"<host-path>: into it when it is a directory. Files keep the guest's mode\n" +
			"and modification time; symlinks are skipped.\n\n" +
			"Pulling again refreshes the copy, transferring only files whose size or\n" +
			"time changed. rig marks each file it writes, and refuses to overwrite one\n" +
			"changed on the host since, or one it did not write that differs from the\n" +
			"guest's. --force overwrites them.",
		Args: cobra.RangeArgs(2, 3),
		RunE: func(c *cobra.Command, args []string) error {
			name, src := args[0], args[1]
			dst, err := destination(c, args, "out")
			if err != nil {
				return err
			}
			if !path.IsAbs(src) {
				return fmt.Errorf("%s: a guest path is absolute", src)
			}
			src = path.Clean(src)
			if err := a.requireInstance(name); err != nil {
				return err
			}
			info, err := a.c.Stat(name, src)
			if err != nil {
				return err
			}
			retry := func(dst string) string {
				return fmt.Sprintf("rig pull --force %s %s %s", name, src, dst)
			}
			switch info.Type {
			case "file":
				if dst == "" || dst == "-" {
					_, err := a.c.PullTo(name, src, os.Stdout)
					return err
				}
				if st, err := os.Stat(dst); err == nil && st.IsDir() {
					dst = filepath.Join(dst, path.Base(src))
				}
				return a.pull(name, src, dst, []incus.GuestEntry{{Rel: ".", FileInfo: info}}, force, retry(dst))
			case "directory":
				switch {
				case dst == "-":
					return fmt.Errorf("%s is a directory; name a host directory to copy it to", src)
				case dst == "" && src == "/":
					return fmt.Errorf("name a host directory to copy the guest's / to")
				case dst == "":
					dst = path.Base(src)
				}
				tree, err := a.c.Tree(name, src)
				if err != nil {
					return err
				}
				entries := append([]incus.GuestEntry{{Rel: ".", FileInfo: info}}, tree...)
				return a.pull(name, src, dst, entries, force, retry(dst))
			}
			return &incus.NotAFileError{Path: src, Type: info.Type}
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "host path")
	_ = cmd.Flags().MarkDeprecated("out", "give the host path as the third argument")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite host files rig did not write, or that changed since it did")
	return cmd
}

// pull copies entries, relative to guestRoot, to the same paths under
// hostRoot. Every one is checked before any is written, so a refusal moves
// nothing.
func (a *app) pull(name, guestRoot, hostRoot string, entries []incus.GuestEntry, force bool, retry string) error {
	var conflicts []pullguard.Conflict
	var todo []incus.GuestEntry
	var files, current, links int
	var size int64
	for _, e := range entries {
		hp := filepath.Join(hostRoot, filepath.FromSlash(e.Rel))
		gp := path.Join(guestRoot, e.Rel)
		switch e.Type {
		case "directory":
			todo = append(todo, e)
			if c := pullguard.CheckDir(hp); c != nil {
				conflicts = append(conflicts, *c)
			}
		case "file":
			files++
			if pullguard.Current(hp, e.Size, e.Modified) {
				current++
				continue
			}
			todo = append(todo, e)
			size += e.Size
			if force {
				continue
			}
			c, err := pullguard.Check(hp, func(w io.Writer) error {
				_, err := a.c.PullTo(name, gp, w)
				return err
			})
			if err != nil {
				return err
			}
			if c != nil {
				conflicts = append(conflicts, *c)
			}
		default:
			links++
		}
	}
	if len(conflicts) > 0 {
		return pullguard.Error(conflicts, retry)
	}

	note("pulling %d file(s), %s, from %s:%s to %s", files-current, humanBytes(size), name, guestRoot, hostRoot)
	for _, e := range todo {
		hp := filepath.Join(hostRoot, filepath.FromSlash(e.Rel))
		gp := path.Join(guestRoot, e.Rel)
		switch e.Type {
		case "directory":
			if err := os.MkdirAll(hp, 0o755); err != nil {
				return err
			}
		case "file":
			err := pullguard.Write(hp, e.Mode, e.Modified, func(w io.Writer) error {
				_, err := a.c.PullTo(name, gp, w)
				return err
			})
			if err != nil {
				return fmt.Errorf("pulling %s: %w", gp, err)
			}
		}
	}
	if links > 0 {
		note("skipped %d symlink(s)", links)
	}
	if current > 0 {
		note("%d file(s) were already current", current)
	}
	note("pulled %d file(s) to %s", files-current, hostRoot)
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
