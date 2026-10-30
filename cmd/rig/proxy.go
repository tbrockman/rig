package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tbrockman/rig/internal/agent"
	"github.com/tbrockman/rig/internal/creds"
	"github.com/tbrockman/rig/internal/incus"
	"github.com/tbrockman/rig/internal/manifest"
	"github.com/tbrockman/rig/internal/policy"
	"github.com/tbrockman/rig/internal/ports"
	"github.com/tbrockman/rig/internal/proxy"
)

// Credentials the guest never holds: see internal/proxy for the design. This
// file is the lifecycle. `rig proxy setup` readies the host once: the bridge
// in Incus, and a check that witmproxy is here and can run as the operator's.
// `rig start` and `rig creds` split the env file into what the guest gets
// (stand-ins) and what the proxy gets (the real values), make the guest's
// proxy if it does not exist yet, start it, give it the plugin, the rules and
// a policy for that guest alone, and hand the guest the proxy's certificate;
// `rig stop` stops the proxy with the VM, `rig rm` removes it with the VM.
// Each proxy is a witmproxy instance in the operator's user scope: no root
// for any of this.

// proxyPreflight is what `rig start` checks before claiming anything, for a VM
// whose record asks for the proxy: that every piece exists. Found here, with
// the VM still stopped, rather than once it is up with no credentials.
func (a *app) proxyPreflight(inst *incus.Instance) error {
	if inst.Config[proxy.InstanceKey] == "" {
		return nil
	}
	if inst.Config[creds.InstanceKey] == "" {
		return fmt.Errorf("%s asks for proxied credentials but has no credential file recorded.\n"+
			"  rig apply -f with env_file set, or rig start %s --env <file>", inst.Name, inst.Name)
	}
	if policy.ProxyIPOf(inst) == "" {
		return fmt.Errorf("%s asks for proxied credentials but has no NIC on %s.\n"+
			"  rig apply -f <its rig.yaml> gives it one; the VM must be stopped for that", inst.Name, policy.ProxyNetwork)
	}
	if _, err := a.c.Network(policy.ProxyNetwork); err != nil {
		return fmt.Errorf("the proxy network %s does not exist on this host:  rig proxy setup", policy.ProxyNetwork)
	}
	if err := proxy.WitmHasUserScope(); err != nil {
		return err
	}
	if on, known := proxy.Linger(); known && !on {
		note("WARNING: your session manager does not linger, so %s's proxy stops when you log out:  rig proxy setup", inst.Name)
	}
	return nil
}

// injectCredentials is what every start, and `rig creds`, ends with: the env
// file into the guest. With proxied credentials in the record, the guest's
// copy carries stand-ins for them and the guest's proxy is told the real
// values and where they may go; the guest is handed the proxy and the
// certificate it needs to trust. Returns how many variables the guest got.
func (a *app) injectCredentials(name, envFile string) (int, error) {
	inst, _, err := a.c.Instance(name)
	if err != nil {
		return 0, err
	}
	content, err := creds.Prepare(envFile)
	if err != nil {
		return 0, err
	}
	credentials, err := proxy.Decode(inst.Config[proxy.InstanceKey])
	if err != nil {
		return 0, err
	}
	if len(credentials) == 0 {
		// A manifest that no longer proxies anything: whatever the proxy
		// held for this VM goes, and the guest's copy is the file as it is.
		a.proxyRevoke(inst)
		return creds.Write(a.c, name, content)
	}
	ip := policy.ProxyIPOf(inst)
	if ip == "" {
		return 0, fmt.Errorf("%s has no NIC on %s; rig apply -f gives it one", inst.Name, policy.ProxyNetwork)
	}
	pi := proxy.InstanceFor(inst.Name, ip)
	// The harness says how the agent trusts an extra certificate; the rest
	// of the environment is the same for every tool.
	h, err := agent.ForInstance(inst.Config)
	if err != nil {
		return 0, err
	}
	// A guest already running holds stand-ins; they stay, so a value swapped
	// with rig creds reaches a running agent with nothing in the guest
	// changed. At a start the file is not there yet, and every stand-in is new.
	keep := map[string]string{}
	if inst.Running() {
		if out, err := a.c.Exec(name, proxy.GuestStandIns(creds.GuestPath), incus.ExecOpts{Timeout: 15 * time.Second}); err == nil {
			keep = proxy.StandInsIn(out)
		}
	}
	guestEnv, rules, err := proxy.Split(content, credentials, keep)
	if err != nil {
		return 0, err
	}
	guestEnv = append(guestEnv, proxy.GuestEnv(pi.ProxyURL(), h.TrustEnv(proxy.GuestCA, proxy.GuestBundle))...)
	if err := a.proxyGrant(inst, pi, rules); err != nil {
		return 0, err
	}
	if _, err := creds.Write(a.c, name, guestEnv); err != nil {
		return 0, err
	}
	note("proxied, so the guest holds a stand-in and its proxy the value: %s", proxy.Describe(rules))
	// The count is the file's: what rig added for the proxy is not a
	// credential, and saying fourteen for two misleads.
	return creds.CountIn(content), nil
}

