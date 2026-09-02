# Declarative base image. The image is a BUILD ARTIFACT, not a snapshot of a VM
# someone logged into. Rebuilding from this flake later produces the same image.
#
#   rig image build
#
# It is also the base a project extends. Everything a guest needs that is not a
# toolchain — a daemon, a directory, a wrapper on PATH — is system
# configuration, and on NixOS that means the image. Provisioning such things by
# hand afterwards produces a VM nothing describes, which is the state this file
# exists to prevent. `lib.mkGuest` lets a project add its own modules without
# copying any of the wiring below.
{
  description = "NixOS GPU dev VM base image for Incus";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";

      # mkGuest builds a guest system from this base plus a project's modules.
      #
      # The project's modules are evaluated against *this* flake's nixpkgs, not
      # one the project pins itself. That is deliberate: the NVIDIA kernel
      # module has to match the kernel, so a guest built from two nixpkgs would
      # be a driver mismatch waiting to happen. A project that needs a different
      # package set for its *toolchain* still has its own devShell flake, where
      # skew is harmless.
      mkGuest = modules: nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [
          # Supplies bootloader, filesystem layout, the Incus guest agent, and
          # the system.build.qemuImage / system.build.metadata outputs.
          "${nixpkgs}/nixos/modules/virtualisation/incus-virtual-machine.nix"
          ./gpu-dev.nix
        ] ++ modules;
      };
    in {
      # The base module on its own, for a project that would rather assemble
      # nixosSystem itself than use mkGuest.
      nixosModules.gpu-dev = ./gpu-dev.nix;
      nixosModules.default = ./gpu-dev.nix;

      lib = { inherit mkGuest; };

      # `rig image build` builds nixosConfigurations.<attr>, so a project guest
      # flake needs exactly this one output and nothing else.
      nixosConfigurations.gpubase = mkGuest [ ];

      packages.${system} = {
        image-disk =
          self.nixosConfigurations.gpubase.config.system.build.qemuImage;
        image-metadata =
          self.nixosConfigurations.gpubase.config.system.build.metadata;
      };
    };
}
