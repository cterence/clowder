#!/usr/bin/env bash
# Builds the clowder daemon binary for Android into the app's jniLibs.
# The binary ships as libclowder.so so the package manager extracts it
# to nativeLibraryDir (executable); the foreground service exec's it.
# arm64-v8a for devices; the emulator runs the arm64 binary under
# translation on modern hosts, so there is one build (amd64 needs cgo).
set -euo pipefail
cd "$(dirname "$0")/.."

out=android/app/src/main/jniLibs
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$out/arm64-v8a/libclowder.so" .
echo "built:"
ls -l "$out/arm64-v8a/"
