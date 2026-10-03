# AGENTS.md

Guidance for AI agents (and humans) working in this repo. Keep this
file, the README, and the Pending Work list below up to date as work
lands.

## Project

Clowder (`clow` binary): an async, e2e-encrypted file-transfer mesh over
tailcat. Go module `clowder`, depends on upstream
`github.com/tailscale/tailcat` (pinned to the commit that added
`Server.PeerKey`).

## Skills

Always load the `go` and `ponytail` skills before writing or
reviewing code in this repo.

## Commands

    nix develop                    # devshell: go, gopls, golangci-lint, prek; installs git hooks
    nix develop .#android          # android app: SDK + JDK 17 + gradle (android/README.md)
    go build ./... && go test -race -count=1 ./... 2>&1 | grep -E '^(ok|FAIL|---)'
                                                 # the grep is deliberate: tests
                                                 # print init noise to stdout, and a
                                                 # filter that lets noise through can
                                                 # push the FAIL line out of view
    prek run --all-files           # gofmt, go vet, golangci-lint (hooks also run on commit)
    GOOS=windows go build ./daemon/   # cross-compile check

Conventions: commits go through the prek pre-commit hooks; tests are
table-driven and loopback-only (no DERP/network in CI); every transfer
path must be exercised by a daemon integration test. Pushes to origin
main are pre-authorized: commit and push without asking (force-push
still needs explicit approval).

