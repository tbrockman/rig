# A per-project guest image: the rig base, plus what this project needs the
# *machine* to have.
#
#   rig image build --flake ./guest --alias myproj-guest
#   rig new myproj --image myproj-guest --env ~/.config/rig/myproj.env --start
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

  # The rig base image. Relative, and it resolves because this directory and
  # base/ are in one git tree: `rig image build` hands nix a bare path inside a
  # git checkout, and nix reads that through git, where ../../base means
  # something. (Behind a `path:` ref it would not — the directory is copied into
  # the store first and the relative path is resolved against the copy.)
  #
  # When you copy this directory into your own project, point it at rig itself:
  #   inputs.rig.url = "github:<owner>/rig?dir=base";
  #   inputs.rig.url = "path:/absolute/path/to/a/rig/checkout/base";
  inputs.rig.url = "path:../../base";

  # `rig image build` builds nixosConfigurations.<attr>, default `gpubase`, so
  # that single output is the whole contract.
  #
  # For a VM someone sits at — a monitor on the card, its peripherals passed
  # through — add rig's desktop module, and set its options in guest.nix:
  #   rig.lib.mkGuest [ rig.nixosModules.desktop ./guest.nix ]
  outputs = { rig, ... }: {
    nixosConfigurations.gpubase = rig.lib.mkGuest [ ./guest.nix ];
  };
}
