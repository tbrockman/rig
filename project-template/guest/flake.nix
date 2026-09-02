# A per-project guest image: the rig base, plus what this project needs the
# *machine* to have.
#
#   rig image build --flake ./guest --alias myproj-guest
#   rig new myproj --image myproj-guest --env secrets/myproj.env --start
#
# This is optional. A project that needs nothing beyond the base should not have
# one — `rig new` uses the default image and that is the right answer.
#
# Reach for it when you would otherwise run setup commands against a running
# guest by hand. Anything you would `nix profile install`, drop into a directory
# on PATH, or mkdir at boot belongs here instead, because a VM assembled by
# remembered commands is a VM nothing describes. Toolchains still belong in the
# project's own devShell flake: this is for the machine, not the build.
{
  description = "Project guest image";

  # Point this at your rig checkout. Absolute, because once this directory is
  # copied into a project the relative path to rig no longer means anything.
  inputs.rig.url = "path:/home/theo/dev/rig/base";

  # `rig image build` builds nixosConfigurations.<attr>, default `gpubase`, so
  # that single output is the whole contract.
  outputs = { rig, ... }: {
    nixosConfigurations.gpubase = rig.lib.mkGuest [ ./guest.nix ];
  };
}
