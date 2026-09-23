# Docker, for a guest whose project brings up its own dependencies (a
# database, an object store) with `docker compose`. A daemon cannot come from
# a project's devShell, so it is a module for the image.
#
#   rig.lib.mkGuest [ rig.nixosModules.docker ./guest.nix ]
#
# Nothing here weakens the isolation: docker's bridges live inside the guest,
# so container traffic never crosses the instance's NIC; only image pulls do.

{ pkgs, ... }:

{
  # Socket-activated, never started on boot, so a VM that never speaks docker
  # runs no daemon. The trade: containers declaring `restart: unless-stopped`
  # do not come back by themselves after a guest reboot.
  virtualisation.docker.enable = true;
  virtualisation.docker.enableOnBoot = false;

  # Off docker's defaults, which are not a free choice here. Docker picks
  # 172.17.0.1/16 on every machine, so a host and a guest both running docker
  # hold the same address, and `rig verify` probing the host's 172.17.0.1 from
  # inside the guest loops back to the guest itself and reads as a breach.
  # 10.201/10.202 rather than another 172.x, because docker's own pool covers
  # 172.17-172.31. Both stay inside a range the ACL rejects.
  virtualisation.docker.daemon.settings = {
    bip = "10.201.0.1/16";
    default-address-pools = [{ base = "10.202.0.0/16"; size = 24; }];
  };

  environment.systemPackages = [ pkgs.docker-compose ];

  # `docker compose` (the subcommand) searches a fixed list of plugin
  # directories that does not include the system profile, so the package alone
  # gives `docker-compose` but not `docker compose`.
  systemd.tmpfiles.rules = [
    "d /root/.docker 0700 root root -"
    "d /root/.docker/cli-plugins 0700 root root -"
    "L+ /root/.docker/cli-plugins/docker-compose - - - - ${pkgs.docker-compose}/libexec/docker/cli-plugins/docker-compose"
  ];
}
