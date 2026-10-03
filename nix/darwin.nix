# nix-darwin module: one launchd agent per clowder instance
# (KeepAlive + RunAtLoad, so it survives suspend/resume without
# restarts). nix-darwin activation installs and loads the plist on
# switch and unloads/removes it when dropped — no manual copying.
#
# The flake imports this as darwinModules.default and injects its own
# `self`, so `package` defaults to the flake build with no specialArgs.
# The agents run with the user's own home, so the daemon's defaults
# apply unchanged (config base in ~/Library/Application Support,
# per-clowder inbox in ~/Downloads/clowder/<name>). A clowder is one
# instance under services.clowder.instances; the key is the local
# clowder name.
#
# The per-instance agent lives inside the submodule: a top-level
# config iterating the instances attrset recurses, because
# discharging that config forces the very option merge it contributes
# to. The outer `config` stays reachable by closure where the instance
# needs a parent option (the log path's primaryUser).
{ self, config, lib, pkgs, ... }:
let
  shared = import ./options.nix { inherit lib pkgs self; };

  instance = { name, ... }@args: {
    config = lib.mkIf args.config.enable {
      launchd.user.agents."clowder-${name}" = {
        command = "${args.config.package}/bin/clow daemon";
        environment = shared.env args.config // { CLOWDER = name; };
        serviceConfig =
          {
            RunAtLoad = true;
            KeepAlive = true;
            ProcessType = "Background";
          }
          // lib.optionalAttrs (config.system.primaryUser != null) {
            # launchd does not expand ~; without a path it discards logs.
            StandardOutPath = "/Users/${config.system.primaryUser}/Library/Logs/clowder-${name}.log";
            StandardErrorPath = "/Users/${config.system.primaryUser}/Library/Logs/clowder-${name}.log";
          };
      };
    };
  };
in
{
  options.services.clowder.instances = lib.mkOption {
    type = lib.types.attrsOf (lib.types.submodule [
      shared.options
      instance
    ]);
    default = { };
    description = "Clowder instances: one daemon and config dir per local clowder name.";
  };
}
