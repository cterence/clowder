#!/usr/bin/env bash
# Puts the daemon into the app's jniLibs for the devshell gradle build
# (nix develop .#android). The canonical APK build is
# `nix build .#clowder-android`, which wires the daemon in itself.
set -euo pipefail
cd "$(dirname "$0")/.."

out=$(nix build --no-link --print-out-paths .#clowder-android-daemon)
install -D "$out/lib/arm64-v8a/libclowder.so" \
  android/app/src/main/jniLibs/arm64-v8a/libclowder.so
echo "installed: android/app/src/main/jniLibs/arm64-v8a/libclowder.so"
