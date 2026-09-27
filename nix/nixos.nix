# NixOS module: the clow daemon as a systemd service.
#
# The flake imports this as nixosModules.default and injects its own
# `self`, so `package` defaults to the flake build with no specialArgs.
# The daemon auto-inits a fresh cat when identity.json is missing, so an
# empty StateDirectory boots straight into a new cat named after the
# hostname (or `name`).
{ self, config, lib, pkgs, ... }:
let
  shared = import ./options.nix { inherit lib pkgs self; };
  cfg = config.services.clowder;
in
{
  options.services.clowder = shared.options;

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
      environment = shared.env cfg // {
        CLOWDER_DIR = "/var/lib/clowder";
        # System users get HOME=/var/empty from passwd; without this the
        # daemon's default inbox lands there and mkdir fails. HOME at
        # the state dir keeps the inbox under what systemd manages:
        # /var/lib/clowder/Downloads/clowder.
        HOME = "/var/lib/clowder";
      };

      serviceConfig = {
        User = "clowder";
        Group = "clowder";
        StateDirectory = "clowder";
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
