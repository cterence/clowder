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
# mapAttrs', never mapAttrsToList: the module system walks each
# module's config structure to collect definitions, and attrValues
# would force the instances' values into that walk — infinite
# recursion.
{ self, config, lib, pkgs, ... }:
let
  shared = import ./options.nix { inherit lib pkgs self; };
  instances = config.services.clowder.instances;
  enabled = lib.filterAttrs (n: c: c.enable) instances;
in
{
  options.services.clowder.instances = lib.mkOption {
    type = lib.types.attrsOf (lib.types.submodule { options = shared.options; });
    default = { };
    description = "Clowder instances: one daemon and config dir per local clowder name.";
  };

  config = {
    launchd.user.agents = lib.mapAttrs' (name: cfg: lib.nameValuePair "clowder-${name}" {
      command = "${cfg.package}/bin/clow daemon";
      environment = shared.env cfg // { CLOWDER = name; };
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
    }) enabled;
  };
}
