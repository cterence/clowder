# AGENTS.md

Guidance for AI agents (and humans) working in this repo. Keep this
file, the README, and the Pending Work list below up to date as work
lands.

## Project

Clowder (`clow` binary): an async, e2e-encrypted file-transfer mesh over
tailcat. Go module `clowder`, depends on upstream
`github.com/tailscale/tailcat` (pinned to the commit that added
`Server.PeerKey`).

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

Before writing any file-persistence, JSON-ledger or atomic-write helper,
extend `persist/` (WriteFunc, WriteStream, LoadJSON, SaveJSON) instead
of re-rolling a per-package copy; use `protocol.Conn.Answer`/`.Ack`
instead of hand-assembling Messages. Shared helpers live in the package
that owns the concern (never a utils/ dump); hoist when two packages
grow the same private helper.

## Key invariants

- Trust is established ONLY via pairing (`clow invite`/`clow join`);
  tailcat addresses are never exchanged by hand. Pairing codes are
  8 words (80 bits) + the inviter's DERP region, valid 5 minutes, and
  strictly one-off: a new invite invalidates the previous one, a code
  dies with its first successful join. The inviter commits only on the
  joiner's ack and `clow join` returns only on the inviter's
  confirmation, so both rosters are durable before the command exits.
  The region is a fast-path hint; `clow join` sweeps the others.
- Why 8 words: pairing derives the inviter's pairing-server static key
  from the words, and WireGuard's MAC1 makes a recorded handshake an
  offline guess oracle — speakable codes force the routing key to stay
  word-derived, so the oracle can only be out-sized by search space
  (TestPairingMAC1OfflineVerifiable). Pairing v2 (random pairing-server
  key riding IN the code, SPAKE2) is the documented fix if a
  non-speakable code is ever acceptable.
- **Two keypairs per cat**: the identity (server) key in the tailcat
  address, and a separate client key (`clientkey.json`) for ALL outbound
  dials, which peers allowlist (roster `ClientKey`, authenticated via
  `Server.PeerKey`). Never share one key between the server and client
  engines: same static key + different per-side PSKs cross-deliver
  handshakes and wedge unrecoverably.
- Both pairing sides derive ephemeral keys from the words; real
  addresses ride the encrypted pairing channel.
- Roster sync is add-only LWW; entries are removed only by signed leave
  tombstones, which always outrank unsigned re-adds.
- Keep transferred files out of memory: stream everywhere; the sealed
  stream is opaque to storers by construction.
- Loopback tests cannot catch transport-authentication bugs; run
  `CLOWDER_INTEGRATION=1 go test ./daemon/ -run Integration -v -count=1`
  (real DERP) after touching the transport, pairing, or hello paths.

## Architecture: no global consensus

