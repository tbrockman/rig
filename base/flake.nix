# Declarative base image. The image is a BUILD ARTIFACT, not a snapshot of a VM
# someone logged into. Rebuilding from this flake later produces the same image.
#
#   ./01-build-image.sh
{
  description = "NixOS GPU dev VM base image for Incus";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
    in {
      nixosConfigurations.gpubase = nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [
          # Supplies bootloader, filesystem layout, the Incus guest agent, and
          # the system.build.qemuImage / system.build.metadata outputs.
          "${nixpkgs}/nixos/modules/virtualisation/incus-virtual-machine.nix"
          ./gpu-dev.nix
        ];
      };

      packages.${system} = {
        image-disk =
          self.nixosConfigurations.gpubase.config.system.build.qemuImage;
        image-metadata =
          self.nixosConfigurations.gpubase.config.system.build.metadata;
      };
    };
}
