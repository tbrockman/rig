# An X11 desktop session on the passed-through card, for a VM whose monitor
# and peripherals a person sits at — or reaches over the network with a
# Moonlight client, when Sunshine is switched on.
#
# Opt-in. The base image stays headless; a project that wants a screen
# imports `rig.nixosModules.desktop` from its guest flake:
#
#   nixosConfigurations.gpubase = rig.lib.mkGuest [ rig.nixosModules.desktop ./guest.nix ];
#
# and sets the options below in guest.nix. Importing the module enables it.
#
# Why X11 and not Wayland: the VM has two GPUs, the card and the virtual
# screen Incus gives every VM for its console, and inside QEMU the virtual one
# is the boot VGA. A Wayland compositor picks the boot VGA and renders in
# software; the NixOS nvidia X driver configuration binds only NVIDIA devices,
# so X lands on the card with nothing to persuade. The virtual screen is kept:
# `incus console --type=vga` on the host is the fallback viewer when the
# monitor is on another input, and it opens no network path.
#
# What to expect at the monitor: nothing until the guest's driver loads. The
# VM's firmware has no option ROM for the card, so it cannot draw on it, and
# the screen stays black from power-on until lightdm starts.

{ config, lib, pkgs, ... }:

let
  cfg = config.rig.desktop;
