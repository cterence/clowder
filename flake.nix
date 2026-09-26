{
  description = "Clowder: an async file-transfer mesh over tailcat";

  inputs.nixpkgs.url = "nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forEachSupportedSystem =
        f:
        nixpkgs.lib.genAttrs supportedSystems (
          system:
          f {
            pkgs = import nixpkgs { inherit system; };
          }
        );
    in
    {
      # The clow binary. go.mod requires go >= 1.27.1 (tailcat and
      # tailscale.com declare it and own the floor), so the build pins
      # nixpkgs' go_1_27 rather than the default `go` alias, which may
      # still be a release behind.
      packages = forEachSupportedSystem (
        { pkgs }:
        {
          # buildGoModule's `go` attribute does not reach the
          # go-modules fetch derivation; overriding the builder swaps
          # the toolchain everywhere, module fetch included.
          default = (pkgs.buildGoModule.override { go = pkgs.go_1_27; }) {
            pname = "clow";
            version = "unstable-2026-09-26";
            src = self;
            vendorHash = "sha256-B0NZyZgmJqKRNZ+9iHPPM1LBdx5UpVxkdEkOIlllXTc=";
            # go names the binary after the module; users type clow.
            postInstall = ''
              mv $out/bin/clowder $out/bin/clow
            '';
          };
        }
      );

      devShells = forEachSupportedSystem (
        { pkgs }:
        {
          default = pkgs.mkShell {
            # GOTOOLCHAIN=auto lets go fetch a newer toolchain than the
            # shell's on first use, should go.mod ever demand one.
            GOTOOLCHAIN = "auto";
            packages = with pkgs; [
              go_1_27
              gotools
              gopls
              golangci-lint
              go-tools # staticcheck
              prek
              git
            ];
            shellHook = ''
              # Install the prek-managed pre-commit hooks (gofmt, vet,
              # golangci-lint) into .git/hooks.
              prek install --config .pre-commit-config.yaml
            '';
          };
        }
      );
    };
}
