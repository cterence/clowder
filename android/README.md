# clowder for Android

A thin app around the daemon. The daemon is the same `clowder` binary
the other cats run, cross-compiled with `GOOS=android` (tailcat patches
Android DNS, CA certificates and interface discovery at init — see its
`android_linux.go`): the app ships it as `libclowder.so` and a
foreground service exec's it with a wake lock, which is the answer to
both Android's process lifecycle and the daemon+client UX. The UI is
plain Jetpack Compose and talks to the daemon over the same
unix-socket JSON IPC the `clow` CLI uses — the app is a second CLI,
not a daemon fork. The daemon package carries no Android code.

## Build

Everything comes from the flake — no Android Studio, no SDK
installer, no wrapper jar:

    nix develop .#android          # SDK + JDK 17 + gradle + go
    ./android/build-native.sh      # the daemon into jniLibs (arm64)
    cd android && gradle assembleDebug
    # -> app/build/outputs/apk/debug/app-debug.apk

The SDK matches app/build.gradle.kts (compileSdk 34, build-tools
34.0.0, AGP 8.5.2, JDK 17) — bump them together in flake.nix. The
shell sets the aapt2 override AGP needs under Nix; the shell's gradle
replaces the wrapper (none is checked in). Android Studio still works
as an optional editor — it picks up the same SDK from the shell's
ANDROID_SDK_ROOT — but nothing requires it.

## Emulator (Apple Silicon)

A second shell adds the emulator and an arm64 system image (~1.5 GB;
the plain build shell stays light without them):

    nix develop .#emulator
    # once:
    avdmanager create avd -n clowder \
      -k "system-images;android-34;default;arm64-v8a" --device pixel
    emulator -avd clowder
    adb install -r app/build/outputs/apk/debug/app-debug.apk

On Apple Silicon the emulator runs the arm64 image under
Hypervisor.framework and can exec the app's arm64-only daemon, so the
whole loop — pair with a real cat, send, receive — works without a
device. (On x86_64 hosts the image is x86_64 and cannot run the
arm64-only daemon; a device is the answer there.)

## How it maps to the CLI

| App screen | Mechanism |
|---|---|
| init | exec `libclowder.so init --name <name>` with `CLOWDER_DIR` and `HOME` pointing into the app sandbox |
| status / pair / send / outbox | IPC ops (`status`, `invite`, `join`, `send`, `cats`) over `clow.sock`, the same wire the CLI speaks |
| inbox | the daemon delivers into `$HOME/Downloads/clowder` inside the app's files dir; the app lists and shares it via FileProvider |
| daemon lifecycle | starts with the app (no manual start); `ClowdService`, a `dataSync` foreground service holding a partial wake lock, restarts the process with backoff if it dies; stop/start, view/clear the log and reset live in the Status screen's settings dialog |
| reset | the Status screen's settings dialog: announces the leave (`leave` op, so the clowder learns this cat is gone), stops the daemon, waits out its IPC socket, then exec's `libclowder.so reset --yes` and returns to init |

## Notes and limits

- The config dir is `<filesDir>/clowder`; the inbox is
  `<filesDir>/Downloads/clowder`. Nothing is shared with Termux or
  other installs; a cat initialized in the app is a new cat.
- Sending uses the system file picker: the picked document is copied
  into the app cache (the daemon needs a real path, not a content
  URI) and queued through the normal outbox.
- Battery: doze can still throttle network for background apps; the
  wake lock keeps transfers alive with the screen off, but this is a
  phone — expect the storer to matter.
- The inbox updates live: a FileObserver watches the daemon's
  inbox directory, so received files appear without a refresh. The
  log — opened from the settings dialog, not a tab — tails the
  daemon's ring buffer in selectable text, with an autoscroll
  checkbox to pin or free the tail.
- Not implemented yet: start on boot, per-cat notification on
  delivery, the storer role UI.