// proxyGrant gives a VM its proxy and the proxy its rules: the instance made
// if it is not yet, its policy written (the hosts the manifest names, from
// this guest, and nothing else), started, rig's plugin in it with the rules,
// and the proxy's certificate in the guest.
func (a *app) proxyGrant(inst *incus.Instance, pi proxy.Instance, rules []proxy.Rule) error {
	if _, err := policy.EnsureProxyACL(a.c, inst.Name, pi.IP, false); err != nil {
		return err
	}
	if !pi.Installed() {
		password, err := proxy.NewPassword()
		if err != nil {
			return err
		}
		if err := pi.Install(password); err != nil {
			return fmt.Errorf("making %s's proxy: %w", inst.Name, err)
		}
		note("made %s's proxy: witmproxy instance %s, listening at %s", inst.Name, pi.Name(), pi.ProxyBind())
	}
	changed, err := pi.WritePolicy(rules, inst.Config[proxy.EgressKey])
	if err != nil {
		return err
	}
	switch {
	case !pi.Running():
		if err := pi.Start(); err != nil {
			return fmt.Errorf("starting %s's proxy: %w", inst.Name, err)
		}
	case changed:
		// A running proxy reads its policy at start only.
		if err := pi.Restart(); err != nil {
			return fmt.Errorf("restarting %s's proxy for its new policy: %w", inst.Name, err)
		}
	}
	c, err := pi.Connect()
	if err != nil {
		return err
	}
	if err := c.InstallPlugin(proxy.Plugin()); err != nil {
		return fmt.Errorf("installing %s in %s's proxy: %w", proxy.PluginID, inst.Name, err)
	}
	if err := c.SetRules(proxy.RulesJSON(rules)); err != nil {
		return err
	}
	ca, err := pi.ReadCA()
	if err != nil {
		return err
	}
	if err := a.c.Mkdir(inst.Name, proxy.GuestCADir, 0o755); err != nil {
		return err
	}
	if err := a.c.WriteFile(inst.Name, proxy.GuestCA, ca, 0o644); err != nil {
		return err
	}
	if out, err := a.c.Exec(inst.Name, proxy.GuestSetup(pi.IP), incus.ExecOpts{Timeout: 60 * time.Second}); err != nil {
		return fmt.Errorf("setting the proxy up in %s: %w\n%s", inst.Name, err, strings.TrimSpace(out))
	}
	return nil
}

// proxyRevoke stops a VM's proxy with the VM: the rules leave the running
// process first, then the process ends. The instance stays, for the next
// start. A guest still running (rig creds to a manifest that no longer
// proxies) loses the certificate too. Quiet for a VM that never proxied
// anything.
func (a *app) proxyRevoke(inst *incus.Instance) { a.proxyRelease(inst, false) }

// proxyRemove takes a VM's proxy away with the VM: the unit, the state (its
// database with the credentials it held, its CA, its log), and the ACL its
// NIC used.
func (a *app) proxyRemove(inst *incus.Instance) { a.proxyRelease(inst, true) }

