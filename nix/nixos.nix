# NixOS module: one systemd service per clowder instance.
#
# The flake imports this as nixosModules.default and injects its own
# `self`, so `package` defaults to the flake build with no specialArgs.
# The daemon auto-inits a fresh cat when identity.json is missing, so an
# empty StateDirectory boots straight into a new cat named after the
# hostname (or `name`). A clowder is one instance under
# services.clowder.instances; the key is the local clowder name
# (CLOWDER=<name>, dir /var/lib/clowder/<name>).
{ self, config, lib, pkgs, ... }:
let
  shared = import ./options.nix { inherit lib pkgs self; };
  instances = config.services.clowder.instances;
in
{
  options.services.clowder.instances = lib.mkOption {
    type = lib.types.attrsOf (lib.types.submodule shared.options);
    default = { };
    example = {
      work.enable = true;
      family = {
        enable = true;
        storer = "on";
        maxCapacity = "10G";
      };
    };
    description = "Clowder instances: one daemon and config dir per local clowder name.";
  };

  config = lib.mkMerge (
    [ (lib.mkIf (instances != { }) {
      users.groups.clowder = { };
      users.users.clowder = {
        isSystemUser = true;
        group = "clowder";
        description = "Clowder daemon user";
      };
    }) ]
    ++ lib.mapAttrsToList (name: cfg: lib.mkIf cfg.enable {
      systemd.services."clowder-${name}" = {
        description = "Clowder e2e-encrypted file-transfer daemon (${name})";
        wantedBy = [ "multi-user.target" ];
        after = [ "network-online.target" ];
        wants = [ "network-online.target" ];

        # The CLI speaks to the daemon through this socket; run it as the
        # clowder user:
        #   sudo -u clowder env CLOWDER_DIR=/var/lib/clowder CLOWDER=${name} clow status
        environment = shared.env cfg // {
          CLOWDER_DIR = "/var/lib/clowder";
          CLOWDER = name;
          # System users get HOME=/var/empty from passwd; without this the
          # daemon's default inbox lands there and mkdir fails. HOME at
          # the state base keeps the per-clowder inboxes under what
          # systemd manages: /var/lib/clowder/Downloads/clowder/<name>.
          HOME = "/var/lib/clowder";
        };

        serviceConfig = {
          User = "clowder";
          Group = "clowder";
          StateDirectory = "clowder/${name}";
          ExecStart = "${cfg.package}/bin/clow daemon";
          Restart = "on-failure";
          RestartSec = "5s";
          # No WatchdogSec: the watchdog clock counts suspend time, so a
          # resumed machine would SIGABRT a perfectly healthy daemon.
          # Hang detection stays with external probes of /healthz.
        };
      };
    }) instances
  );
}
