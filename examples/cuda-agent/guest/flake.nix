# The example's guest image: rig's base, the NVIDIA driver, and the agent.
{
  description = "cuda-agent guest image";

  # Resolves inside a rig checkout. In your own project, point it at rig:
  #   inputs.rig.url = "github:tbrockman/rig?dir=base";
  inputs.rig.url = "path:../../../base";

  outputs = { rig, ... }: {
    nixosConfigurations.guest = rig.lib.mkGuest [ rig.nixosModules.nvidia ./guest.nix ];
  };
}
