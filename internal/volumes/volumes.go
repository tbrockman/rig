// Package volumes gives a guest the Incus custom storage volumes its manifest
// names, and takes away the ones it no longer does.
//
// A custom volume lives in the storage pool, not in the instance: it survives
// `rig rm`, so data on it survives the VM being recreated from a new image,
// which is how every change to a guest image reaches a VM. It is not a
// directory of this host either, so mounting one opens no path between the
// guest and the host's filesystem; the manifest refuses host paths outright.
package volumes

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
)

// devicePrefix marks the disk devices this package owns on an instance.
const devicePrefix = "rig-vol-"

// DeviceName is the disk device that mounts one volume.
func DeviceName(v manifest.Volume) string { return devicePrefix + v.Name }

// Pool is the storage pool of the instance's root disk, where its volumes go.
func Pool(inst *incus.Instance) string {
	for _, d := range inst.ExpandedDevices {
		if d.Type() == "disk" && d["path"] == "/" {
			return d["pool"]
		}
	}
	return ""
}

// Device is the Incus disk device mounting a volume at its path.
func Device(v manifest.Volume, pool string) incus.Device {
	return incus.Device{"type": "disk", "pool": pool, "source": v.Name, "path": v.Path}
}

// Held lists the volumes an instance mounts, by name, for telling someone
// what `rig rm` leaves behind.
func Held(inst *incus.Instance) []string {
	var out []string
	for name, d := range inst.Devices {
		if strings.HasPrefix(name, devicePrefix) && d.Type() == "disk" {
			out = append(out, d["source"])
		}
	}
	sort.Strings(out)
	return out
}

// Plan works out the instance's devices with exactly the declared volumes
// mounted. Devices rig did not put there are left alone. Pure.
func Plan(inst *incus.Instance, vols []manifest.Volume, pool string) (map[string]incus.Device, []string) {
	devices := make(map[string]incus.Device, len(inst.Devices))
	for name, d := range inst.Devices {
		devices[name] = d
	}
	want := map[string]incus.Device{}
	for _, v := range vols {
		want[DeviceName(v)] = Device(v, pool)
	}
	var changes []string
	for name, d := range inst.Devices {
		if strings.HasPrefix(name, devicePrefix) && want[name] == nil {
			delete(devices, name)
			changes = append(changes, fmt.Sprintf("unmount volume %s from %s (the volume itself is kept)", d["source"], d["path"]))
		}
	}
	for name, d := range want {
		if !equal(devices[name], d) {
			devices[name] = d
			changes = append(changes, fmt.Sprintf("mount volume %s at %s", d["source"], d["path"]))
		}
	}
	sort.Strings(changes)
	return devices, changes
}

// Reconcile creates any declared volume that does not exist yet and mounts
// exactly the declared ones, or with dryRun reports what it would do. A
// volume's size and owner apply when it is created; an existing volume is
// never resized or chowned from here.
func Reconcile(c *incus.Client, inst *incus.Instance, vols []manifest.Volume, dryRun bool) ([]string, error) {
	pool := Pool(inst)
	if pool == "" && len(vols) > 0 {
		return nil, fmt.Errorf("%s has no root disk in a storage pool to put volumes in", inst.Name)
	}
	var changes []string
	for _, v := range vols {
		if c.CustomVolumeExists(pool, v.Name) {
			continue
		}
		config := map[string]string{}
		detail := ""
		if v.Size != "" {
			config["size"] = v.Size
			detail += ", " + v.Size
		}
		if v.Owner != "" {
			uid, gid, _ := strings.Cut(v.Owner, ":")
			config["initial.uid"], config["initial.gid"], config["initial.mode"] = uid, gid, "0755"
			detail += ", owned by " + v.Owner
		}
		changes = append(changes, fmt.Sprintf("create volume %s in pool %s%s", v.Name, pool, detail))
		if !dryRun {
			if err := c.CreateCustomVolume(pool, v.Name, config); err != nil {
				return changes, fmt.Errorf("creating volume %s: %w", v.Name, err)
			}
		}
	}
	devices, devChanges := Plan(inst, vols, pool)
	changes = append(changes, devChanges...)
	if len(devChanges) == 0 || dryRun {
		return changes, nil
	}
	if inst.Running() {
		return changes, fmt.Errorf("%s is running; its volumes are changed while it is stopped.\n  rig stop %s, then rig apply -f again", inst.Name, inst.Name)
	}
	return changes, c.SetDevices(inst.Name, devices)
}

func equal(a, b incus.Device) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
