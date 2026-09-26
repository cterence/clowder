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

1. Build the daemon binary into the app's jniLibs:

       ./android/build-native.sh

2. Open `android/` in Android Studio (it resolves the Gradle wrapper
   and SDKs on first sync) and run the app on a device or emulator.

There is no wrapper jar checked in; Android Studio generates the
wrapper on first sync, or run `gradle wrapper` if you have a local
Gradle.

## How it maps to the CLI

| App screen | Mechanism |
|---|---|
| init | exec `libclowder.so init <name>` with `CLOWDER_DIR` and `HOME` pointing into the app sandbox |
| status / pair / send / outbox | IPC ops (`status`, `invite`, `join`, `send`, `cats`) over `clow.sock`, the same wire the CLI speaks |
| inbox | the daemon delivers into `$HOME/Downloads/clowder` inside the app's files dir; the app lists and shares it via FileProvider |
| daemon lifecycle | `ClowdService`, a `dataSync` foreground service holding a partial wake lock, restarting the process with backoff if it dies |

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
- Not implemented yet: start on boot, per-cat notification on
  delivery, the storer role UI.
