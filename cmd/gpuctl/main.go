// Command gpuctl is the privileged half of the tooling: GPU arbitration plus
// the policy reconcile. Routine work goes through rig; this has the verbs that
// can break an invariant.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"rig/internal/gpu"
	"rig/internal/incus"
	"rig/internal/policy"
)

type app struct {
	c   *incus.Client
	cfg gpu.Config
}

func main() {
	a := &app{c: incus.New(""), cfg: gpu.ConfigFromEnv()}

	root := &cobra.Command{
		Use:   "gpuctl",
		Short: "GPU arbitration and isolation policy",
		Long: "gpuctl enforces one invariant: at most one instance has the GPU\n" +
			"device configured, and it never moves away from a running instance.\n" +
			"It also reconciles the network isolation policy.\n\n" +
			"Environment: GPUCTL_PCI, GPUCTL_DEVICE, GPUCTL_ACL, GPUCTL_PROFILE,\n" +
			"GPUCTL_LOCK, INCUS_SOCKET.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(a.statusCmd(), a.claimCmd(), a.releaseCmd(), a.startCmd(), a.stopCmd(), a.applyCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func (a *app) statusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Who holds the card",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			instances, err := a.c.Instances()
			if err != nil {
				return err
			}
			if err := gpu.CheckProfiles(a.c, instances); err != nil {
				return err
			}
			holders := gpu.Holders(instances)

			if asJSON {
				type row struct {
					Name     string `json:"name"`
					Status   string `json:"status"`
					Isolated bool   `json:"isolated"`
				}
				rows := []row{}
				for i := range instances {
					unisolated, _ := policy.Report(&instances[i], a.cfg.ACL)
					rows = append(rows, row{instances[i].Name, instances[i].Status, len(unisolated) == 0})
				}
				if holders == nil {
					holders = []gpu.Holder{}
				}
				// "card" is the address the tools would use for the next claim,
				// discovered even when nothing holds it. Saves every caller
				// re-implementing the lookup.
				card, _ := gpu.DiscoverPCI(a.c, a.cfg)
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"card": card, "holders": holders, "instances": rows,
				})
			}

			switch len(holders) {
			case 0:
				fmt.Println("GPU is unassigned.")
			case 1:
				fmt.Printf("GPU held by %s (%s) at %s\n", holders[0].Instance, holders[0].Status, holders[0].PCI)
			default:
				fmt.Println("!! GPU device is configured on MULTIPLE instances:")
				for _, h := range holders {
					fmt.Printf("   %-20s %-10s %s\n", h.Instance, h.Status, h.PCI)
				}
				fmt.Println("   Starting a second one will hot-unplug the card from the")
				fmt.Println("   first. Fix with: gpuctl release && gpuctl claim <instance>")
			}

			fmt.Println()
			for i := range instances {
				inst := &instances[i]
				mark := " "
				for _, h := range holders {
					if h.Instance == inst.Name {
						mark = "*"
					}
				}
				note := ""
				if unisolated, noEgress := policy.Report(inst, a.cfg.ACL); len(unisolated) > 0 {
					note = "  !! NO ISOLATION on " + join(unisolated)
				} else if len(noEgress) > 0 {
					note = "  !! no egress on " + join(noEgress)
				}
				fmt.Printf(" %s %-20s %-10s%s\n", mark, inst.Name, inst.Status, note)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

func (a *app) claimCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "claim <instance>",
		Short: "Move the card to an instance",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return gpu.Claim(a.c, a.cfg, args[0]) },
	}
}

func (a *app) releaseCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Detach the card from whoever holds it",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return gpu.Release(a.c, a.cfg, force) },
	}
	cmd.Flags().BoolVar(&force, "force", false, "hot-unplug from a running instance (breaks its workload)")
	return cmd
}

func (a *app) startCmd() *cobra.Command {
	var allow bool
	var timeout int
	cmd := &cobra.Command{
		Use:   "start <instance>",
		Short: "Claim the card, then start",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return gpu.Start(a.c, a.cfg, args[0], allow, timeout)
		},
	}
	cmd.Flags().BoolVar(&allow, "allow-unisolated", false, "start even with no isolation ACL on the NIC")
	cmd.Flags().IntVar(&timeout, "timeout", 120, "seconds to allow for the start operation")
	return cmd
}

func (a *app) stopCmd() *cobra.Command {
	var timeout int
	cmd := &cobra.Command{
		Use:   "stop <instance>",
		Short: "Stop an instance (the card stays attached to it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return gpu.Stop(a.c, a.cfg, args[0], timeout)
		},
	}
	cmd.Flags().IntVar(&timeout, "timeout", 120, "seconds to allow for the stop operation")
	return cmd
}

func (a *app) applyCmd() *cobra.Command {
	var dryRun bool
	var profile string
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Reconcile the isolation ACL and the profile NIC",
		Long: "Declares the policy — the egress reject ranges and the three NIC keys —\n" +
			"and reconciles Incus to it. `incus admin init --preseed` does not cover\n" +
			"network ACLs, so this is the only way to get them from a clean host.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			changes, err := policy.Apply(a.c, a.cfg.ACL, profile, dryRun)
			for _, ch := range changes {
				if err != nil {
					fmt.Printf("  %s\n", ch)
				}
			}
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				fmt.Println("already reconciled; nothing to do.")
			} else {
				if dryRun {
					fmt.Println("dry run, nothing applied:")
				} else {
					fmt.Println("applied:")
				}
				for _, ch := range changes {
					fmt.Printf("  %s\n", ch)
				}
			}

			// Instance-level NIC overrides win over the profile, so reconciling
			// the profile does not necessarily isolate everything. Those are left
			// alone — an override may be deliberate — but they are worth naming.
			instances, err := a.c.Instances()
			if err != nil {
				return err
			}
			var stragglers []string
			for i := range instances {
				unisolated, noEgress := policy.Report(&instances[i], a.cfg.ACL)
				if len(unisolated) > 0 {
					stragglers = append(stragglers, fmt.Sprintf("  %s: no ACL on %s", instances[i].Name, join(unisolated)))
				} else if len(noEgress) > 0 {
					stragglers = append(stragglers, fmt.Sprintf("  %s: no egress on %s", instances[i].Name, join(noEgress)))
				}
			}
			if len(stragglers) > 0 {
				fmt.Println("\ninstances not covered (instance-level NIC overrides win over the profile; not changed):")
				for _, s := range stragglers {
					fmt.Println(s)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without changing it")
	cmd.Flags().StringVar(&profile, "profile", envOr("GPUCTL_PROFILE", "default"), "profile to reconcile")
	return cmd
}

func join(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
