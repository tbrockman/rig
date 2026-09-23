# This VM's image: rig's base, plus what the project needs the machine to
# have. `rig apply -f rig.yaml` builds it (as <name>-guest) when it is missing.
{
  description = "Project guest image";

  # rig's base. `rig init` points this at the rig that wrote it.
  inputs.rig.url = "path:../../base";

  # `rig image build` builds nixosConfigurations.guest; that is the whole
  # contract. rig's optional modules go in the list:
  #   rig.nixosModules.nvidia    the NVIDIA driver, for a `kind: gpu` device
  #   rig.nixosModules.docker    docker and compose, socket-activated
  #   rig.nixosModules.desktop   an X11 session on the card (brings nvidia)
  outputs = { rig, ... }: {
    nixosConfigurations.guest = rig.lib.mkGuest [ ./guest.nix ];
  };
}
