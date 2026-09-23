# What this project needs the guest machine to have: packages on PATH,
# daemons, directories that must exist at boot. Toolchains belong in the
# project's own devShell; this is for the machine.
{ pkgs, ... }:

{
  environment.systemPackages = with pkgs; [
    # htop
  ];

  # With rig.nixosModules.desktop in guest/flake.nix:
  # rig.desktop.user = "me";
}
