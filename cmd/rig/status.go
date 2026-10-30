package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/agent"
	"github.com/tbrockman/rig/internal/cpuset"
	"github.com/tbrockman/rig/internal/creds"
	"github.com/tbrockman/rig/internal/devices"
	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
	"github.com/tbrockman/rig/internal/policy"
	"github.com/tbrockman/rig/internal/proxy"
	"github.com/tbrockman/rig/internal/volumes"
)

// What Incus records about VMs, read back. Nothing here runs in a guest, so
// every answer is cheap and a stopped VM gives one too.

// vmRow is one VM in a listing.
type vmRow struct {
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	Address  string   `json:"address,omitempty"`
	HasGPU   bool     `json:"has_gpu"`
	Devices  []string `json:"devices"`
	Isolated bool     `json:"isolated"`
	Managed  bool     `json:"managed"`
}

// vmRows lists instances with the devices each holds. The address costs a
// request per running VM, so only a caller that shows it asks for it.
func (a *app) vmRows(instances []incus.Instance, holders []devices.Holder, withAddress bool) []vmRow {
	rows := []vmRow{}
	for i := range instances {
		inst := &instances[i]
		r := vmRow{Name: inst.Name, Status: inst.Status, Devices: []string{},
			Managed: inst.Config[managedKey] == "true"}
		for _, h := range holders {
			if h.Instance != inst.Name {
				continue
			}
			r.Devices = append(r.Devices, h.Device)
			r.HasGPU = r.HasGPU || h.Kind == manifest.KindGPU
		}
		unisolated, _ := policy.Report(inst, a.cfg.ACL)
		r.Isolated = len(unisolated) == 0
		if withAddress && inst.Running() {
			r.Address, _ = a.c.GlobalIPv4(inst.Name)
		}
		rows = append(rows, r)
	}
	return rows
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) listCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		GroupID: "vm",
		Short:   "Every VM, one line each: state, address, devices, isolation",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			instances, err := a.c.Instances()
			if err != nil {
				return err
			}
			rows := a.vmRows(instances, devices.Holders(instances), true)
			if asJSON {
				return printJSON(rows)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATE\tADDRESS\tDEVICES\tISOLATION")
			for _, r := range rows {
				iso := "isolated"
				if !r.Isolated {
					iso = "!! NONE"
				}
				name := r.Name
				if !r.Managed {
					name += " (not rig's)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, r.Status,
					firstNonEmpty(r.Address, "-"), firstNonEmpty(strings.Join(r.Devices, ","), "-"), iso)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

func (a *app) statusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "status [<vm>...]",
		GroupID: "vm",
		Short:   "Who holds which host device; with VMs named, everything rig recorded on each",
		Long: "With no VM: each host device a VM holds, then every VM, marking those\n" +
			"that hold one (*) or are not isolated (!!).\n\n" +
			"With VMs named: each one's state and address, and every grant rig\n" +
			"recorded on it: devices, ports, volumes, input, credentials, the agent.\n" +
			"It is all read from Incus, so a stopped VM answers too. `rig doctor`\n" +
			"checks the record against this host; `rig agent status` asks the guest.",
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return a.describeVMs(args, asJSON)
			}
			instances, err := a.c.Instances()
			if err != nil {
				return err
			}
			if err := devices.CheckProfiles(a.c, instances); err != nil {
				return err
			}
			holders := devices.Holders(instances)
			rows := a.vmRows(instances, holders, false)

			if asJSON {
				if holders == nil {
					holders = []devices.Holder{}
				}
				// "card" is the address the next claim would use, discovered
				// even when nothing holds it, so callers need not re-implement
				// the lookup.
				card, _ := devices.DiscoverPCI(a.c, a.cfg)
				return printJSON(map[string]any{
					"card": card, "holders": holders, "instances": rows,
				})
			}

			// One line per exclusive device, and a loud one for an address
			// configured twice: that is the state where the next start
			// hot-unplugs hardware from a running guest, silently.
			byAddr := map[string][]devices.Holder{}
			for _, h := range holders {
				if h.Exclusive() {
					byAddr[h.PCI] = append(byAddr[h.PCI], h)
				}
			}
			if len(byAddr) == 0 {
				fmt.Println("no host device is assigned to a VM.")
			}
			for _, addr := range sortedKeys(byAddr) {
				hs := byAddr[addr]
				if len(hs) == 1 {
					fmt.Printf("%s %s held by %s (%s) as %s\n", hs[0].Kind, addr, hs[0].Instance, hs[0].Status, hs[0].Device)
					continue
				}
				var who []string
				for _, h := range hs {
					who = append(who, h.Instance)
				}
				fmt.Printf("!! %s %s is configured on MULTIPLE instances (%s) — starting a second\n", hs[0].Kind, addr, strings.Join(who, ", "))
				fmt.Println("   one will hot-unplug it from the first. Fix with `rig release`.")
			}
			fmt.Println()
			for _, r := range rows {
				mark := " "
				if len(r.Devices) > 0 {
					mark = "*"
				}
				flags := ""
				if !r.Isolated {
					flags = "  !! NO ISOLATION"
				}
				fmt.Printf(" %s %-20s %-10s%s\n", mark, r.Name, r.Status, flags)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// vmDetail is everything rig recorded on one instance.
type vmDetail struct {
	Name         string              `json:"name"`
	Status       string              `json:"status"`
	Managed      bool                `json:"managed"`
	Address      string              `json:"address,omitempty"`
	Manifest     string              `json:"manifest,omitempty"`
	Image        string              `json:"image,omitempty"`
	ImageBuilt   string              `json:"image_built,omitempty"`
	CPUs         string              `json:"cpus,omitempty"`
	Memory       string              `json:"memory,omitempty"`
	Isolated     bool                `json:"isolated"`
	Isolation    string              `json:"isolation"`
	Devices      []devices.Holder    `json:"devices"`
	Ports        []string            `json:"ports"`
	Forwards     []int               `json:"forwards"`
	Volumes      map[string]string   `json:"volumes"` // name -> path in the guest
	Input        string              `json:"input,omitempty"`
	EnvFile      string              `json:"env_file,omitempty"`
	Proxied      map[string][]string `json:"proxied,omitempty"`
	Egress       string              `json:"egress,omitempty"`
	AgentSession string              `json:"agent_session,omitempty"`
	Agent        *manifest.Agent     `json:"agent,omitempty"` // guest.agent's long form, as recorded
}

func (a *app) vmDetail(name string) (vmDetail, error) {
	if err := a.requireInstance(name); err != nil {
		return vmDetail{}, err
	}
	inst, _, err := a.c.Instance(name)
	if err != nil {
		return vmDetail{}, err
	}
	unisolated, noEgress := policy.Report(inst, a.cfg.ACL)
	d := vmDetail{
		Name:         inst.Name,
		Status:       inst.Status,
		Managed:      inst.Config[managedKey] == "true",
		Manifest:     inst.Config[manifestKey],
		Image:        inst.Config[imageKey],
		ImageBuilt:   inst.Config["image.rig.built"],
		CPUs:         inst.Config["limits.cpu"],
		Memory:       inst.Config["limits.memory"],
		Isolated:     len(unisolated) == 0,
		Isolation:    isolationDetail(a.cfg.ACL, len(inst.NICs()), unisolated, noEgress),
		Devices:      devices.Holders([]incus.Instance{*inst}),
		Ports:        []string{},
		Forwards:     []int{},
		Volumes:      volumes.Mounts(inst),
		Input:        inst.Config[inputKey],
		EnvFile:      inst.Config[creds.InstanceKey],
		Egress:       inst.Config[proxy.EgressKey],
		AgentSession: inst.Config[agent.SessionKey],
	}
	if d.Devices == nil {
		d.Devices = []devices.Holder{}
	}
	if inst.Running() {
		d.Address, _ = a.c.GlobalIPv4(name)
	}
	recorded, err := recordedPorts(inst)
	if err != nil {
		return d, err
	}
	for _, p := range recorded {
		d.Ports = append(d.Ports, p.String())
	}
	if fwd, err := recordedForwards(inst); err != nil {
		return d, err
	} else if fwd != nil {
		d.Forwards = fwd
	}
	if d.Proxied, err = proxy.Decode(inst.Config[proxy.InstanceKey]); err != nil {
		return d, err
	}
	if spec, err := recordedAgent(inst); err != nil {
		return d, err
	} else if !spec.IsZero() {
		d.Agent = &spec
	}
	return d, nil
}

// describeAgentSpec is guest.agent's long form in a line: the settings a
// start reads, then the budgets and thresholds, each only when set.
func describeAgentSpec(a manifest.Agent) string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+" "+v)
		}
	}
	add("harness", a.Harness)
	add("brief", a.PromptFile)
	add("workdir", a.Workdir)
	add("model", a.Model)
	add("timeout", a.Timeout)
	add("idle timeout", a.IdleTimeout)
	add("memory", a.MemoryMax)
	if a.UntilDone {
		parts = append(parts, "until done")
	}
	if b := a.Budgets; b != nil {
		var limits []string
		if b.Hours > 0 {
			limits = append(limits, fmt.Sprintf("%gh", b.Hours))
		}
		if b.Tokens > 0 {
			limits = append(limits, fmt.Sprintf("%d tokens", b.Tokens))
		}
		if b.Usage != nil {
			limits = append(limits, fmt.Sprintf("%s at %.0f%%", b.Usage.Window, b.Usage.Max*100))
		}
		if b.Restarts > 0 {
			limits = append(limits, fmt.Sprintf("%d restarts", b.Restarts))
		}
		parts = append(parts, "budgets: "+strings.Join(limits, ", "))
	}
	if w := a.Watch; w != nil {
		var marks []string
		if w.IdleMinutes > 0 {
			marks = append(marks, fmt.Sprintf("idle %dm", w.IdleMinutes))
		}
		if w.UnitMinutes > 0 {
			marks = append(marks, fmt.Sprintf("unit %dm", w.UnitMinutes))
		}
		if w.StatusMinutes > 0 {
			marks = append(marks, fmt.Sprintf("status %dm", w.StatusMinutes))
		}
		if w.Usage != nil {
			marks = append(marks, fmt.Sprintf("%s at %.0f%%", w.Usage.Window, w.Usage.Max*100))
		}
		if len(w.On) > 0 {
			marks = append(marks, fmt.Sprintf("%d with a message", len(w.On)))
		}
		parts = append(parts, "watch: "+strings.Join(marks, ", "))
	}
	return strings.Join(parts, "; ")
}

