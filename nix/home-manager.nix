# Home Manager module: one per-user service per clowder instance — a
# launchd agent on macOS, a systemd user unit on Linux. Home Manager
# installs/loads the agent (or unit) on activation and swaps it on
# change.
#
# The flake imports this as homeManagerModules.default and injects its
# own `self`, so `package` defaults to the flake build with no
# specialArgs. The services run with the user's own home, so the
# daemon's defaults apply unchanged (config base under the platform's
# UserConfigDir, per-clowder inbox in ~/Downloads/clowder/<name>). A
# clowder is one instance under services.clowder.instances; the key
# is the local clowder name.
#
# mapAttrs', never mapAttrsToList: the module system walks each
# module's config structure to collect definitions, and attrValues
# would force the instances' values into that walk — infinite
# recursion. mapAttrs' derives the agent keys without forcing the
# per-instance configs.
#
# Do not enable an instance on a machine that also runs a system-level
# instance of the same name: two daemons means two cats per clowder.
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

  config = lib.mkMerge [
    (lib.mkIf pkgs.stdenv.hostPlatform.isDarwin {
      launchd.agents = lib.mapAttrs' (name: cfg: lib.nameValuePair "clowder-${name}" {
        enable = true;
        config = {
          ProgramArguments = [ "${cfg.package}/bin/clow" "daemon" ];
          EnvironmentVariables = shared.env cfg // { CLOWDER = name; };
          RunAtLoad = true;
          KeepAlive = true;
          ProcessType = "Background";
          # launchd does not expand ~; Home Manager knows the home dir.
          StandardOutPath = "${config.home.homeDirectory}/Library/Logs/clowder-${name}.log";
          StandardErrorPath = "${config.home.homeDirectory}/Library/Logs/clowder-${name}.log";
        };
      }) enabled;
    })
    (lib.mkIf (!pkgs.stdenv.hostPlatform.isDarwin) {
      systemd.user.services = lib.mapAttrs' (name: cfg: lib.nameValuePair "clowder-${name}" {
        Unit.Description = "Clowder e2e-encrypted file-transfer daemon (${name})";
        Install.WantedBy = [ "default.target" ];
        Service = {
          ExecStart = "${cfg.package}/bin/clow daemon";
          Environment = lib.mapAttrsToList (n: v: "${n}=${v}") (shared.env cfg // { CLOWDER = name; });
          Restart = "on-failure";
          RestartSec = "5s";
        };
      }) enabled;
    })
  ];
}
