// Command hostgpu moves the card between host desktop use and VM use.
//
// Assumes the console is on the iGPU (HDMI), the card is on DisplayPort, and the
// host default target is multi-user.target. Needs root for the sysfs writes.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"rig/internal/gpu"
	"rig/internal/incus"
)

type conf struct {
	gpuPCI   string
	audioPCI string
	dm       string
	client   *incus.Client
}

func main() {
	c := &conf{
		gpuPCI:   os.Getenv("GPU_PCI"),
		audioPCI: os.Getenv("GPU_AUDIO_PCI"),
		client:   incus.New(""),
	}

	root := &cobra.Command{
		Use:   "hostgpu",
		Short: "Move the GPU between the host desktop and VMs",
		Long: "Assumes the console is on the iGPU and the card is on DisplayPort.\n" +
			"Needs root for the sysfs writes.\n\n" +
			"Environment: GPU_PCI, GPU_AUDIO_PCI.",
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: c.resolve,
	}
	root.PersistentFlags().StringVar(&c.dm, "display-manager", "gdm", "display manager unit")
	root.AddCommand(
		&cobra.Command{
			Use: "status", Short: "Where the card is", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error { return c.status() },
		},
		&cobra.Command{
			Use: "desktop", Short: "Reclaim the card for the host and start the desktop", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error { return c.toDesktop() },
		},
		&cobra.Command{
			Use: "headless", Short: "Stop the desktop and free the card for VMs", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error { return c.toHeadless() },
		},
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "hostgpu: %v\n", err)
		os.Exit(1)
	}
}

func (c *conf) resolve(*cobra.Command, []string) error {
	if c.gpuPCI == "" {
		pci, err := gpu.DiscoverPCI(c.client, gpu.ConfigFromEnv())
		if err != nil {
			return err
		}
		c.gpuPCI = pci
	}
	if c.audioPCI == "" {
		// The audio function is the same device at function 1 and has to move
		// with the GPU: they share an IOMMU group.
		c.audioPCI = strings.TrimSuffix(c.gpuPCI, ".0") + ".1"
	}
	return nil
}

func (c *conf) status() error {
	fmt.Printf("GPU   %s   driver: %s\n", c.gpuPCI, driver(c.gpuPCI))
	fmt.Printf("audio %s   driver: %s\n", c.audioPCI, driver(c.audioPCI))
	fmt.Printf("override: %s\n", readFile(sysfs(c.gpuPCI, "driver_override")))
	fmt.Printf("%s: %s\n", c.dm, run("systemctl", "is-active", c.dm))

	instances, err := c.client.Instances()
	if err != nil {
		fmt.Println("instances: (incus unreachable)")
		return nil
	}
	holders := gpu.Holders(instances)
	if len(holders) == 0 {
		fmt.Println("instances holding a gpu device: none")
		return nil
	}
	fmt.Println("instances holding a gpu device:")
	for _, h := range holders {
		fmt.Printf("  %s (%s)\n", h.Instance, h.Status)
	}
	return nil
}

func (c *conf) toDesktop() error {
	// Refuse while a VM still has the card configured: the rebind would race
	// with Incus and both sides lose.
	if instances, err := c.client.Instances(); err == nil {
		if holders := gpu.Holders(instances); len(holders) > 0 {
			return fmt.Errorf("%s still holds the GPU. Run:  rig stop %s && rig release",
				holders[0].Instance, holders[0].Instance)
		}
	}

	if driver(c.gpuPCI) == "vfio-pci" {
		fmt.Println("rebinding to nvidia...")
		// Order matters: unload stale modules while the card is still on
		// vfio-pci, or the reloaded module grabs it mid-transition and mismatches.
		run("modprobe", "-r", "nvidia_drm", "nvidia_modeset", "nvidia_uvm", "nvidia")
		writeSysfs("/sys/bus/pci/drivers/vfio-pci/unbind", c.gpuPCI)
		writeSysfs("/sys/bus/pci/drivers/vfio-pci/unbind", c.audioPCI)
		for _, addr := range []string{c.gpuPCI, c.audioPCI} {
			writeSysfs(sysfs(addr, "driver_override"), "\n")
		}
		if out := run("modprobe", "nvidia"); out != "" {
			fmt.Println(out)
		}
		writeSysfs("/sys/bus/pci/drivers_probe", c.audioPCI)
	}

	fmt.Println("starting desktop...")
	if err := exec.Command("systemctl", "start", c.dm).Run(); err != nil {
		return fmt.Errorf("starting %s: %w", c.dm, err)
	}
	fmt.Println("\nSwitch your monitor to the DisplayPort input.")
	fmt.Println("(The HDMI/iGPU console stays available on tty1.)")
	return c.status()
}

func (c *conf) toHeadless() error {
	if err := exec.Command("systemctl", "stop", c.dm).Run(); err != nil {
		return fmt.Errorf("stopping %s: %w", c.dm, err)
	}
	if nodes, _ := filepath.Glob("/dev/nvidia*"); len(nodes) > 0 {
		if out := run("fuser", append([]string{"-v"}, nodes...)...); strings.TrimSpace(out) != "" {
			fmt.Fprintf(os.Stderr, "warning: something still holds /dev/nvidia*:\n%s\n", out)
		}
	}
	fmt.Println("desktop stopped; card is free for VMs.")
	return c.status()
}

// --- sysfs helpers -------------------------------------------------------

func sysfs(pci, leaf string) string { return filepath.Join("/sys/bus/pci/devices", pci, leaf) }

func driver(pci string) string {
	target, err := os.Readlink(sysfs(pci, "driver"))
	if err != nil {
		return "(none)"
	}
	return filepath.Base(target)
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeSysfs(path, value string) {
	if err := os.WriteFile(path, []byte(value), 0o200); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "warning: writing %s: %v\n", path, err)
	}
}

func run(name string, args ...string) string {
	out, _ := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out))
}