func (a *app) describeVMs(names []string, asJSON bool) error {
	var all []vmDetail
	for _, name := range names {
		d, err := a.vmDetail(name)
		if err != nil {
			return err
		}
		all = append(all, d)
	}
	if asJSON {
		return printJSON(all)
	}
	for i, d := range all {
		if i > 0 {
			fmt.Println()
		}
		printDetail(d)
	}
	return nil
}

func printDetail(d vmDetail) {
	head := d.Name + "  " + d.Status
	if d.Address != "" {
		head += "  " + d.Address
	}
	if !d.Managed {
		head += "  (not created by rig)"
	}
	fmt.Println(head)
	line := func(label, value string) { fmt.Printf("  %-10s %s\n", label, value) }
	none := func(list []string) string { return firstNonEmpty(strings.Join(list, ", "), "none") }

	if d.Manifest != "" {
		line("manifest", d.Manifest)
	}
	if d.Image != "" {
		image := d.Image
		if d.ImageBuilt != "" {
			image += ", built " + d.ImageBuilt
		}
		line("image", image)
	}
	var size []string
	switch {
	case cpuset.IsSet(d.CPUs):
		size = append(size, "cpus "+d.CPUs+" (pinned)")
	case d.CPUs != "":
		size = append(size, d.CPUs+" cpus")
	}
	if d.Memory != "" {
		size = append(size, d.Memory)
	}
	if len(size) > 0 {
		line("size", strings.Join(size, ", "))
	}
	isolation := d.Isolation
	if !d.Isolated {
		isolation = "!! " + isolation
	}
	line("isolation", isolation)

	var devs []string
	for _, h := range d.Devices {
		devs = append(devs, fmt.Sprintf("%s %s as %s", h.Kind, h.PCI, h.Device))
	}
	line("devices", none(devs))
	line("ports", none(d.Ports))
	if len(d.Forwards) > 0 {
		var fwd []string
		for _, p := range d.Forwards {
			fwd = append(fwd, fmt.Sprintf("127.0.0.1:%d", p))
		}
		held := "held by rig-forward-" + d.Name + " while it runs"
		if !forwardActive(d.Name) {
			held = "not held now; rig start holds them, or: rig forward " + d.Name + " --background"
		}
		line("forwards", strings.Join(fwd, ", ")+" over vsock ("+held+")")
	}
	var vols []string
	for _, name := range sortedKeys(d.Volumes) {
		vols = append(vols, name+" at "+d.Volumes[name])
	}
	if len(vols) == 0 {
		line("volumes", "none; /work is on its disk, and rig rm deletes it with the VM")
	} else {
		line("volumes", none(vols))
	}
	switch {
	case d.Input == manifest.InputHost && inputActive(d.Name):
		line("input", "this host's keyboard and mouse, lent now ("+inputUnit(d.Name)+")")
	case d.Input == manifest.InputHost:
		line("input", "this host's keyboard and mouse, from start to stop")
	}
	if d.EnvFile != "" {
		line("env file", d.EnvFile)
	}
	if len(d.Proxied) > 0 {
		proxied := proxy.DescribeRecord(d.Proxied)
		if d.Egress != "" {
			proxied += "; egress: " + d.Egress
		}
		line("proxied", proxied)
	}
	if d.Agent != nil {
		line("agent", describeAgentSpec(*d.Agent))
	}
	if d.AgentSession != "" {
		line("", "session "+d.AgentSession+";  rig agent status "+d.Name+" asks the guest")
	}
}
