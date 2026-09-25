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
      devShells = forEachSupportedSystem (
        { pkgs }:
        {
          default = pkgs.mkShell {
            # tailcat's go.mod requires a newer toolchain than nixpkgs
            # ships; let go fetch it on first use.
            GOTOOLCHAIN = "auto";
            packages = with pkgs; [
              go
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
