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

  # --- Docker --------------------------------------------------------------
  # A daemon, so it cannot come from a project flake the way every other
  # toolchain here does: enabling it needs system configuration, and on NixOS
  # that means the image. This is the one exception to "the base image carries
  # the driver and nothing else", and it is what lets a project bring up its own
  # dependencies — a database, an object store, an identity provider — the same
  # `docker compose` way it would anywhere else.
  #
  # Nothing here weakens the isolation. Docker's bridges live inside the guest,
  # so container traffic never crosses the instance's NIC and never meets the
  # ACL; only image pulls do, and those go to the public internet like any other
  # egress.
  #
  # Socket-activated, never started on boot. That is what keeps this honest as a
  # base-image entry: a VM that never speaks docker never runs a daemon, gets no
  # docker0 bridge and carries no extra running surface, so the cost of shipping
  # it to every project is disk in one image that every instance CoW-shares.
  # The trade is that containers declaring `restart: unless-stopped` do not come
  # back by themselves after a guest reboot — whatever brought them up has to be
  # run again, which for a project stack is one `just up`.
  virtualisation.docker.enable = true;
  virtualisation.docker.enableOnBoot = false;

  # Move docker's bridges off their defaults, which are not a free choice here.
  # Docker picks 172.17.0.1/16 for docker0 on every machine it has ever run on,
  # and allocates compose networks from 172.17.0.0/12 — so a host running docker
  # and a guest running docker hold the *same addresses*. That collision is not
  # a routing problem (neither side's docker traffic crosses the guest's NIC);
  # it is a measurement problem. `rig verify` probes the host's addresses from
  # inside the guest, and a probe at an address the guest also holds loops back
  # to the guest's own sshd — which verify read as the guest reaching the host,
  # its most serious verdict, from a probe that never touched the wire.
  #
  # 10.201/10.202 rather than another 172.x: docker's own default pool covers
  # 172.17–172.31, so any 172 choice can collide again the moment the host
  # creates one more compose network. Both stay inside a range the ACL rejects,
  # which is correct — a guest's docker bridges have no business leaving it.
  virtualisation.docker.daemon.settings = {
    bip = "10.201.0.1/16";
    default-address-pools = [{ base = "10.202.0.0/16"; size = 24; }];
  };

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
    docker-compose
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
  #
  # The compose plugin is symlinked into root's own plugin directory rather than
  # left to the system profile. `docker compose` (the subcommand) searches a
  # fixed list of directories that does not include /run/current-system/sw, so
  # the package being installed is not enough: without this, `docker-compose`
  # works and `docker compose` does not, which is the spelling every project's
  # own tooling uses.
  systemd.tmpfiles.rules = [
    "d /work 0755 root root -"
    "d /root/.docker 0700 root root -"
    "d /root/.docker/cli-plugins 0700 root root -"
    "L+ /root/.docker/cli-plugins/docker-compose - - - - ${pkgs.docker-compose}/libexec/docker/cli-plugins/docker-compose"
  ];

  system.stateVersion = lib.mkDefault "26.05";
}
