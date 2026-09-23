# The NVIDIA driver, for a guest given a card (`kind: gpu`).
#
#   rig.lib.mkGuest [ rig.nixosModules.nvidia ./guest.nix ]
#
# The base flake also builds it on its own as nixosConfigurations.guest-nvidia:
#   rig image build --attr guest-nvidia --alias rig-nvidia

{ config, pkgs, ... }:

{
  nixpkgs.config.allowUnfree = true;   # required for the NVIDIA driver

  # videoDrivers is the idiomatic switch even on a headless system; it is what
  # pulls in the kernel module, not just the X driver.
  services.xserver.videoDrivers = [ "nvidia" ];

  hardware.graphics.enable = true;

  hardware.nvidia = {
    # The open kernel modules cover Turing and newer; verified on Ada.
    # For an older card, set this to false.
    open = true;
    nvidiaSettings = false;            # no GUI in the guest
    modesetting.enable = true;
    powerManagement.enable = false;    # meaningless for a passed-through card
    package = config.boot.kernelPackages.nvidiaPackages.stable;
  };

  # Fail loudly when the card is absent. Starting a second VM with the same
  # card hot-unplugs it from the first, which keeps running; a restart then
  # surfaces it here instead of as inexplicably broken CUDA.
  systemd.services.gpu-present = {
    description = "Assert the passed-through NVIDIA GPU is present";
    wantedBy = [ "multi-user.target" ];
    after = [ "systemd-udev-settle.service" ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
    };
    script = ''
      if ! ${pkgs.pciutils}/bin/lspci -nn | ${pkgs.gnugrep}/bin/grep -q '\[10de:'; then
        echo "FATAL: no NVIDIA PCI device in this VM." >&2
        echo "The GPU was never attached, or was hot-unplugged by another" >&2
        echo "instance starting with the same pci= address." >&2
        exit 1
      fi
    '';
  };

  # What `rig doctor` runs to prove the card works in the guest.
  environment.systemPackages = [
    (pkgs.writeShellScriptBin "gpu-check" ''
      # nvidia-smi ships with the driver and is only on the system profile.
      export PATH=/run/current-system/sw/bin:$PATH
      set -e
      if ! ${pkgs.pciutils}/bin/lspci -nn | ${pkgs.gnugrep}/bin/grep '\[10de:'; then
        echo "FAIL: no NVIDIA device on the PCI bus" >&2
        exit 1
      fi
      if ! command -v nvidia-smi >/dev/null 2>&1; then
        echo "FAIL: device present but nvidia-smi not found on PATH" >&2
        exit 1
      fi
      nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv,noheader
    '')
  ];
}
