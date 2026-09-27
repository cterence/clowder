# Home Manager module: the clow daemon as a per-user service — a
# launchd agent on macOS, a systemd user unit on Linux. Home Manager
# installs/loads the agent (or unit) on activation and swaps it on
# change.
#
# The flake imports this as homeManagerModules.default and injects its
# own `self`, so `package` defaults to the flake build with no
# specialArgs. The service runs with the user's own home, so the
# daemon's defaults apply unchanged (config under the platform's
# UserConfigDir, inbox in ~/Downloads/clowder).
#
# Do not enable this on a machine that also runs the system-level
# services.clowder: two daemons means two cats on one host.
{ self, config, lib, pkgs, ... }:
let
  shared = import ./options.nix { inherit lib pkgs self; };
  cfg = config.services.clowder;
  env = shared.env cfg;
in
{
  options.services.clowder = shared.options;

  config = lib.mkMerge [
    (lib.mkIf (cfg.enable && pkgs.stdenv.hostPlatform.isDarwin) {
      launchd.agents.clowder = {
        enable = true;
        config = {
          ProgramArguments = [ "${cfg.package}/bin/clow" "daemon" ];
          EnvironmentVariables = env;
          RunAtLoad = true;
          KeepAlive = true;
          ProcessType = "Background";
          # launchd does not expand ~; Home Manager knows the home dir.
          StandardOutPath = "${config.home.homeDirectory}/Library/Logs/clowder.log";
          StandardErrorPath = "${config.home.homeDirectory}/Library/Logs/clowder.log";
        };
      };
    })
    (lib.mkIf (cfg.enable && !pkgs.stdenv.hostPlatform.isDarwin) {
      systemd.user.services.clowder = {
        Unit.Description = "Clowder e2e-encrypted file-transfer daemon";
        Install.WantedBy = [ "default.target" ];
        Service = {
          ExecStart = "${cfg.package}/bin/clow daemon";
          Environment = lib.mapAttrsToList (n: v: "${n}=${v}") env;
          Restart = "on-failure";
          RestartSec = "5s";
        };
      };
    })
  ];
}
