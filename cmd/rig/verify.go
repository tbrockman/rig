package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"rig/internal/verify"
)

func (a *app) verifyCmd() *cobra.Command {
	var asJSON bool
	var allowGaps []string
	var bridge string

	cmd := &cobra.Command{
		Use:     "verify <name>",
		GroupID: "vm",
		Short:   "Prove the isolation holds, by probing from inside the guest",
		Long: "Sends real traffic from the guest and reports what it could and could\n" +
			"not reach.\n\n" +
			"`doctor` reads configuration; this proves the effect. Two things it\n" +
			"refuses to call a pass: a target the host cannot reach either (guest\n" +
			"failure would prove nothing), and a probe that did not run (a missing\n" +
			"binary looks exactly like a refused connection).\n\n" +
			"Exit status: 0 proven, 1 a property is violated, 2 could not be proven.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.requireInstance(name); err != nil {
				return err
			}
			addr, err := verify.BridgeAddress(a.c, name, bridge)
			if err != nil {
				return err
			}

			out := os.Stdout
			r := &verify.Runner{
				C: a.c, Instance: name, ACL: a.cfg.ACL, Bridge: addr,
				AllowGaps: allowGaps, Out: out,
			}
			if asJSON {
				r.Out = nil
			}
			report, err := r.Run()
			if err != nil {
				return err
			}

			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return err
				}
			} else {
				printCoverage(report)
			}
			if code := report.ExitCode(); code != 0 {
				return &exitCodeError{code: code, msg: report.Summary}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().StringVar(&bridge, "bridge", "", "bridge network name (default: the one the instance's NIC uses)")
	cmd.Flags().StringSliceVar(&allowGaps, "allow-gap", verify.DefaultAllowGaps,
		"reject ranges accepted as unprovable here; still reported, but they do not make the run inconclusive")
	return cmd
}

func printCoverage(r *verify.Report) {
	fmt.Println("\n--- coverage: every declared reject range needs one attributable proof ---")
	for _, cov := range r.Coverage {
		switch {
		case len(cov.Proofs) > 0:
			fmt.Printf("  ok    %-18s %d proof(s): %s\n", cov.Range, len(cov.Proofs), cov.Proofs[0])
		case cov.Acknowledged:
			fmt.Printf("  gap   %-18s acknowledged: nothing here is reachable from this host\n", cov.Range)
		default:
			fmt.Printf("  GAP   %-18s NOT PROVEN — no reachable target in this range\n", cov.Range)
		}
	}

	fmt.Printf("\n=== %s: %s ===\n", strings.ToUpper(string(r.Verdict)), r.Summary)
	switch r.Verdict {
	case verify.Inconclusive:
		fmt.Println("unproven is not a pass: those checks could not distinguish a blocked")
		fmt.Println("guest from a broken probe, so they say nothing either way.")
	case verify.Violated:
		fmt.Println("the guest reached something it must not, or could not reach something")
		fmt.Println("it needs. Do not hand this VM to an unattended agent.")
	}
}
