# The rig guest base: what every VM gets, and nothing a VM might not want.
#
# Imported by base/flake.nix alongside the nixpkgs VM-image module, which
# supplies the bootloader and filesystem layout. A guest that needs the NVIDIA
# driver, docker or a desktop adds rig.nixosModules.nvidia, .docker or
# .desktop in its own guest flake.

{ lib, pkgs, ... }:

{
  # Enables `rig exec`, `rig shell`, `rig push` and the rest: the Incus agent
  # over vsock, a channel the host opens and the guest cannot.
  virtualisation.incus.agent.enable = true;

  # No sshd. The NIC rejects ingress, so nothing could reach one; a listener
  # that can never be reached is surface without a purpose.

  environment.systemPackages = with pkgs; [
    pciutils
    git

    # The guest half of `rig forward`. It is rig's own dependency, not a
    # project's: a rig verb that fails on a fresh guest until someone installs
    # a package by hand is the provisioning-by-memory this image exists to end.
    socat
    # `rig doctor` proves a proxied credential's host is reached through the
    # proxy by asking curl whose certificate it saw.
    curl
  ];

  # The proxy NIC, for a manifest that proxies credentials (guest.proxy). rig
  # gives it a MAC of its own, so it is proxy0 whichever slot it lands in,
  # and rig sets its address at each start: the host pins that address to
  # the NIC, so there is nothing to negotiate, and DHCP on it would only add
  # a route to a bridge that leads to the host proxy and nowhere else.
  services.udev.extraRules = ''
    SUBSYSTEM=="net", ACTION=="add", ATTR{address}=="02:52:49:47:*", NAME="proxy0"
  '';
  networking.dhcpcd.denyInterfaces = [ "proxy0" ];

  nix.settings.experimental-features = [ "nix-command" "flakes" ];
  nix.settings.trusted-users = [ "root" "@wheel" ];

  # The default destination of `rig push`, `rig mount` and `rig agent`.
  systemd.tmpfiles.rules = [ "d /work 0755 root root -" ];

  system.stateVersion = lib.mkDefault "26.05";
}
