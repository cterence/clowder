# AGENTS.md

Guidance for AI agents (and humans) working in this repo. Keep this
file, the README, and the Pending Work list below up to date as work
lands.

## Project

Clowder (`clow` binary): an async, e2e-encrypted file-transfer mesh over
tailcat. Go module `clowder`, depends on upstream
`github.com/tailscale/tailcat` (currently a pseudo-version pinned to the
commit that added `Server.PeerKey`).

## Commands

    nix develop                    # devshell: go, gopls, golangci-lint, prek; installs git hooks
    nix develop .#android          # android app: SDK + JDK 17 + gradle (android/README.md)
    go build ./... && go test -race -count=1 ./...
    prek run --all-files           # gofmt, go vet, golangci-lint (hooks also run on commit)
    GOOS=windows go build ./daemon/   # cross-compile check

Conventions: commits go through the prek pre-commit hooks; tests are
table-driven and loopback-only (no DERP/network in CI); every transfer
path must be exercised by a daemon integration test.

## Layout

    main.go          CLI (thin IPC client; init/reset are local file ops)
    envelope/        chunked e2e sealing (age-style STREAM over the node keypair)
    persist/         durable local files: atomic writes + JSON ledger load/save
    roster/          cats, LWW merge, persistence; identity = node key
    protocol/        CBOR messages, 4-byte framing; sealed streams are raw bytes
    store/           storer spool: atomic writes, TTL, delete-on-ack
    daemon/          the runtime: serve/send/sync, pairing, IPC, stats,
                     TailcatTransport (production) + LocalTransport (tests)
    docs/specs/      the design doc (protocol, envelope, roster rules)

## Shared code — check before you write

Before writing any file-persistence, JSON-ledger or atomic-write
helper, check `persist/` (WriteFunc, WriteStream, LoadJSON, SaveJSON)
and extend it rather than re-rolling a per-package copy — identity,
me.json, stats, blocklist, outbox, roster and spool all go through it.
Likewise for protocol replies: use `protocol.Conn.Answer` and `.Ack`
instead of hand-assembling Messages. Shared helpers live in the
package that owns the concern (never a utils/ dump); if two packages
have grown the same private helper, hoist it there.

## Key invariants