func (a *app) proxyRelease(inst *incus.Instance, remove bool) {
	pi := proxy.InstanceFor(inst.Name, policy.ProxyIPOf(inst))
	if inst.Config[proxy.InstanceKey] == "" && !pi.Installed() && !remove {
		return
	}
	if pi.Installed() && pi.Running() {
		if c, err := pi.Connect(); err == nil {
			if err := c.SetRules("[]"); err != nil {
				note("WARNING: could not take %s's credentials out of its proxy: %v", inst.Name, lastLine(err.Error()))
			}
		}
		if err := pi.Stop(); err != nil {
			note("WARNING: could not stop %s's proxy: %v", inst.Name, lastLine(err.Error()))
		} else {
			note("stopped %s's proxy; its credentials left with it", inst.Name)
		}
	}
	if inst.Running() && a.c.Exists(inst.Name) {
		_, _ = a.exec(inst.Name, proxy.GuestTeardown())
	}
	if !remove {
		return
	}
	if pi.Installed() {
		if err := pi.Uninstall(); err != nil {
			note("WARNING: could not remove %s's proxy: %v", inst.Name, lastLine(err.Error()))
			note("         By hand:  witm service uninstall --user --instance %s -y", pi.Name())
		} else {
			note("removed %s's proxy and everything it held", inst.Name)
		}
	}
	if err := policy.DeleteProxyACL(a.c, inst.Name); err != nil {
		note("WARNING: could not remove ACL %s: %v", policy.ProxyACLName(inst.Name), err)
	}
}

// --- the verb ----------------------------------------------------------------

func (a *app) proxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "proxy",
		GroupID: "guest",
		Short:   "The host proxies that hold credentials for the guests",
		Long: "A manifest's guest.proxy.credentials names variables from its env_file\n" +
			"that the guest never sees. The guest gets a stand-in; a witmproxy on this\n" +
			"host, running rig's plugin for that guest alone, puts the real value into\n" +
			"HTTPS requests to the endpoints named, and into nothing else. A guest that\n" +
			"leaks everything it holds leaks a string worth nothing anywhere but there.\n\n" +
			"Each guest has a proxy of its own: a witmproxy instance in your user scope\n" +
			"(no root), made at the guest's first start, stopped and removed with the\n" +
			"guest, with a policy that lets the guest reach the hosts its manifest names\n" +
			"and nothing else; with guest.proxy.egress: internet, everything its own NIC\n" +
			"may reach, this host and the LAN still refused, for the tools under the\n" +
			"agent that inherit its HTTPS_PROXY. The guest reaches it over a second NIC on a bridge of\n" +
			"rig's own, which leads to that proxy and nowhere else; the isolation ACL on\n" +
			"its first NIC is untouched and `rig verify` still holds. `rig proxy setup`\n" +
			"makes the host ready once.",
	}
	cmd.AddCommand(a.proxySetupCmd(), a.proxyStatusCmd(), a.proxyLogCmd())
	return cmd
}

func (a *app) proxySetupCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Ready this host: the proxy network, witmproxy able to run as you, lingering, the firewall",
		Long: "Creates the bridge " + policy.ProxyNetwork + " (" + policy.ProxySubnet() + ", no NAT, no routing) in\n" +
			"Incus, and checks that `witm` is on PATH and can install user services. Then\n" +
			"what the proxies need of the host. It lets your session manager linger, so\n" +
			"they outlive a logout and start at boot: yours to grant on systemd 256 and\n" +
			"later, no root involved; an older logind asks an admin, and rig prints the\n" +
			"root form instead. And where ufw is active it admits the proxies' ports on\n" +
			"the bridge, under sudo, printing the rule first; ufw skips a rule it has.\n" +
			"The guests' proxies themselves are made by rig start, one per guest. Safe to\n" +
			"run again; --dry-run says what it would do and does none of it.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			changes, err := policy.ApplyProxy(a.c, dryRun)
			for _, ch := range changes {
				fmt.Printf("  %s\n", ch)
			}
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				note("network %s is as rig wants it", policy.ProxyNetwork)
			}
			if err := proxy.WitmHasUserScope(); err != nil {
				return err
			}
			witm, _ := proxy.Witm()
			note("witmproxy at %s can run as you (--user)", witm)
			if on, known := proxy.Linger(); !known {
				note("could not tell whether your session manager lingers; without it the proxies stop when you log out:  %s", proxy.LingerHint())
			} else if on {
				note("your session manager lingers: the proxies outlive a logout and start at boot")
			} else if dryRun {
				note("would let your session manager linger (loginctl enable-linger), so the proxies outlive a logout and start at boot")
			} else if err := proxy.EnableLinger(); err != nil {
				note("could not let your session manager linger as you: %s", lastLine(err.Error()))
				note("  Once, as root:  %s", proxy.LingerHint())
			} else {
				note("your session manager now lingers: the proxies outlive a logout and start at boot")
			}
			// The host's own firewall drops a guest's traffic to the host's
			// address on the new bridge; the one rule that admits it is root's.
			if ports.HostFirewall() {
				if dryRun {
					note("ufw is active: would admit the guests' way to their proxies:  %s", proxy.UFWRule())
				} else if err := proxy.AllowUFW(); err != nil {
					return err
				}
			}
			if dryRun {
				return nil
			}
			note("next: guest.proxy.credentials in a rig.yaml, then rig apply -f and rig start; rig doctor <vm> checks it all")
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change in Incus")
	return cmd
}