## Layout

    main.go          CLI (thin IPC client; init/reset are local file ops)
    envelope/        chunked e2e sealing (age-style STREAM over the node keypair)
    persist/         durable local files: atomic writes + JSON ledger load/save
    roster/          cats, LWW merge, persistence; identity = node key
    protocol/        CBOR messages, 4-byte framing; sealed streams are raw bytes
    store/           storer spool: atomic writes, TTL, delete-on-ack
    daemon/          the runtime: serve/send/sync, pairing, IPC, stats,
                     TailcatTransport (production) + LocalTransport (tests)
    nix/             service modules: shared options, NixOS, nix-darwin,
                     Home Manager
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
  address, and a separate dial key (`clientkey.json`) for ALL outbound
  dials, which peers allowlist (roster `DialKey`, authenticated via
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
(roster), signatures on entries (identity), the local blocklist
(never propagated). Core promise is delivery to cats that are OFFLINE —
partitions are the norm, so consensus would block progress. Reopen this
stance only for globally unique namespaces, shared mutable clowder-wide
state, coordinated resource accounting, or exactly-one-node jobs — and
then prefer per-object signed ownership or CRDTs first; a signed lease
beats leader election at this scale.

## Pending work

Track todos as GitHub issues, not in this file: file an issue for
new work and reference it here only when the context an agent needs is
not already captured there.

Tracked in GitHub issues:

- Security and DoS findings from the 2026-09 security review — the
  wire-auth bundle landed (signed offers #7, signed receipts #8, the
  HKDF sign-key derivation and the provisional-pin binding #12, the
  storer per-sender quota #15). All cats must upgrade — sign pins
  rotate automatically on first contact (no re-pair; a mixed
  old/new mesh is broken until the stragglers upgrade). #11 closed
  as documented accepted risks (rotation runbook in the README);
  #16 landed (roster-hash sync skips unchanged payloads).

## Shipped

One line each; the pinning tests carry the details.

- **Wire-auth bundle** (#7, #8, #12, #15; upgrade-in-place, no
  re-pair — all cats upgrade, sign pins rotate on first contact):
  offers are signed by the sender's sign key and verified by the
  final recipient — relayed through storers too, the sig rides the
  spool (TestOfferSignatureRequired); storers refuse unsigned
  deposits and give each sender at most a quarter of the spool,
  derived from the spool scan so a restart cannot reset it
  (TestStorerPerSenderQuota); the sign key is HKDF(node seed,
  clowder/sign/v1), not the raw seed reused across WireGuard and
  Ed25519 (TestSignKeyDomainSeparated); a sync-carried sign-key pin is
  provisional — the owner's authenticated Hello re-pins and takes
  its live entry (TestSyncPinIsProvisional); and a storer-held send
  leaves the outbox only on the recipient's signed receipt pushed on
  its sync round — the hold probe no longer deletes, it only
  re-queues (TestSignedReceiptClearsOutbox). `clow send --storer`
  skips the direct attempt and its dial timeout entirely
  (TestSendViaStorerSkipsDirect).

- **DoS and availability hardening** (#10, parts of #7 and #8): one
  authenticated peer may hold at most 8 concurrent serves
  (TestPeerConnCapDropsExcess) and at most 10 post-handshake roster
  pushes per connection (TestRosterMergeCapCutsConn); offers carry the
  sender's node key, so a forgotten cat's relayed files are refused by
  key, not by the name the roster no longer resolves
  (TestRelayedOfferBlockedByKey); an AckStored no longer deletes the
  outbox entry — a poll-tick hold probe confirms the storer still has
  the file and re-queues the send when it does not
  (TestAcceptAndDropStorerCaughtByProbe). A lying storer still defeats
  the probe; the propagated receipt (#8) and signed offers (#7) are the
  full fixes.

- **Security hardening pass** (#6, #9, #13, #14): a sync is not a
  trust root — unsigned entries for unknown keys no longer propagate
  and a tailcat address's Key must derive from the Addr (TestSyncRejects
  UnknownUnsignedEntry); an authenticated dial key can only mark its
  own roster identity seen and the liveness map is bounded
  (TestServeConnMarksOnlyKnownIdentities, TestMarkSeenBounded); the
  pairing code is never logged whole (TestInviteCodeStaysOutOfLogs);
  IPC is unix-socket-only, the path-sniffed TCP fallback is gone
  (TestIPCPathWithColonStaysUnix).
- **`clow version`** (#17): the flake wires the build revision through
  ldflags, so a stale daemon is distinguishable from a fresh one.
- **Pairing and roster UX**: join is daemon-side state — the CLI
  returns at once and polls, Ctrl-C leaves the attempt running, and a
  retry with the same code reports the recorded outcome instead of
  re-dialing a dead invite (TestJoinReportsDaemonState); the inviter
  pushes its full roster and liveness with the join confirmation, so a
  fresh joiner is alive before the first sync tick (TestJoinRosterPush,
  RosterSync.Liveness wire field); both pairing sides fan out a sync
  round the moment the joiner commits, so online cats have the full
  roster without waiting a tick (TestPairingFansOutRosterSync); leave
  is synchronous and fast: the goodbye is announced in parallel, any
  cat that does not answer within 10s (a cold tailcat dial to an
  online cat pays netcheck + relay attach, measured ~3s on the real
  mesh) is cut, and the command fails loudly but still wipes locally
  — a cat nobody trusts back must not become unleaveable; one reached
  cat suffices (recipients re-broadcast, the tombstone rides sync to
  sleepers); join is a synchronous blocking RPC (a Ctrl-C'd attempt
  keeps running in the daemon, so a retry with the now-dead code
  reports a misleading timeout — re-pair with a fresh invite if that
  happens), the joiner acks the roster push and the inviter
  closes the pairing channel on that ack so the confirmation cannot
  be black-holed by a close with bytes in flight (the old 45s "slow
  inviter" join, TestPairingHoldsChannelForJoinerAck), and the joiner
  signs and stamps its intro so a re-pair outranks the stale
  tombstone from an earlier leave instead of being silently un-paired
  (TestRepairSurvivesStaleTombstone); `clow forget` accepts the short
  key prefix `clow status` displays
  (TestForgetKeyPrefixMatchesStatusDisplay).
- **Signed leave + roster entry signing**: Ed25519 keys derived from the
  node key seed sign every roster entry (merge requires the pinned sign
  key's signature, so no cat can inject or override entries via LWW) and
  the leave tombstone; tombstones ride sync and outrank unsigned
  re-adds — only a re-pair resurrects.
- **Transfer hardening**: receive-time free-space check (64 MiB
  reserve; on Windows via GetDiskFreeSpaceEx), 4 concurrent streams max, idle tailcat-client eviction that
  spares clients with an open dial, in-flight claims against duplicate
  transfers, a strict 32-hex transfer-ID guard (spool
  paths are built from IDs), size-lie enforcement; delivery retries
  skip the source re-hash while the file is unchanged (digest and
  stat cached in the outbox entry).
- **Storers**: capacity with atomic reservations, push sweep (replaced
  the manual `clow fetch` pull), dropbox mode (third-party storage
  only).
- **Liveness and sync**: all-peers roster sync every poll tick and at
  start; symmetric liveness marking. The sync IS the keepalive —
  nothing else probes periodically; `clow ping <CAT-OR-KEY>` is the
  on-demand check (the same authenticated handshake, marks liveness
  both ways, bounded 15s, and reports direct vs the DERP relay). The
  Hello carries a roster digest (#16): matched hashes skip the roster
  payload — an idle round is two Hellos, not O(N) bytes (a pre-#16
  peer always gets the full roster; TestSyncRosterHashSkipsUnchanged
  and roster.TestSyncHash pin it).
- **Local trust commands**: `clow forget <CAT-OR-KEY>` (roster +
  outbox; refused by roster merge, so it sticks against re-adds;
  re-pairing clears the blocklist — the way back), and duplicate-name
  hardening (status names each claimant's short key and age):
  sends to a name claimed by two keys are refused, and both pairing
  sides refuse a name another key already claims — collisions can still
  arrive via parallel invites through different inviters, `clow status`
  tags them, and one cat re-inits with a fresh name to resolve.
- **Daemon lifecycle**: single-instance lock on the config dir; drain
  at shutdown — after Run returns the daemon touches no disk, network,
  or logs; in-flight work is uncancelable by design.
- **In-status observability**: in-flight transfer progress (both
  directions) and lifetime stats.
- **Multiple files and directories (#31)**: `clow send <CAT> <FILE-OR-DIR>...`
  queues one outbox entry per file; a directory walks to one entry per
  regular file with its slash-separated path relative to the sent root
  riding `Offer.FileName` (the root's name included, scp -r style).
  Symlinks and special files are refused, never followed. The receiver
  sanitizes each component itself — control chars stripped, empty/./..
  dropped, backslashes read as separators, Windows-reserved names munged
  — and mkdirs the parents; a pre-#31 daemon flattens the name to the
  basename, so a mixed mesh degrades instead of breaking. Pins:
  TestInboxPathNested, FuzzInboxPath (containment),
  TestSendNameValidation, TestSendNamedDeliversNested,
  TestSendNamedViaStorer, TestNestedOfferNameSanitized,
  TestExpandSendPaths, TestSendDirectoryQueuesEachFile.
- **Send lifecycle gaps**: undelivered sends expire after 7 days
  (outboxTTL mirrors the storer spool's DefaultTTL — a cat that never
  comes online is not retried forever; TestOutboxExpires); every attempt
  cycle records its newest refusal on the outbox entry and `clow status`
  shows it ("last tried 30s ago: direct: ..."), instead of only daemon
  logs (TestOutboxRecordsLastRefusal); `clow send --all` broadcasts to
  every roster cat, targeted by key so duplicate names cannot misroute
  (TestSendAllQueuesEachCat), and send resolves its target by name or
  full key — a duplicate-named cat is targetable by key (TestSendByKey).
  --all refuses --clipboard: one staged copy cannot outlive its first
  outbox entry.
- **Sync send and cancel**: `clow send` follows the transfer until it
  is delivered or a storer holds it; `--async` queues and returns
  immediately. Ctrl-C (or `clow cancel [<ID>]`, all sends when no
  ID) drops the outbox entry and aborts an in-flight attempt
  through its delivery claim.
- **Clipboard send**: `clow send --clipboard <CAT>` and the Android
  app's send-clipboard button stage the clipboard as a file
  (`<configdir>/staging` on the CLI, cacheDir on Android — outbox
  retries re-read the source, so the copy must outlive the send).
  The daemon owns the copy: every outbox-entry death (direct
  delivery, the recipient's signed receipt, cancel, forget, leave)
  sweeps its staged source, so a storer-held send keeps it until the
  receipt arrives and `--async` cannot orphan it; only a send that
  never queued (a lost IPC reply) can leave one behind
  (TestStagedSourceSurvivesStorerHold, TestStagedSourceDiesWithOutbox).
- **Wire-adjacent**: Hello wire-version field (logged, never refused),
  fuzz targets (ReadMsg, parsePairCode, inboxPath), disco-ping dial
  probe (meow Ping is one-shot per client), allocation-bounded
  OpenStream.
- **Packaging and ops**: nix flake `packages.default`,
  `packages.clowder-android` (the APK: the daemon is a nix
  GOOS=android cross-build, the gradle caches one hash-pinned fetch
  verified against android/gradle/verification-metadata.xml, the APK
  an offline replay — nix/android.nix), Dockerfile with
  auto-init and CLOWDER_NAME/CLOWDER_STORER, HTTP health endpoint,
  opt-in pprof, self-hosted DERP map; CI nix job builds the flake
  package for x86_64-linux and aarch64-darwin and pushes both to the
  niks3 binary cache (GitHub OIDC, no secrets — the server's subject
  allowlist lives in the homelab niks3 chart).
- **Multiple clowders (#19)**: a clowder IS a config dir —
  `base/<name>/` under $CLOWDER_DIR (now the base), default clowder at
  `base/default/` with no-flag commands meaning it; a flat pre-nesting
  base auto-migrates on first run. Names are local only
  (--clowder flag > $CLOWDER env > default, slug-validated), never on
  the wire; per-clowder default inbox `~/Downloads/clowder/<name>/`.
  Zero wire change: one daemon per clowder, disjoint identities,
  cross-clowder sameness a non-goal. The tailcat listener is virtual,
  so clowders sharing a host coexist on the same overlay port
  (2569 is a mesh-wide convention only; --health differs per instance
  when enabled). nix modules expose
  `services.clowder.instances.<name>` (one unit/agent per name); the
  Android app's header is the local switcher (one daemon at a time,
  restarted on the chosen dir). Pins: TestClowderNesting,
  TestFlatBaseMigration, TestInboxDirPerClowder,
  TestIntegrationTwoClowders.
- **Service packaging**: NixOS module (`nixosModules.default`,
  `services.clowder.instances.<name>`: dedicated clowder user,
  StateDirectory /var/lib/clowder/<name>, HOME pointed at the base
  (system users get /var/empty, which would break the default inbox) —
  auto-init rides the daemon's own first-start init,
  Restart=on-failure; deliberately
  NO WatchdogSec, since the watchdog clock counts suspend time and
  would kill a healthy daemon on wake — hang detection stays with
  external probes of healthAddr) and, sharing one options.nix, a
  per-user nix-darwin module (launchd user agent, KeepAlive +
  RunAtLoad) and a Home Manager module (launchd agent on macOS,
  systemd user unit on Linux); all survive suspend/resume without
  restarts. Flake module wrappers must take pattern args
  ({ pkgs, ... }@args): module args are injected only into
  pattern-named parameters.
- **Test harness**: a black-box end-to-end integration test at the repo
  root (the test binary re-execs itself as the real clow binary; run
  with CLOWDER_INTEGRATION=1), and TestMain pointing TMPDIR at a short
  /tmp path (macOS TMPDIR overflows the unix socket path limit). The
  nix CI job runs a consumer-render gate (nix/render-test.nix): the
  NixOS module imported into a real NixOS eval, the rendered unit
  forced — drvPath-level checks cannot see a module that renders
  nothing.

## Known caveats

- The golangci-lint-action version in CI is unverified until a green
  run is observed.
- Storer spools have TTL but no size quota.
- Rosters created before the two-keypair fix must be re-paired
  (`clow forget` + invite/join). The wire-auth bundle needs no
  re-pair: identity and dial keys are unchanged, New() re-signs the
  self entry with the new sign key at startup, and the provisional
  pin heals from the peer's authenticated Hello on first contact.
- The simplification pass removed resume, receipts, rotate,
  distrust/trust and /stats; all were wire-visible, so a pre-cut mesh
  re-pairs (`clow forget` + invite/join) to talk to a cut daemon.
- Signing upgrade seam: an entry with no pinned sign key (pre-signing)
  accepts the first sign key it sees — from a sync or an authenticated
  Hello — so a cat upgrading mid-attack can be pinned wrong once; and a
  tombstone for a key a cat never knew is unverifiable and dropped, so
  a brand-new member can be fed a stale pre-leave entry by a stale peer.
- Unsigned sync entries for unknown keys are refused, and a SIGNED
  entry still propagates for unknown keys — but its sign-key pin is
  provisional: the key's owner replaces it from its authenticated
  Hello, taking its live roster entry verbatim. The window is only a
  victim that never contacts you (offline forever, or the forged
  entry also mangled the dial key so its dials fail); re-pairing is
  the way back.
