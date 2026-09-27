# NixOS module: the clow daemon as a systemd service.
#
# The flake imports this as nixosModules.default and injects its own
# `self`, so `package` defaults to the flake build with no specialArgs.
# The daemon auto-inits a fresh cat when identity.json is missing, so an
# empty StateDirectory boots straight into a new cat named after the
# hostname (or `name`).
{ self, config, lib, pkgs, ... }:
let
  cfg = config.services.clowder;
in
{
  options.services.clowder = with lib; {
    enable = mkEnableOption "the clowder file-transfer daemon";

    package = mkOption {
      type = types.package;
      default = self.packages.${pkgs.system}.default;
      description = "The clow package to run.";
    };

    name = mkOption {
      type = types.nullOr types.str;
      default = null;
      description = "Cat name (CLOWDER_NAME); defaults to the hostname.";
    };

    storer = mkOption {
      type = types.nullOr (types.enum [ "on" "off" "dropbox" ]);
      default = null;
      description = ''
        Storer role (CLOWDER_STORER). Null leaves the role alone so a
        role set with `clow` survives restarts.
      '';
    };

    maxCapacity = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "10G";
      description = "Storer/dropbox capacity (CLOWDER_MAX), e.g. \"10G\".";
    };

    healthAddr = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "127.0.0.1:8080";
      description = ''
        HTTP health endpoint (CLOWDER_HEALTH_ADDR) serving GET /healthz
        and a /stats JSON snapshot for external watchdogs. Null disables.
      '';
    };

    derpMapUrl = mkOption {
      type = types.nullOr types.str;
      default = null;
      description = "Self-hosted DERP map URL (CLOWDER_DERPMAP_URL).";
    };
  };

  config = lib.mkIf cfg.enable {
    users.groups.clowder = { };
    users.users.clowder = {
      isSystemUser = true;
      group = "clowder";
      description = "Clowder daemon user";
    };

    systemd.services.clowder = {
      description = "Clowder e2e-encrypted file-transfer daemon";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];

      # The CLI speaks to the daemon through this socket; run it as the
      # clowder user:
      #   sudo -u clowder env CLOWDER_DIR=/var/lib/clowder clow status
      environment =
        {
          CLOWDER_DIR = "/var/lib/clowder";
        }
        // lib.optionalAttrs (cfg.name != null) { CLOWDER_NAME = cfg.name; }
        // lib.optionalAttrs (cfg.storer != null) { CLOWDER_STORER = cfg.storer; }
        // lib.optionalAttrs (cfg.maxCapacity != null) { CLOWDER_MAX = cfg.maxCapacity; }
        // lib.optionalAttrs (cfg.healthAddr != null) {
          CLOWDER_HEALTH_ADDR = cfg.healthAddr;
        }
        // lib.optionalAttrs (cfg.derpMapUrl != null) {
          CLOWDER_DERPMAP_URL = cfg.derpMapUrl;
        };

      serviceConfig = {
        User = "clowder";
        Group = "clowder";
        StateDirectory = "clowder";
        # No $HOME for this user, so the inbox falls back to
        # /var/lib/clowder/inbox (daemon.InboxDir), spool-safe under the
        # same directory systemd manages.
        ExecStart = "${cfg.package}/bin/clow daemon";
        Restart = "on-failure";
        RestartSec = "5s";
        # No WatchdogSec: the watchdog clock counts suspend time, so a
        # resumed machine would SIGABRT a perfectly healthy daemon.
        # Hang detection stays with external probes of /healthz.
      };
    };
  };
}
