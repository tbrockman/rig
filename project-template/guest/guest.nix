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

    # Run one command inside the project's devShell.
    #
    # In systemPackages, so it lands in /run/current-system/sw/bin — which is on
    # BOTH the login shell's PATH and the agent unit's. Installed by hand the
    # only writable directory reachable is /usr/bin, which is on the unit's PATH
    # and not the operator's, so the same command worked for the agent and not
    # for the human watching it.
    #
    # No `path:` prefix, and that is the whole point. A `path:` flakeref copies
    # the WHOLE directory into the store, ignoring .gitignore, on every
    # invocation — so a project with a 19G `target/` writes 19G to /nix/store
    # each time you run a build. It fills the disk, and the failure arrives as
    # ENOSPC from an unrelated command long after the cause. Seen for real: ten
    # copies of one repo, 163 GiB, on a 197G VM.
    #
    # A bare path inside a git working tree is resolved as a git tree instead,
    # which honours .gitignore. If the directory is NOT a git repo, nix falls
    # back to copying everything again — so `git init` is load-bearing here.
    (writeShellScriptBin "dev" ''
      set -euo pipefail
      dir="''${PROJECT_DIR:-/work/project}"
      if [ ! -e "$dir/.git" ]; then
        echo "dev: $dir is not a git working tree." >&2
        echo "  nix would copy the entire directory into /nix/store on every" >&2
        echo "  build, .gitignore and all. Run: git -C $dir init" >&2
        exit 78
      fi
      exec ${pkgs.nix}/bin/nix develop "$dir" -c "$@"
    '')
  ];

  # Directories the brief tells the agent exist — here, one for it to hand
  # results back through. Told, in a document it re-reads every restart: if
  # the directory is missing it burns a turn discovering that and creating it,
  # every time, until someone fixes the document or the machine.
  systemd.tmpfiles.rules = [
    "d /work/handoff 0755 root root -"
  ];

  # With rig.nixosModules.desktop in the flake's module list, the session on
  # the card is configured here. Sunshine lets a Moonlight client use it from
  # another machine; its ports then belong in rig.yaml's `ports:` as well.
  #
  # rig.desktop.user = "operator";
  # rig.desktop.sunshine.enable = true;
}