No leader, no quorum, no globally linearizable state — by decision. The
mesh's consensus-shaped problems are adversarial, not coordination
problems: any paired cat may misbehave, and quorum only decides between
honest writers while signatures decide against dishonest ones. Each
problem gets the smallest mechanism that suffices: bounded handshakes
(pairing's intro/ack/commit-confirm), LWW plus signed tombstones
(roster), signatures on entries (identity), local distrust (never
propagated). Core promise is delivery to cats that are OFFLINE —
partitions are the norm, so consensus would block progress. Reopen this
stance only for globally unique namespaces, shared mutable clowder-wide
state, coordinated resource accounting, or exactly-one-node jobs — and
then prefer per-object signed ownership or CRDTs first; a signed lease
beats leader election at this scale.

## Pending work

Ordered by complexity, easiest first; do not reorder without a reason.
Update this list and the README when something ships.

1. **Android app client** — architecture DECIDED, app scaffolded in
   android/. The daemon is the plain GOOS=android binary shipped as
   libclowder.so (nativeLibraryDir is executable), exec'd by ClowdService,
   a dataSync foreground service with a wake lock and restart-with-backoff
   — NOT gomobile bindings; the daemon package stays Android-dep-free;
   the UI is plain Compose speaking the same unix-socket JSON IPC as the
   CLI (daemon/ipc.go shapes). REMAINING: dogfood — the APK builds green
   from the flake (`nix develop .#android`, build-native.sh, `gradle
   assembleDebug`; the emulator shell `nix develop .#emulator` runs it on
   Apple Silicon) — then pair with a real cat, send/receive through the
   app; then boot-receive, delivery notifications, and the storer-role
   UI. Termux is explicitly NOT a goal (the foreground-service app is
   the answer); on-device builds blocked until Termux ships go >= 1.27.1.
2. **Multiple clowders** — named clowders: per-clowder roster files,
   `--clowder` on invite/join/send, Hello carries the clowder name so a
   connection routes to the right roster. One identity, one daemon,
   clowders stay disjoint. Largest refactor; do last, design tombstones
   against the final roster shape. Sync scaling lands here too: every
   peer is synced every poll tick, so a full-roster exchange is O(N²)
   bytes per cycle — fine at homelab scale.
3. **Nix service packaging: systemd + launchd** — run the daemon as a
   real service from the flake. Linux: a NixOS module wrapping
   `packages.default` in a systemd unit — StateDirectory,
   Restart=on-failure, health-endpoint watchdog probe,
   CLOWDER_NAME/CLOWDER_STORER options, auto-init-on-first-start. macOS:
   a launchd agent (per-user, not a daemon — the default inbox resolves
   under $HOME/Downloads) with KeepAlive + RunAtLoad; keep the flake free
   of OS-conditional deps (modules ship as passthrough attributes).
   Must survive suspend/resume without restarts.

## Shipped

One line each; the pinning tests carry the details.

- **Signed leave + roster entry signing**: Ed25519 keys derived from the
  node key seed sign every roster entry (merge requires the pinned sign
  key's signature, so no cat can inject or override entries via LWW) and
  the leave tombstone; tombstones ride sync and outrank unsigned
  re-adds — only a re-pair resurrects.
- **Resumable transfers**: a fixed per-stream seal secret in the outbox
  entry makes retried attempts byte-identical; receivers checkpoint per
  64 KiB chunk and clamp through envelope.LastResumeBoundary; all legs
  covered (direct, storer push, storer deposit).
- **Delivery receipts**: sealed to the ORIGINAL sender's node key,
  signed by the receiver and verified against the pinned roster sign key
  (an unpinned pre-signing entry accepts — same seam as entries); relays
  only see the flag and size; 4 KiB cap, digest-checked; ledger in
  `clow status`.
- **Transfer hardening**: receive-time free-space check (64 MiB
  reserve), 4 concurrent streams max, idle tailcat-client eviction that
  spares clients with an open dial, in-flight claims against duplicate
  transfers, a strict 32-hex transfer-ID guard (spool and parts paths
  are built from IDs), size-lie enforcement.
- **Storers**: capacity with atomic reservations, push sweep (replaced
  the manual `clow fetch` pull), dropbox mode (third-party storage
  only).
- **Liveness and sync**: all-peers roster sync every poll tick and at
  start; symmetric liveness marking; background path cache so
  `clow status` never probes.
- **Local trust commands**: `clow distrust`/`trust` (never propagated),
  `clow forget` (roster + outbox; refused by roster merge, so it sticks
  against re-adds — re-pairing is the way back), `clow rotate` (new
  PSK/address under the same identity), duplicate-name tagging (LWW is
  per-key, so collisions persist; resolution is social).
- **Daemon lifecycle**: single-instance lock on the config dir; drain
  at shutdown — after Run returns the daemon touches no disk, network,
  or logs; in-flight work is uncancelable by design.
- **In-status observability**: in-flight transfer progress (both
  directions) and lifetime stats.
- **Wire-adjacent**: Hello wire-version field (logged, never refused),
  fuzz targets (ReadMsg, parsePairCode, inboxPath), disco-ping dial
  probe (meow Ping is one-shot per client), allocation-bounded
  OpenStream.
- **Packaging and ops**: nix flake `packages.default`, Dockerfile with
  auto-init and CLOWDER_NAME/CLOWDER_STORER, HTTP health endpoint,
  opt-in pprof, self-hosted DERP map.
- **Test harness**: a black-box end-to-end integration test at the repo
  root (the test binary re-execs itself as the real clow binary; run
  with CLOWDER_INTEGRATION=1), and TestMain pointing TMPDIR at a short
  /tmp path (macOS TMPDIR overflows the unix socket path limit).

## Known caveats

- The golangci-lint-action version in CI is unverified until a green
  run is observed.
- Storer spools have TTL but no size quota.
- Rosters created before the two-keypair fix must be re-paired
  (`clow forget` + invite/join).
- Resume rides a retry: nothing probes for a checkpoint before the
  sender commits to an attempt, and a pre-resume outbox entry (or a
  receipt) restarts from chunk zero.
- Signing upgrade seam: an entry with no pinned sign key (pre-signing)
  accepts the first sign key it sees — from a sync or an authenticated
  Hello — so a cat upgrading mid-attack can be pinned wrong once; and a
  tombstone for a key a cat never knew is unverifiable and dropped, so
  a brand-new member can be fed a stale pre-leave entry by a stale peer.