- Trust is established ONLY via pairing (`clow invite`/`clow join`);
  tailcat addresses are never exchanged by hand. Pairing codes are
  8 words (80 bits) + the inviter's DERP region, valid 5 minutes, and
  strictly one-off: every `clow invite` is a fresh code (a new invite
  invalidates the previous one), and a code dies with its first
  successful join. The inviter commits a pairing only on the joiner's
  ack (a reply lost on the wire leaves the invite alive for the
  joiner's retry), and `clow join` returns only on the inviter's
  confirmation, so both rosters are durable before the command exits.
  The region is a fast-path hint, not a requirement: `clow join` tries
  it first and interleaves retries of it with sweeping the other
  regions, so a wrong hint or a slow relay attach costs seconds, not
  the pairing.
- Why 8 words: wireguard-go keys every handshake's MAC1 with
  blake2s("mac1----" || responderStaticPub), and pairing derives the
  inviter's pairing-server static public key from the words, so a
  recorded pairing handshake is an offline guess oracle
  (pinned by TestPairingMAC1OfflineVerifiable and
  BenchmarkPairingCandidateTest). Speakable codes force the routing
  key to stay word-derived, so the oracle cannot be removed within
  current tailcat — only out-sized by search space. Pairing v2 (a
  random pairing-server key riding IN the code, SPAKE2 over it) is the
  documented principled fix if a non-speakable code is ever
  acceptable.
- **Two keypairs per cat**: the identity (server) key inside the
  tailcat address, and a separate client key (`clientkey.json`) used
  for ALL outbound dials, which peers allowlist (roster `ClientKey`,
  carried in Hello and PairIntro, authenticated via
  `Server.PeerKey`). Never share one key between the server and client
  engines: two WireGuard engines with the same static key but different
  per-side PSKs cross-deliver handshakes and wedge unrecoverably.
- Both sides of a pairing derive ephemeral keypairs from the words;
  real addresses ride the encrypted pairing channel.
- Roster sync is add-only LWW; never delete entries outside the (pending)
  tombstone mechanism.
- Keep transferred files out of memory: stream everywhere; the sealed
  stream is opaque to storers by construction.
- Loopback tests cannot catch transport-authentication bugs; run
  `CLOWDER_INTEGRATION=1 go test ./daemon/ -run Integration -v -count=1`
  (real DERP) after touching the transport, pairing, or hello paths.

## Architecture: no global consensus

There is no leader, no quorum, and no globally linearizable state in
clowder — by decision, not omission. The mesh's consensus-shaped
problems are adversarial, not coordination problems: any paired cat
may misbehave, and quorum only decides between honest writers while
signatures decide against dishonest ones. Every problem gets the
smallest mechanism that suffices:

- A single decision between two cats: a bounded handshake. The
  pairing exchange's intro/ack/commit-confirm grew exactly this way,
  after a real split-brain.
- Shared mergeable facts (the roster): LWW under an add-only
  invariant, signed tombstones for removal (pending), and conflicts
  resolved by rule — newest Updated wins — never by vote.
- Ownership and identity: signatures on entries (pending), never a
  leader that could inject or override them.
- One-directional decisions (distrust): local state, deliberately
  not propagated.

Why not Raft or friends: consensus needs known membership and a
reachable quorum to make progress, while clowder's core promise is
delivery to cats that are OFFLINE — partitions are the norm, and a
two-cat clowder with both rarely online must still work. A leader is
a privileged injection point, the exact threat roster signing closes.
And end-to-end sealing means no decision needs global agreement: a
stale roster entry costs a failed dial and a retry, never corruption.

Reopen this stance only if a feature needs globally unique
namespaces, shared mutable clowder-wide state, coordinated resource
accounting, or exactly-one-node jobs — and even then, prefer
per-object signed ownership or richer CRDTs before a consensus
module; a signed lease beats leader election at this scale.

## Pending work

Ordered by complexity, easiest first; do not reorder without a
reason. Update this list and the README when something ships.

1. **Leave with signed forget-me gossip + roster entry signing** —
   derive an Ed25519 keypair from the node key seed
   (ed25519.NewKeyFromSeed(nodeRaw32)); announce the sign-public in
   Hello/roster entries; `clow leave` broadcasts a signed {leaver,
   timestamp} to all reachable peers; recipients drop the leaver
   (roster, allowlist, spool) and re-broadcast once. Roster sync must
   carry signed tombstones that always outrank later unsigned re-adds
   (offline peers catch up on next sync). The SAME sign key must
   authenticate roster entries: today any trusted cat can inject or
   override entries via LWW (brick an entry with a broken address, or
   duplicate a name to capture sends). Merge should require a valid
   signature on entries for keys already known.
   Upon resetting a cat, a leave should be sent to the clowder.
2. **Android app client** — architecture DECIDED, app scaffolded in
   android/. The daemon is the plain GOOS=android binary shipped as
   libclowder.so (nativeLibraryDir is executable), exec'd by
   ClowdService, a dataSync foreground service holding a partial wake
   lock and restarting the process with backoff — NOT gomobile
   bindings; the daemon package stays Android-dep-free, and the UI
   is plain Compose speaking the same unix-socket JSON IPC as the
   clow CLI (daemon/ipc.go shapes). The app runs `init` by exec'ing
   the binary with CLOWDER_DIR/HOME pointed into its sandbox; the
   inbox lands under filesDir, listed/shared via FileProvider.
   REMAINING: dogfood — the APK builds green from the flake as of
   2026-09-27 (`nix develop .#android`, build-native.sh, `gradle
   assembleDebug` in android/ — no Android Studio; the emulator shell
   `nix develop .#emulator` runs it on Apple Silicon, where the arm64
   system image can exec the arm64-only daemon), so what is left is
   actually running it: emulator or on-device dogfood (pair with a
   real cat, send/receive through the app); then boot-receive,
   delivery notifications, and the storer-role UI. Termux: running the CLI in
   Termux is explicitly NOT a goal (the foreground-service app is the
   answer); on-device builds are blocked until Termux ships
   go >= 1.27.1.
3. **Multiple clowders** — named clowders: per-clowder roster files,
   `--clowder` on invite/join/send, Hello carries the clowder name so a
   connection routes to the right roster. One identity, one daemon,
   clowders stay disjoint (flat sync would otherwise merge them).
   Largest refactor; do last, design tombstones against the final
   roster shape. Sync scaling lands here too: every peer is synced
   every poll tick, so a full-roster exchange is O(N²) bytes per
   cycle — trivial at homelab scale, but a large-roster cadence
   belongs here.
4. **Nix service packaging: systemd + launchd** — run the daemon as a
   real service from the flake. Linux: a NixOS module
   (`nixosModules.default`) wrapping `packages.default` in a systemd
   unit — `StateDirectory` for the config dir, Restart=on-failure, the
   health endpoint for a watchdog probe, CLOWDER_NAME/CLOWDER_STORER
   options, and auto-init-on-first-start so a fresh service
   bootstraps its identity. macOS: a launchd agent (plist or a
   nix-darwin module) with KeepAlive + RunAtLoad; agent not daemon,
   because the default inbox resolves under $HOME/Downloads — keep it
   a per-user agent, or make the inbox configurable first. The unit
   must survive suspend/resume without restarts (sleeping cats are a
   first-class case). Keep the flake free of OS-conditional deps:
   modules ship as passthrough attributes (`nixosModules`, and the
   plist as a template the README documents).

## Shipped

One line each; the pinning tests carry the details.

- **Resumable transfers**: an interrupted transfer resumes from the
  chunks already on disk instead of restarting — every send carries a
  fixed per-stream seal secret persisted in its outbox entry, so every
  attempt seals to the same stream bytes (Offer.Resumable; a receiver
  holding a partial answers with the sealed-stream offset of its last
  chunk checkpoint; SealStreamAt re-hashes the skipped prefix so the
  whole-file digest check still passes). Receivers checkpoint per
  64 KiB chunk into parts/ with a sidecar and move atomically into the
  inbox; all legs covered — direct, storer push, storer deposit
  (spool.PutResume parses only frame lengths, stream stays opaque).
  Receivers clamp their checkpoint through envelope.LastResumeBoundary
  — a checkpoint after the final short chunk is not a boundary
  SealedToPlain accepts, and answering with it verbatim wedges the
  transfer — and receiveResumable truncates the part to the boundary's
  plaintext before appending. Pinned by TestDirectSendResumesAfterCut,
  TestDirectSendResumesAcrossRestart, TestStorerPushResumesAfterCut,
  TestDepositResumeRoundTrip, TestDirectSendResumesFromFinalChunkCheckpoint.
- **Delivery receipts**: receivers seal {transferID, fileName,
  deliveredAt} to the ORIGINAL sender's node key and relay it like any
  small transfer; Offer.Receipt / store.Meta carry the flag across
  storer hops; the receipts.json ledger shows in `clow status`;
  payload capped at 4 KiB, digest-checked.
- **Transfer hardening**: receive-time free-space check (64 MiB
  reserve), a daemon-wide cap of 4 concurrent streams, idle
  tailcat-client eviction after 10 minutes, duplicate-transfer
  prevention (in-flight claims per outbox entry and per received
  offer ID), and size-lie enforcement (a sealed stream must match its
  announced size exactly).
- **Storers**: capacity (`clow storer on --max`, atomic reservations,
  refused sends retry), push sweep (deliver held files as soon as the
  target is online — the manual `clow fetch` pull was removed, the
  sweep replaced it), and dropbox mode (third-party storage only).
- **Liveness and sync**: all-peers roster sync on every poll tick and
  at start (pinned by TestSyncPeersDialsEveryPeer, TestStartupSyncBurst);
  symmetric liveness marking (TestDialerMarksTargetSeenOnHelloReply); a
  background path cache so `clow status` never probes
  (TestPathSnapshotNeverProbes); passive liveness ("seen Xm ago").
- **Local trust commands**: `clow distrust`/`trust` (blocklist in
  blocked.json, never propagated), `clow forget` (roster entry plus
  outbox), `clow rotate` (new PSK/address under the same identity),
  and duplicate-name hardening (LWW is per-key, so collisions persist;
  `clow status` tags them and resolution is social).
- **Daemon lifecycle**: a single-instance lock on the config dir — a
  second Run refuses rather than wedging the two-keypair invariant
  (TestSecondDaemonOnSameDirRefuses) — and drain at shutdown: cancel
  Run and wait, and the daemon no longer touches disk, network, or
  logs; in-flight work is uncancelable by design.
- **In-status observability**: in-flight transfer progress (per 64 KiB
  chunk, both directions) and transfer stats.
- **Wire-adjacent**: a Hello wire-version field (logged, never
  refused), fuzz targets for ReadMsg/parsePairCode/inboxPath, a
  dead-peer dial fix (tailcat's meow Ping is one-shot per client, so
  Dial probes with a real disco ping), and per-chunk buffer reuse in
  envelope.OpenStream (TestOpenStreamAllocationBound).
- **Packaging and ops**: a nix flake `packages.default` (buildGoModule
  pinned to go_1_27; devShell ships go 1.27.1), a Dockerfile with
  auto-init and CLOWDER_NAME/CLOWDER_STORER, an HTTP health endpoint
  (`clow daemon --health`), opt-in pprof on the health endpoint, and a
  self-hosted DERP map (CLOWDER_DERPMAP_URL / `clow daemon
  --derp-map`).
- **Test harness**: a black-box end-to-end integration test at the
  repo root (the test binary re-execs itself as the real clow binary;
  run with CLOWDER_INTEGRATION=1), and TestMain points TMPDIR at a
  short /tmp path (the default macOS TMPDIR overflows the unix socket
  path limit).

Known caveats: the golangci-lint-action version in CI (v7 + v2.13.2) is
unverified until a green run is observed; storer spools have TTL but no
size quota; rosters created before the two-keypair fix must be re-paired
(`clow forget` + invite/join). Interrupted transfers resume from the
chunks already on disk, but the resume rides a retry: nothing probes
for a checkpoint before the sender commits to an attempt, and a
pre-resume outbox entry (or a receipt) restarts from chunk zero.
