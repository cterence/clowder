{
  description = "Clowder: an async file-transfer mesh over tailcat";

  inputs = {
    nixpkgs.url = "nixpkgs/nixos-unstable";
    # The Android SDK as a flake. Only the android devShell depends on
    # it; the Go devshell and the clow package do not.
    android-nixpkgs = {
      url = "github:tadfisher/android-nixpkgs";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      android-nixpkgs,
    }:
    let
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      # android-nixpkgs supports x86_64-linux and aarch64-darwin only
      # (no aarch64-linux), so the android shells are generated over
      # this subset.
      androidSystems = [
        "x86_64-linux"
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
      forEachAndroidSystem =
        f:
        nixpkgs.lib.genAttrs androidSystems (
          system:
          f {
            pkgs = import nixpkgs { inherit system; };
            android-sdk = android-nixpkgs.sdk.${system};
            inherit system;
          }
        );
      # The system image arch the emulator runs per host: on Apple
      # Silicon it must be arm64-v8a — the daemon is arm64-only, an
      # x86_64 image could not exec it.
      imageArch = {
        "x86_64-linux" = "x86-64";
        "aarch64-darwin" = "arm64-v8a";
      };
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

      # NixOS: services.clowder, a systemd unit wrapping the flake
      # package. The wrapper's pattern must name the standard module
      # args (pkgs at minimum) — NixOS only injects them into
      # pattern-named arguments — and `self` rides along so the module
      # needs no specialArgs.
      nixosModules.default =
        { pkgs, ... }@args: import ./nix/nixos.nix (args // { inherit self; });

      # nix-darwin: services.clowder, a per-user launchd agent. Same
      # wrapper pattern as nixosModules.
      darwinModules.default =
        { pkgs, ... }@args: import ./nix/darwin.nix (args // { inherit self; });

      # Home Manager: services.clowder, a launchd agent on macOS, a
      # systemd user unit on Linux. Same wrapper pattern as
      # nixosModules.
      homeManagerModules.default =
        { pkgs, ... }@args: import ./nix/home-manager.nix (args // { inherit self; });

      # Merge the per-system shell sets INSIDE each system, never
      # with a top-level `//`: that merge is shallow, the android set
      # once replaced devShells.<system>.default wholesale, and
      # `nix develop` then silently fell back to
      # packages.<system>.default — the buildGoModule clow
      # derivation — as the "environment", whose go-module setup hook
      # exports GOFLAGS="-mod=vendor -trimpath" and
      # GOTOOLCHAIN=local, breaking every go command in a repo that
      # does not vendor.
      devShells =
        let
          goShells = forEachSupportedSystem (
            { pkgs }:
            {
            default = pkgs.mkShell {
              # GOTOOLCHAIN=auto lets go fetch a newer toolchain than
              # the shell's on first use, should go.mod ever demand one.
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
                # Install the prek-managed pre-commit hooks (gofmt,
                # vet, golangci-lint) into .git/hooks.
                prek install --config .pre-commit-config.yaml
              '';
            };
          }
          );
          androidShells = forEachAndroidSystem (
            {
              pkgs,
              android-sdk,
              system,
            }:
            {
          # The Android app shell: nix develop .#android. The SDK
          # matches android/app/build.gradle.kts (compileSdk 34,
          # build-tools 34.0.0, AGP 8.5.2, JDK 17) — bump them
          # together. Everything needed to build the APK without
          # Android Studio: SDK, JDK, gradle (no wrapper jar is
          # checked in), plus go so build-native.sh works here too.
          android =
            let
              sdk = android-sdk (
                sdkPkgs: with sdkPkgs; [
                  cmdline-tools-latest
                  platform-tools
                  build-tools-34-0-0
                  platforms-android-34
                ]
              );
            in
            pkgs.mkShell rec {
              packages = [
                sdk
                pkgs.jdk17
                pkgs.gradle
                pkgs.go_1_27
              ];
              ANDROID_HOME = "${sdk}/share/android-sdk";
              ANDROID_SDK_ROOT = "${sdk}/share/android-sdk";
              # AGP downloads a prebuilt aapt2 from Maven that does not
              # run on Nix (unpatched ELF); point it at the SDK's own.
              GRADLE_OPTS = "-Dorg.gradle.project.android.aapt2FromMavenOverride=${ANDROID_HOME}/build-tools/34.0.0/aapt2";
              JAVA_HOME = pkgs.jdk17.home;
            };

          # The emulator shell: nix develop .#emulator. Same SDK as
          # the android shell plus the emulator and one system image
          # (~1.5 GB extra — hence its own shell). On Apple Silicon
          # the emulator runs the arm64 image under
          # Hypervisor.framework and can exec the app's arm64-only
          # daemon. Create the AVD once (it lives in ~/.android):
          #   avdmanager create avd -n clowder \
          #     -k "system-images;android-34;default;${imageArch.${system}}"
          # then run it:
          #   emulator -avd clowder
          emulator =
            let
              image = "system-images-android-34-default-${imageArch.${system}}";
              sdk = android-sdk (
                sdkPkgs: with sdkPkgs; [
                  cmdline-tools-latest
                  platform-tools
                  build-tools-34-0-0
                  platforms-android-34
                  emulator
                  sdkPkgs.${image}
                ]
              );
            in
            pkgs.mkShell {
              packages = [ sdk pkgs.jdk17 ];
              ANDROID_HOME = "${sdk}/share/android-sdk";
              ANDROID_SDK_ROOT = "${sdk}/share/android-sdk";
              JAVA_HOME = pkgs.jdk17.home;
            };
          }
          );
        in
        nixpkgs.lib.mapAttrs (
          system: go: go // (androidShells.${system} or { })
        ) goShells;
    };
}
