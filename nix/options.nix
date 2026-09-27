# Shared services.clowder definitions: the NixOS, nix-darwin and Home
# Manager modules expose identical knobs and map them to the same
# CLOWDER_* environment, differing only in how the service is declared.
{ lib, pkgs, self }:
let
  inherit (lib) mkEnableOption mkOption optionalAttrs types;
in
{
  options = {
    enable = mkEnableOption "the clowder file-transfer daemon";

    package = mkOption {
      type = types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
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

  # env maps the options to the daemon's CLOWDER_* environment.
  env = cfg:
    { }
    // optionalAttrs (cfg.name != null) { CLOWDER_NAME = cfg.name; }
    // optionalAttrs (cfg.storer != null) { CLOWDER_STORER = cfg.storer; }
    // optionalAttrs (cfg.maxCapacity != null) { CLOWDER_MAX = cfg.maxCapacity; }
    // optionalAttrs (cfg.healthAddr != null) { CLOWDER_HEALTH_ADDR = cfg.healthAddr; }
    // optionalAttrs (cfg.derpMapUrl != null) { CLOWDER_DERPMAP_URL = cfg.derpMapUrl; };
}
