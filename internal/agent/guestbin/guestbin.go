// Package guestbin carries the guest build of rig (cmd/rig/guest), which `rig
// agent start` installs in the guest. `make` builds it into bin/ before it
// builds rig; it is not committed, so a checkout that has not run make embeds
// only bin/README, and For says how to get the rest.
package guestbin

import (
	"embed"
	"fmt"
)

//go:embed bin
var bins embed.FS

// goarch is the Go name for each Incus architecture rig builds for.
var goarch = map[string]string{"x86_64": "amd64", "aarch64": "arm64"}

// File is the embedded name of the build for an Incus architecture.
func File(incusArch string) (string, error) {
	a, ok := goarch[incusArch]
	if !ok {
		return "", fmt.Errorf("no guest build of rig for architecture %q", incusArch)
	}
	return "rig-linux-" + a, nil
}

// For is the guest build for an Incus architecture ("x86_64", "aarch64").
func For(incusArch string) ([]byte, error) {
	name, err := File(incusArch)
	if err != nil {
		return nil, err
	}
	b, err := bins.ReadFile("bin/" + name)
	if err != nil {
		return nil, fmt.Errorf("this rig was built without its guest half (%s): build rig with make, "+
			"which builds that first; `go build` and `go install` alone leave it out", name)
	}
	return b, nil
}
