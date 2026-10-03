# clowder for Android

A thin app around the daemon. The daemon is the same `clowder` binary
the other cats run, cross-compiled with `GOOS=android` (tailcat patches
Android DNS, CA certificates and interface discovery at init — see its
`android_linux.go`): the app ships it as `libclowder.so` and exec's it
while the app is on screen — no foreground service, no wake lock, no
background battery use. The UI is
plain Jetpack Compose and talks to the daemon over the same
unix-socket JSON IPC the `clow` CLI uses — the app is a second CLI,
not a daemon fork. The daemon package carries no Android code.

## Build

The canonical build is one command, fully hermetic: the daemon is a
nix cross-build (GOOS=android), the gradle/maven caches are one
hash-pinned fetch verified against
`android/gradle/verification-metadata.xml` (the go.sum equivalent —
every artifact's sha256 is checked in), and the APK builds offline
from those caches. nix/android.nix holds the derivations.

    nix build -o result.apk .#clowder-android
    # result.apk IS the APK (single-file output), signed with
    # keystore-debug.keystore; adb insists on the extension, hence -o

Iterating in the devshell (SDK + JDK 17 + gradle, no Android Studio,
no wrapper jar):

    nix develop .#android
    ./android/build-native.sh      # the daemon into jniLibs, from the nix build
    cd android && gradle assembleDebug

Updating pinned dependencies (both change together):
- regenerate `gradle/verification-metadata.xml` with a fresh cache:
  `GRADLE_USER_HOME=$(mktemp -d) gradle --write-verification-metadata sha256 assembleDebug`
- update the `gradleDeps` `outputHash` in nix/android.nix — a build
  with a stale hash prints the got-hash. The derivation name must
  stay constant (`clowder-gradle-deps`): nix keys a fixed-output
  store path by name+hash, so a version-stamped name forces a
  refetch on every commit. The cache keeps `modules-2` only —
  gradle rebuilds transforms and script caches offline — so the
  hash stays independent of the app sources.

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

Home (the cat list) is the app; everything else is a pushed screen —
the system back gesture closes it.

| App screen | Mechanism |
|---|---|
| init | exec `libclowder.so init --name <name>` with `CLOWDER_DIR` and `HOME` pointing into the app sandbox |
| home | the cat roster (liveness, roles) with in-flight transfers and the outbox, polled over `status` op |
| cat detail | tap a cat: send (system file picker), ping (online check, marks liveness), its live transfers, queued sends (cancellable) and send receipts (delivered / held-by-storer, session state cleared on app close), and a confirmed `forget` — the CLI's local-only forget |
| pair | top-bar `+`: IPC ops (`invite`, `join`) over `clow.sock`, the same wire the CLI speaks |
| inbox | top-bar mail icon: a receipt log, not a directory view — every delivered file is logged when the publisher moves it into the system's `Download/clowder/<name>` for the active clowder (`MediaStore.Downloads`, API 29+; a pre-nesting flat publish migrates into `default/` once), entries survive deleting the file from Downloads, "clear" empties the log only, and "open in files" launches the phone's file manager at its root (the files are in Download/clowder/<name>) |
| settings | top-bar gear: stop/start the daemon, view/clear the log, leave the clowder, reset (announces the leave — `leave` op, so the clowder learns this cat is gone — then stops the daemon, waits out its IPC socket, then exec's `libclowder.so reset --yes` and returns to init) |

The daemon runs only while the app is on screen: MainActivity starts
`ClowdService` in `onStart` and stops it in `onStop`; the service
restarts the process with backoff if it dies while the app is open.
Files are received — and transfers finish — only while the app is
open; the mesh's storer role is the answer for offline delivery.

## Notes and limits

- The config base is `<filesDir>/clowder`; each clowder is a directory
  under it (`<filesDir>/clowder/<name>`, default `default`), and the
  per-clowder inbox is `<filesDir>/Downloads/clowder/<name>`, and
  the published Downloads dir is the system's
  `Download/clowder/<name>` (the receipt log is per-clowder too).
  The home
  header is the clowder switcher: picking (or creating) a clowder
  restarts the daemon on its config dir — one clowder at a time, and
  an uninitialized one lands on the init screen. Nothing is shared
  with Termux or other installs; a cat initialized in the app is a new
  cat.
- Sending picks files first, sends after: the system picker allows
  multiple documents, the selection is removable, and one send button
  queues them all. Each picked document is copied
  into the app cache (the daemon needs a real path, not a content
  URI) and queued through the normal outbox.
- Battery: the daemon runs only while the app is on screen — it uses
  nothing in the background. The flip side: nothing is received while
  the app is closed; expect the storer to matter on a phone. One
  exception: a transfer still in flight when the app leaves the screen
  runs to completion — the service promotes itself to a dataSync
  foreground service (a notification with live progress) and stops
  the daemon the moment it goes idle.
- The inbox updates live: a FileObserver watches the daemon's
  inbox directory, so receipts appear without a refresh, and each
  delivery raises a toast. The daemon log — opened from the Settings
  screen, not a tab — tails the daemon's ring buffer in selectable
  text, with an autoscroll checkbox to pin or free the tail.
- Not implemented yet: start on boot, per-cat notification on
  delivery, the storer role UI.
