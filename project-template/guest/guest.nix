# What this project needs the guest machine to have.
#
# Everything here is system configuration — the things a devShell cannot give
# you because they are not on PATH, they are the machine: daemons, directories
# that must exist at boot, and wrappers that have to be found by name from both
# an operator's login shell and the agent's systemd unit.
{ pkgs, ... }:

{
  environment.systemPackages = with pkgs; [
    # The unattended agent. In the system profile rather than installed by hand
    # with `nix profile install`: claude-code is unfree, and a flake reference
    # does not read ~/.config/nixpkgs/config.nix, so the by-hand form needs both
    # NIXPKGS_ALLOW_UNFREE=1 and --impure and fails confusingly without them.
    # `nixpkgs.config.allowUnfree` in the base module covers it here.
    claude-code

    # Bridges a guest TCP port to a host vsock listener. Incus proxy devices
    # cannot do this for VMs — bind=instance is container-only — so vsock is the
    # only way to reach a host-side service without opening the network ACL.
    socat

    # Run one command inside the project's devShell.
    #
    # In systemPackages, so it lands in /run/current-system/sw/bin — which is on
    # BOTH the login shell's PATH and the agent unit's. Installed by hand the
    # only writable directory reachable is /usr/bin, which is on the unit's PATH
    # and not the operator's, so the same command worked for the agent and not
    # for the human watching it.
    (writeShellScriptBin "dev" ''
      set -euo pipefail
      exec ${pkgs.nix}/bin/nix develop "path:''${PROJECT_DIR:-/work/project}" -c "$@"
    '')
  ];

  # Directories the agent is told exist. Told, in a document it re-reads every
  # restart — so if one is missing it burns a turn discovering that and creating
  # it, every time, until someone fixes the document or the machine.
  systemd.tmpfiles.rules = [
    "d /work/handoff 0755 root root -"
  ];
}
