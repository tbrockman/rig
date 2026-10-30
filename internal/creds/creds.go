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

// Values reads the variables a file defines, for a reader on this host that
// needs one of them (a harness asking the provider about its credential's
// quota). A value's surrounding quotes are dropped, as a shell would.
// Nothing here logs or prints a value.
func Values(file string) (map[string]string, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	vars := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if !assignment.MatchString(text) {
			continue
		}
		k, v, _ := strings.Cut(text, "=")
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		vars[k] = v
	}
	return vars, scanner.Err()
}

// Count returns how many variables the file defines.
func Count(file string) int {
	content, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	return CountIn(content)
}

// CountIn returns how many variables env file content defines.
func CountIn(content []byte) int {
	n := 0
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		if assignment.MatchString(strings.TrimSpace(scanner.Text())) {
			n++
		}
	}
	return n
}

// Prepare validates the file and returns its content, for a caller that
// changes what the guest gets before writing it (internal/proxy swaps the
// values the guest must never hold for stand-ins).
func Prepare(file string) ([]byte, error) {
	if err := Validate(file); err != nil {
		return nil, err
	}
	return os.ReadFile(file)
}

// Write puts env file content into the guest at GuestPath, root-owned and
// 0600, and returns how many variables it defines.
func Write(c *incus.Client, instance string, content []byte) (int, error) {
	if err := c.Mkdir(instance, path.Dir(GuestPath), 0o700); err != nil {
		return 0, err
	}
	if err := c.WriteFile(instance, GuestPath, content, 0o600); err != nil {
		return 0, err
	}
	return CountIn(content), nil
}

// Inject pushes the file into the guest at GuestPath, as it is.
func Inject(c *incus.Client, instance, file string) (int, error) {
	content, err := Prepare(file)
	if err != nil {
		return 0, err
	}
	return Write(c, instance, content)
}