func (a *app) proxyStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "The proxy network, witmproxy, and each guest's proxy: installed, running, what it holds",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			row := func(k, v string) { fmt.Printf("  %-12s %s\n", k, v) }
			if _, err := a.c.Network(policy.ProxyNetwork); err != nil {
				row("network", policy.ProxyNetwork+" missing:  rig proxy setup")
			} else {
				row("network", policy.ProxyNetwork+" ("+policy.ProxySubnet()+"; the proxies at "+policy.ProxyAddr+", ports "+policy.ProxyPortRange()+")")
			}
			if err := proxy.WitmHasUserScope(); err != nil {
				row("witmproxy", lastLine(err.Error()))
			} else {
				witm, _ := proxy.Witm()
				row("witmproxy", witm+" (user scope)")
			}
			if on, known := proxy.Linger(); known && !on {
				row("linger", "off: the proxies stop when you log out;  rig proxy setup")
			} else if known {
				row("linger", "on")
			}
			if ports.HostFirewall() {
				if n := proxy.RecentlyBlocked(); n > 0 {
					row("firewall", fmt.Sprintf("ufw dropped %d packet(s) from guests to their proxies in the last 10 minutes;  rig proxy setup admits them", n))
				} else {
					row("firewall", "ufw is active; no drops from guests in the last 10 minutes (rig proxy setup admits their proxy ports)")
				}
			}
			instances, err := a.c.Instances()
			if err != nil {
				return err
			}
			seen := map[string]bool{}
			any := false
			for i := range instances {
				inst := &instances[i]
				record, err := proxy.Decode(inst.Config[proxy.InstanceKey])
				if err != nil || len(record) == 0 {
					continue
				}
				any = true
				ip := policy.ProxyIPOf(inst)
				pi := proxy.InstanceFor(inst.Name, ip)
				seen[pi.Name()] = true
				detail := proxy.DescribeRecord(record)
				if inst.Config[proxy.EgressKey] == manifest.EgressInternet {
					detail += "; egress: internet"
				}
				switch {
				case ip == "":
					detail += "; no NIC on " + policy.ProxyNetwork + ":  rig apply -f, with the VM stopped"
				case !pi.Installed():
					detail += "; proxy not made yet (rig start does)"
				case pi.Running():
					detail += "; proxy running at " + pi.ProxyBind()
				default:
					detail += "; proxy stopped"
					if inst.Running() {
						detail += " while the VM runs:  rig creds " + inst.Name + " <env-file>"
					}
				}
				row(inst.Name, detail)
			}
			if !any {
				row("guests", "none proxies credentials")
			}
			// An instance with no VM: rig rm takes a proxy away with its
			// VM, so this is a VM deleted some other way.
			for _, name := range proxy.InstalledVMs() {
				if !seen[instancePrefixed(name)] {
					row("orphan", instancePrefixed(name)+": no VM named "+name+" (witm service uninstall --user --instance "+instancePrefixed(name)+" -y)")
				}
			}
			return nil
		},
	}
}

func instancePrefixed(vm string) string { return proxy.InstanceFor(vm, "").Name() }

func (a *app) proxyLogCmd() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:   "log <vm>",
		Short: "What a guest's proxy said: every connection it carried and every dial its policy refused",
		Long:  "Runs `witm service logs` for the guest's witmproxy instance.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.requireInstance(args[0]); err != nil {
				return err
			}
			inst, _, err := a.c.Instance(args[0])
			if err != nil {
				return err
			}
			pi := proxy.InstanceFor(inst.Name, policy.ProxyIPOf(inst))
			if !pi.Installed() {
				return fmt.Errorf("%s has no proxy yet; rig start makes it when the manifest proxies credentials", inst.Name)
			}
			return pi.Logs(follow, lines)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing as it happens")
	cmd.Flags().IntVarP(&lines, "lines", "n", 100, "how many recent lines")
	return cmd
}
