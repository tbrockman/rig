# NixOS guest module for the GPU dev VM base image.
#
# Imported by base/flake.nix alongside the nixpkgs VM-image module, which
# supplies the bootloader and filesystem layout. Build with ./01-build-image.sh.
#
# This is a module rather than a full nixosSystem so the image module owns the
# hardware side and this file owns only the GPU/dev concerns.

{ config, lib, pkgs, ... }:

{
  nixpkgs.config.allowUnfree = true;   # required for the NVIDIA driver

  # --- NVIDIA driver -------------------------------------------------------
  # videoDrivers is the idiomatic switch even on a headless system; it is what
  # pulls in the kernel module, not just the X driver.
  services.xserver.videoDrivers = [ "nvidia" ];

  hardware.graphics.enable = true;     # was hardware.opengl before 24.11

  hardware.nvidia = {
    # Ada (RTX 4080 SUPER) is fully supported by the open kernel modules.
    open = true;
    nvidiaSettings = false;            # no GUI in the guest
    modesetting.enable = true;
    powerManagement.enable = false;    # meaningless for a passed-through card
    package = config.boot.kernelPackages.nvidiaPackages.stable;
  };

  # --- Incus agent ---------------------------------------------------------
  # Enables `incus exec` / `incus file` into this VM.
  virtualisation.incus.agent.enable = true;

  # --- Fail loudly when the GPU is absent ----------------------------------
  # Guards against the silent hot-unplug: if another instance's start pulls the
  # card out from under this VM, a restart surfaces it immediately instead of
  # presenting as inexplicably broken CUDA.
  systemd.services.gpu-present = {
    description = "Assert the passed-through NVIDIA GPU is present";
    wantedBy = [ "multi-user.target" ];
    after = [ "systemd-udev-settle.service" ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
    };
    script = ''
      if ! ${pkgs.pciutils}/bin/lspci -nn \
           | ${pkgs.gnugrep}/bin/grep -q '\[10de:'; then
        echo "FATAL: no NVIDIA PCI device in this VM." >&2
        echo "The GPU was never attached, or was hot-unplugged by another" >&2
        echo "instance starting with the same pci= address." >&2
        exit 1
      fi
    '';
  };

  # Cheap check the host-side tool (and the agent) can call.
  environment.systemPackages = with pkgs; [
    pciutils
    gnugrep
    git
    (writeShellScriptBin "gpu-check" ''
      # writeShellScriptBin provides NO PATH at all. Reference tools by store
      # path, and prepend the system profile for driver binaries: nvidia-smi
      # ships with the driver and is not reachable as a plain pkgs attribute
      # from here.
      export PATH=/run/current-system/sw/bin:$PATH
      set -e

      if ! ${pciutils}/bin/lspci -nn | ${gnugrep}/bin/grep -q '\[10de:'; then
        echo "FAIL: no NVIDIA device on the PCI bus" >&2
        exit 1
      fi
      ${pciutils}/bin/lspci -nn | ${gnugrep}/bin/grep '\[10de:'

      if ! command -v nvidia-smi >/dev/null 2>&1; then
        echo "FAIL: device present but nvidia-smi not found on PATH" >&2
        echo "  PATH=$PATH" >&2
        exit 1
      fi
      nvidia-smi --query-gpu=name,memory.total,driver_version \
                 --format=csv,noheader
    '')
  ];

  # --- Housekeeping --------------------------------------------------------
  nix.settings.experimental-features = [ "nix-command" "flakes" ];
  nix.settings.trusted-users = [ "root" "@wheel" ];

  services.openssh = {
    enable = true;                      # fallback transport if the agent dies
    settings.PasswordAuthentication = false;
  };

  # Project working tree lives here; back it with an Incus disk device so it
  # survives instance rebuilds.
  systemd.tmpfiles.rules = [ "d /work 0755 root root -" ];

  system.stateVersion = lib.mkDefault "26.05";
}
