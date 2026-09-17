// Package creds injects credentials into a guest.
//
// The design decision this implements: credentials are environment variables in
// a host file, and scoping them is the operator's job. Nothing here judges
// whether a key is narrow enough — it cannot — but it does refuse the two
// mistakes that are unambiguous.
package creds

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/tbrockman/rig/internal/incus"
)

// GuestPath is on tmpfs, so credentials never reach the instance's disk or its
// Incus config, and they vanish when the VM stops. rig start re-injects them.
const GuestPath = "/run/rig/env"

// InstanceKey holds the *path* to the env file on the instance, never the
// secret itself.
const InstanceKey = "user.rig.env"

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// Validate checks the file is safe to inject: not readable by other users, and
// nothing but comments, blanks and KEY=VALUE. The second stops shell constructs
// reaching the guest, since the file is sourced there.
func Validate(file string) error {
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("env file %s is mode %04o — readable by other users.\n"+
			"  It holds credentials:  chmod 600 %s", file, perm, file)
	}

	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	var bad []string
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") || assignment.MatchString(text) {
			continue
		}
		bad = append(bad, fmt.Sprintf("%d:%s", line, scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("env file %s has lines that are not KEY=VALUE:\n%s",
			file, strings.Join(bad, "\n"))
	}
	return nil
}

// Count returns how many variables the file defines.
func Count(file string) int {
	f, err := os.Open(file)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if assignment.MatchString(strings.TrimSpace(scanner.Text())) {
			n++
		}
	}
	return n
}

// Inject pushes the file into the guest at GuestPath, root-owned and 0600.
func Inject(c *incus.Client, instance, file string) (int, error) {
	if err := Validate(file); err != nil {
		return 0, err
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	if err := c.Mkdir(instance, path.Dir(GuestPath), 0o700); err != nil {
		return 0, err
	}
	if err := c.WriteFile(instance, GuestPath, content, 0o600); err != nil {
		return 0, err
	}
	return Count(file), nil
}