in
{
  options.rig.desktop = {
    enable = lib.mkEnableOption "an X11 desktop session on the passed-through card";

    user = lib.mkOption {
      type = lib.types.str;
      default = "operator";
      description = ''
        The account the session runs as. Not root: a root desktop is refused
        by most display managers and is a bad idea in the rest. The agent
        still runs as root; it reaches this session's display through the
        user's Xauthority, which root can read.
      '';
    };

    autoLogin = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Log the user in as soon as X is up, so there is something on the
        monitor when the driver comes up and nobody has to type a password
        into an unattended machine.
      '';
    };

    input.enable = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Accept the host's keyboard and mouse from `rig host input <vm>`: a
        daemon listening on vsock that replays them through uinput, so the
        guest sees virtual devices and never the hardware. The host's
        keyboard and mouse have vendor configuration interfaces a guest could
        use to program them; passed by identity instead, a guest could store
        a macro that later types into the host.
      '';
    };

    sunshine = {
      enable = lib.mkEnableOption "Sunshine, so a Moonlight client can use this session remotely";

      nvenc = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = ''
          Build Sunshine with CUDA so it encodes on the card. Without it the
          stream is encoded in software, which works but costs the CPU the
          agent's build wanted. The CUDA build is not in the public binary
          cache, so the first image build compiles Sunshine and takes a while.
        '';
      };

      package = lib.mkOption {
        type = lib.types.package;
        default =
          if cfg.sunshine.nvenc
          then pkgs.sunshine.override { cudaSupport = true; }
          else pkgs.sunshine;
        defaultText = lib.literalExpression "pkgs.sunshine, with cudaSupport when nvenc is set";
        description = ''
          The Sunshine to run. A project pins a newer release here when the
          one in the pinned nixpkgs has an advisory against it; see the desk
          project's guest.nix for the shape of that override.
        '';
      };
    };
  };

  config = lib.mkIf cfg.enable {
    services.xserver.enable = true;
    services.xserver.displayManager.lightdm.enable = true;

    # No boot menu. With a keyboard redirected into the VM, the firmware sees
    # it during boot, and a USB keyboard initialising tends to send a stray
    # key event — which stops systemd-boot's countdown, and the VM then sits
    # at the menu until someone at the monitor presses Enter. Seen for real.
    boot.loader.timeout = lib.mkDefault 0;

    # Point X at the card by bus address, found at boot.
    #
    # Without a BusID, X hands a driver only the boot VGA device, and inside
    # QEMU that is the virtual screen — so the nvidia driver reported "No
    # devices detected" with a working card one slot over. The card's guest
    # address depends on how Incus orders the VM's devices, so it is read from
    # sysfs at boot rather than written into the image, and X is started with
    # a layout that names exactly that device and nothing else.
    systemd.services.rig-xorg-card = {
      description = "Point X at the passed-through card";
      wantedBy = [ "display-manager.service" ];
      before = [ "display-manager.service" ];
      after = [ "systemd-udev-settle.service" ];
      serviceConfig.Type = "oneshot";
      script = ''
        addr=""
        for d in /sys/bus/pci/devices/*; do
          [ "$(cat "$d/vendor")" = 0x10de ] || continue
          case "$(cat "$d/class")" in 0x0300*) addr=$(basename "$d") ;; esac
        done
        if [ -z "$addr" ]; then
          echo "no NVIDIA display controller in this VM; X has nothing to drive" >&2
          exit 1
        fi
        # 0000:07:00.0 -> PCI:7:0:0; X wants decimal.
        bus=$((16#''${addr:5:2})); dev=$((16#''${addr:8:2})); fn=''${addr:11:1}
        mkdir -p /run/rig-xorg
        cat > /run/rig-xorg/10-card.conf <<EOF
        Section "Device"
          Identifier "rig-card"
          Driver "nvidia"
          BusID "PCI:$bus:$dev:$fn"
        EndSection
        Section "Screen"
          Identifier "rig-screen"
          Device "rig-card"
        EndSection
        Section "ServerLayout"
          Identifier "rig"
          Screen "rig-screen"
        EndSection
        EOF
        echo "X will drive $addr (PCI:$bus:$dev:$fn)"
      '';
    };
    # Into NixOS's own config directory, not one of ours: replacing the
    # directory with `-configdir` also dropped the libinput InputClass rules
    # that live there, and X then logged "No input driver specified" for
    # every keyboard and mouse while the desktop looked fine.
    environment.etc."X11/xorg.conf.d/10-rig-card.conf".source = "/run/rig-xorg/10-card.conf";
    services.xserver.displayManager.xserverArgs = [ "-layout" "rig" ];
    services.xserver.desktopManager.xfce = {
      enable = true;
      # An unattended VM's screen must not lock or blank: the person who
      # switches the monitor to it wants to see what the agent is doing, and
      # a locked screen with no password set is a wall.
      enableScreensaver = false;
    };
    services.displayManager.defaultSession = "xfce";
    services.displayManager.autoLogin = lib.mkIf cfg.autoLogin {
      enable = true;
      user = cfg.user;
    };
    # The power button is how a VM is asked to stop, and a desktop session
    # takes it away: xfce4-power-manager holds a handle-power-key inhibitor
    # in "block" mode and does nothing useful with the key, so `rig stop`
    # waited out its whole timeout while the guest logged "Power key pressed
    # short" and kept running with the host's devices inside it.
    #
    # PowerKeyIgnoreInhibited does not help, whatever it looks like: it
    # overrides only the high-level shutdown/sleep locks, and a low-level
    # handle-power-key lock is always honoured (logind.conf(5)). This was
    # set here for a while and every stop was still forced (2026-09-23).
    # So the power manager is not in the session at all: a VM has no
    # battery, no lid and no backlight, and blanking is off below anyway.
    environment.xfce.excludePackages = [ pkgs.xfce4-power-manager ];
    services.logind.settings.Login.HandlePowerKey = "poweroff";

    # No DPMS: a monitor that has gone to sleep looks exactly like a card that
    # never came up, and this project has enough of those already.
    services.xserver.serverFlagsSection = ''
      Option "BlankTime" "0"
      Option "StandbyTime" "0"
      Option "SuspendTime" "0"
      Option "OffTime" "0"
    '';

    users.users.${cfg.user} = {
      isNormalUser = true;
      description = "desktop session";
      # uinput: Sunshine injects a remote client's keyboard and mouse through
      # /dev/uinput, which is root:uinput 0660. Without the group the stream
      # works and nothing the client does reaches the desktop, and Sunshine
      # only says so as a warning in its own log.
      extraGroups = [ "wheel" "video" "audio" "input" "uinput" "docker" ];
    };
    users.groups.uinput = { };
    # The guest is the sandbox; nothing inside it is protected from a person
    # sitting at its keyboard. Asking that person for a password they were
    # never given only locks them out of their own VM.
    security.sudo.wheelNeedsPassword = false;

    # DisplayPort audio goes to the guest with the card's audio function.
    services.pipewire = {
      enable = true;
      pulse.enable = true;
      alsa.enable = true;
    };

    environment.systemPackages = with pkgs; [
      firefox      # for the person at the monitor, and for Sunshine's pairing page
      xclip
    ];

    # The guest half of `rig host input`; see base/rig-input/main.go.
    boot.kernelModules = lib.mkIf cfg.input.enable [ "uinput" ];
    systemd.services.rig-input = lib.mkIf cfg.input.enable {
      description = "Replay the host's keyboard and mouse, sent over vsock by rig host input";
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        ExecStart = "${pkgs.buildGoModule {
          pname = "rig-input";
          version = "1";
          src = ./rig-input;
          # Standard library only. In the rig checkout it is part of rig's own
          # module (go:embed will not reach into a nested one), so it carries
          # no go.mod of its own; this is the whole of one.
          postPatch = "printf 'module rig-input\\n\\ngo 1.22\\n' > go.mod";
          vendorHash = null;
          env.CGO_ENABLED = "0";
        }}/bin/rig-input";
        Restart = "always";
        RestartSec = 2;
        # It needs /dev/uinput and a vsock socket, and nothing else.
        DevicePolicy = "closed";
        DeviceAllow = [ "/dev/uinput rw" ];
        RestrictAddressFamilies = [ "AF_VSOCK" ];
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        ProtectKernelTunables = true;
        ProtectControlGroups = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
      };
    };

    services.sunshine = lib.mkIf cfg.sunshine.enable {
      enable = true;
      autoStart = true;
      # The guest's own firewall. The host side — Incus forwarding the ports
      # to this VM and the isolation ACL letting them in — is `ports:` in the
      # project's rig.yaml; both are needed and neither can set the other.
      openFirewall = true;
      # X11 capture needs no KMS access; keep the capability off.
      capSysAdmin = false;
      package = cfg.sunshine.package;
    };
  };
}
