# The Android app, built hermetically: nix build .#clowder-android.
#
# The daemon is a nix cross-build (buildGoModule, GOOS=android), the
# gradle/maven caches are one fixed-output fetch, and the APK itself
# builds offline from them. Only hosts with an android-nixpkgs SDK
# composition can build it (x86_64-linux, aarch64-darwin), matching
# the android devShell's system support.
#
# The gradle build signs with the repo's pinned debug keystore
# (android/keystore-debug.keystore), so every build — local, nix, CI
# — produces the same signature and installs over the previous one.
{ pkgs, android-sdk, src, version, vendorHash }:

let
  sdk = android-sdk (sdkPkgs: with sdkPkgs; [
    cmdline-tools-latest
    platform-tools
    build-tools-34-0-0
    platforms-android-34
  ]);

  # The daemon as the app ships it: arm64, packaged as a ".so" so the
  # package manager extracts it to the executable nativeLibraryDir.
  daemon = (pkgs.buildGoModule.override { go = pkgs.go_1_27; }) {
    pname = "clowder-android-daemon";
    inherit version src vendorHash;
    doCheck = false;
    env.CGO_ENABLED = "0";
    # module.nix pins GOOS/GOARCH to the host platform (darwin here);
    # exporting in preBuild — after the env is set, before go build —
    # is what actually crosses to android/arm64.
    preBuild = ''
      export GOOS=android GOARCH=arm64
    '';
    postBuild = ''
      # go puts cross binaries in $GOPATH/bin/android_arm64; module.nix
      # normalizes that only for a cross stdenv, and this one is native.
      mv "$GOPATH/bin/android_arm64"/* "$GOPATH/bin/"
    '';
    postInstall = ''
      install -D $out/bin/clowder $out/lib/arm64-v8a/libclowder.so
      rm $out/bin/clowder
    '';
  };

  gradleEnv = {
    ANDROID_HOME = "${sdk}/share/android-sdk";
    ANDROID_SDK_ROOT = "${sdk}/share/android-sdk";
    JAVA_HOME = pkgs.jdk17.home;
    # AGP downloads a prebuilt aapt2 from Maven that does not run on
    # Nix (unpatched ELF); point it at the SDK's own. Kotlin compiles
    # in-process: no daemon spawns under nix.
    GRADLE_OPTS = "-Dorg.gradle.project.android.aapt2FromMavenOverride=${sdk}/share/android-sdk/build-tools/34.0.0/aapt2 -Dkotlin.compiler.execution.strategy=in-process";
    # The nix daemon may export a stale or missing CA path; gradle's
    # HTTPS fetches deserve a real one.
    NIX_SSL_CERT_FILE = "${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt";
  };

  # One networked build that populates the maven/gradle caches; the
  # APK derivation replays them offline. Only the fetched artifacts
  # (modules-2) are kept: gradle rebuilds transforms and script
  # caches offline, and those embed project-source state, which
  # would churn the hash with every commit. The name must stay
  # constant too: nix keys a fixed-output path by name+hash, so a
  # version-stamped name forces a refetch (and a rehash) per commit.
  gradleDeps = pkgs.stdenv.mkDerivation {
    name = "clowder-gradle-deps";
    inherit src;
    nativeBuildInputs = [ pkgs.jdk17 pkgs.gradle ];
    inherit (gradleEnv) ANDROID_HOME ANDROID_SDK_ROOT JAVA_HOME GRADLE_OPTS NIX_SSL_CERT_FILE;
    buildPhase = ''
      runHook preBuild
      # Deterministic writable homes: AGP insists on creating
      # ~/.android, and the nix build HOME is not writable.
      export HOME=$NIX_BUILD_TOP/home
      export ANDROID_USER_HOME=$HOME/.android
      export GRADLE_USER_HOME=$NIX_BUILD_TOP/gradle-home
      mkdir -p "$HOME" "$ANDROID_USER_HOME" "$GRADLE_USER_HOME"
      # `command gradle` bypasses nixpkgs' gradle wrapper function,
      # which forces --offline (its MITM-cache design): this is the
      # one build that needs the network.
      command gradle --no-daemon --console=plain -p android assembleDebug
      runHook postBuild
    '';
    installPhase = ''
      mkdir -p $out/caches
      cp -r "$GRADLE_USER_HOME/caches/modules-2" $out/caches/
    '';
    outputHashAlgo = "sha256";
    outputHashMode = "recursive";
    outputHash = "sha256-ezGwrN+5lhlpKJVyB5sxWvKaX0RjZMYqhqjY/gXDpH4=";
  };
in
{
  # The deps fetch, exposed for CI caching and hash updates.
  inherit gradleDeps;

  # The daemon cross-build, for build-native.sh's devshell iteration
  # loop; clowder-android wires it in itself.
  clowder-android-daemon = daemon;

  # nix build .#clowder-android → the debug APK itself as the output
  # (a single-file output, like a fetchurl): result IS the apk.
  clowder-android = pkgs.stdenv.mkDerivation {
    name = "clowder-android-${version}.apk";
    inherit src;
    nativeBuildInputs = [ pkgs.jdk17 pkgs.gradle ];
    inherit (gradleEnv) ANDROID_HOME ANDROID_SDK_ROOT JAVA_HOME GRADLE_OPTS NIX_SSL_CERT_FILE;
    buildPhase = ''
      runHook preBuild
      export HOME=$NIX_BUILD_TOP/home
      export ANDROID_USER_HOME=$HOME/.android
      export GRADLE_USER_HOME=$NIX_BUILD_TOP/gradle-home
      mkdir -p "$HOME" "$ANDROID_USER_HOME" "$GRADLE_USER_HOME"
      cp -r "${gradleDeps}/caches" "$GRADLE_USER_HOME/caches"
      # The copy carries the store's read-only mode; gradle writes
      # lock files into its caches.
      chmod -R u+w "$GRADLE_USER_HOME/caches"
      # The daemon is a nix output, not a checked-in binary.
      install -D "${daemon}/lib/arm64-v8a/libclowder.so" \
        android/app/src/main/jniLibs/arm64-v8a/libclowder.so
      gradle --offline --no-daemon --console=plain -p android assembleDebug
      runHook postBuild
    '';
    installPhase = ''
      cp android/app/build/outputs/apk/debug/app-debug.apk $out
    '';
  };
}
