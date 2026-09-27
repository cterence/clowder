# nix-darwin module: the clow daemon as the primary user's launchd
# agent (KeepAlive + RunAtLoad, so it survives suspend/resume without
# restarts). nix-darwin activation installs and loads the plist on
# switch and unloads/removes it when dropped — no manual copying.
#
# The flake imports this as darwinModules.default and injects its own
# `self`, so `package` defaults to the flake build with no specialArgs.
# The agent runs with the user's own home, so the daemon's defaults
# apply unchanged (config in ~/Library/Application Support, inbox in
# ~/Downloads/clowder).
{ self, config, lib, pkgs, ... }:
let
  shared = import ./options.nix { inherit lib pkgs self; };
  cfg = config.services.clowder;
in
{
  options.services.clowder = shared.options;

  config = lib.mkIf cfg.enable {
    launchd.user.agents.clowder = {
      command = "${cfg.package}/bin/clow daemon";
      environment = shared.env cfg;
      serviceConfig =
        {
          RunAtLoad = true;
          KeepAlive = true;
          ProcessType = "Background";
        }
        // lib.optionalAttrs (config.system.primaryUser != null) {
          # launchd does not expand ~; without a path it discards logs.
          StandardOutPath = "/Users/${config.system.primaryUser}/Library/Logs/clowder.log";
          StandardErrorPath = "/Users/${config.system.primaryUser}/Library/Logs/clowder.log";
        };
    };
  };
}
