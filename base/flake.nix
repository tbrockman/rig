# The rig guest image, and the modules a project's guest flake builds on.
#
#   rig image build                                          # -> rig-base
#   rig image build --attr guest-nvidia --alias rig-nvidia   # with the NVIDIA driver
#
# The image is a build artifact, not a snapshot of a VM someone logged into.
# Everything a guest needs that is not a toolchain (a daemon, a directory, a
# wrapper on PATH) is system configuration, and on NixOS that means the image.
{
  description = "rig guest image for Incus";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";

      # mkGuest builds a guest from this base plus a project's modules.
      #
      # The project's modules are evaluated against *this* flake's nixpkgs, not
      # one the project pins itself: the NVIDIA kernel module has to match the
      # kernel, so a guest built from two nixpkgs would be a driver mismatch
      # waiting to happen. A project's toolchain still comes from its own
      # devShell flake, where skew is harmless.
      mkGuest = modules: nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [
          # Bootloader, filesystem layout, and the image outputs.
          "${nixpkgs}/nixos/modules/virtualisation/incus-virtual-machine.nix"
          ./base.nix
        ] ++ modules;
      };
    in {
      nixosModules = {
        default = ./base.nix;
        nvidia = ./nvidia.nix;
        docker = ./docker.nix;
        # An X11 desktop on a passed-through NVIDIA card, with Sunshine as an
        # option. Importing it enables it; see desktop.nix for what it assumes.
        desktop = {
          imports = [ ./nvidia.nix ./desktop.nix ];
          rig.desktop.enable = nixpkgs.lib.mkDefault true;
        };
      };

      lib = { inherit mkGuest; };

      # `rig image build` builds nixosConfigurations.<attr>, `guest` unless
      # told otherwise, so a project's guest flake needs exactly that output.
      nixosConfigurations = {
        guest = mkGuest [ ];
        guest-nvidia = mkGuest [ ./nvidia.nix ];
      };
    };
}
