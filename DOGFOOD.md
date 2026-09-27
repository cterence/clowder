# Dogfood log

Findings from running the mesh for real. One line each, newest first;
promote to pending work in AGENTS.md when a fix is agreed.

- 2026-09-28: android inbox tab showed "nothing received yet" despite
  files arriving — the MediaStore query matched `Download/clowder`
  without the trailing slash MediaProvider stores; fixed in the same
  session (reinstall the APK to pick it up).
- 2026-09-27: homelab3 join straddled DERP congestion: the CLI was
  Ctrl-C'd while the daemon completed the handshake, so the retry
  failed on the consumed code with a misleading timeout. -> pending
  item 3(b).
- 2026-09-27: the storer's 1 GiB push to stronghold stalled at 68% and
  did not visibly resume within ~20 minutes. Unresolved: watch for a
  repeat and capture daemon logs from both ends if it does.
- 2026-09-27: forget is local-only by design, so a forgotten cat rides
  back in from peers that never forgot it (zenfone). The clean removal
  is the signed leave from the cat itself. -> pending item 3(c).
- 2026-09-27: right after join, every cat reads "never seen" until
  syncs land. -> pending item 3(a).
