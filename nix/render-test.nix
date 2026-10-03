# Consumer-render gate: imports the NixOS module into a real NixOS
# evaluation and forces the rendered unit — the probe that catches
# what drvPath-level checks cannot (options declared under config,
# config that forces the very option merge it feeds). Run in CI by
# the nix job.
#
# Usage:
#   nix eval --raw --impure --expr \
#     '(let f = builtins.getFlake "path:$(pwd)"; in
#        import ./nix/render-test.nix { pkgs = import f.inputs.nixpkgs {}; self = f; })'
#
# The Home Manager and nix-darwin modules share this module shape and
# are exercised by a real consumer at switch time; a standalone HM
# host would need home-manager's option set, which is not a clowder
# input.
{
  pkgs,
  self,
}:
let
  nixos = import "${pkgs.path}/nixos/lib/eval-config.nix" {
    system = pkgs.stdenv.hostPlatform.system;
    modules = [
      self.nixosModules.default
      {
        services.clowder.instances.default.enable = true;
        services.clowder.instances.default.package = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      }
    ];
  };
  exec = nixos.config.systemd.services.clowder-default.serviceConfig.ExecStart;
in
if !nixos.config.systemd.services ? "clowder-default" then
  throw "clowder nixos module rendered no unit for the default instance"
else if exec == null then
  throw "clowder nixos unit has no ExecStart"
else
  "ok nixos ${exec}"
