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
    daemon/          the runtime: serve/send/fetch/sync, pairing, IPC, stats,
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
  5 words (50 bits) + the inviter's DERP region, valid 5 minutes, and
  strictly one-off: every `clow invite` is a fresh code (a new invite
  invalidates the previous one), and a code dies with its first
  successful join. The inviter commits a pairing only on the joiner's
  ack (a reply lost on the wire leaves the invite alive for the
  joiner's retry), and `clow join` returns only on the inviter's
  confirmation, so both rosters are durable before the command exits.
  The
  region is a fast-path hint, not a requirement: `clow join` tries it
  first and interleaves retries of it with sweeping the other regions
  (relay presence can flap away from the encoded one on networks with
  two nearby relays, and a pairing server can attach late on slow
  machines), so a wrong hint or a slow attach costs seconds, not the
  pairing.
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

1. **Pairing hardening: offline guessability + PAKE** — Phase 1
   (verify the offline-guess reasoning) is CLOSED as of 2026-09-26:
   CONFIRMED. The old file comment claimed guessing was
   "active-only, nothing verifiable offline" — false. wireguard-go
   keys every handshake's MAC1 with blake2s("mac1----" ||
   responderStaticPub), and derivePairing derives the inviter's
   pairing-server static public key from the five words, so a
   recorded pairing handshake (the DERP relay operator, or whoever
   compromises a relay, can record one) is an offline oracle for
   candidate word codes. Pinned in-repo by
   TestPairingMAC1OfflineVerifiable (candidate tester reproduces the
   captured MAC from the words alone) and
   BenchmarkPairingCandidateTest (~66µs per candidate through the
   full derive; an attacker's inviter-only pipeline keeps the X25519
   at tens of µs). Exploit window math: the words are one-off and
   expire in ~5 minutes, so a from-scratch 50-bit search cannot
   finish inside the window — but the pub key table is PRECOMPUTABLE
   once and universal against any future capture, after which each
   candidate test is two blake2s and a large farm fits 2^50 tests
   inside the TTL. Borderline-feasible for a well-resourced
   adversary: weaker than designed, exactly the attack a PAKE
   exists to prevent. Rate-limiting/alerting on join failures was
   evaluated and rejected: a wrong-word join dies in the WireGuard
   handshake, invisible to the daemon layer, so there is nothing to
   rate-limit or alert on — active guessing is already cost-
   hopeless and the only real surface is the offline oracle. What
   remains is Phase 2, a product decision between: (a) stopgap —
   grow pairWordCount 5 → 8 (80 bits makes even the precompute
   absurd; trivial change, UX cost: typing three more words), or
   (b) the principled fix — pairing v2: the pairing server's key
   becomes random per invite and rides IN the code (the routing
   part is public, ~52 chars; the words stay secret), the tunnel it
   describes is unauthenticated, and a PAKE (SPAKE2) over that
   tunnel authenticates the words and derives the PairIntro channel
   keys — removing the oracle entirely (no offline test exists
   because no key material is word-derived). (b) changes the
   invite/join UX and the pairing protocol shape; touches pairing +
   transport: full CLOWDER_INTEGRATION run required after either.
   The one-round no-ack
   exchange split-brain (inviter paired and invite retired while the
   joiner was stranded, seen on CI) is CLOSED as of 2026-09-26: the
   exchange is now intro / intro / ack / commit-confirm (see
   servePairConn and joinExchange), with the inviter committing only
   on the joiner's ack and the joiner returning only on the
   inviter's confirmation (committing optimistically if that
   confirmation is lost after its own ack was written).
2. **Leave with signed forget-me gossip + roster entry signing** —
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
3. **Android app client** — a clowder client for Android. tailcat has
   Android support (see its android_linux.go and INSTALL.md), so the
   shape is: the daemon packages as an Android library (aar) or runs in
   a foreground service, with a thin UI for init/invite/join/send/
   inbox. Decide the UI approach (plainCompose/gomobile) before
   starting; the daemon package itself must not grow Android deps.
   Termux findings (2026-09-26): GOOS=android GOARCH=arm64
   cross-compiles cleanly (CI keeps it green); on-device builds are
   impossible until Termux ships go >= 1.27.1 (no android
   golang.org/toolchain download); Termux exec via linker64 inserts
   the binary path as os.Args[1] (fixed in argvfix_termux.go, same as
   tailcat); Android kills the daemon without a wake lock. Running
   the CLI in Termux is explicitly NOT a goal — the foreground-service
   app is the answer to both the lifecycle and the daemon+client UX.
4. **Multiple clowders** — named clowders: per-clowder roster files,
   `--clowder` on invite/join/send, Hello carries the clowder name so a
   connection routes to the right roster. One identity, one daemon,
   clowders stay disjoint (flat sync would otherwise merge them).
   Largest refactor; do last, design tombstones against the final
   roster shape. Revisit sync scaling here too (full-roster sync is
   O(N²) bytes per cycle; one-peer-per-tick means propagation latency
   grows linearly).
5. **Nix service packaging: systemd + launchd** — run the daemon as a
   real service from the flake. Linux: a NixOS module
   (`nixosModules.default`) wrapping `packages.default` in a systemd
   unit — `StateDirectory` for the config dir, Restart=on-failure, the
   health endpoint for a watchdog probe, CLOWDER_NAME/CLOWDER_STORER
   options, and the container's auto-init-on-first-start so a fresh
   service bootstraps its identity. macOS: a launchd agent (plist or a
   nix-darwin module) with KeepAlive + RunAtLoad; agent not daemon,
   because the default inbox resolves under $HOME/Downloads — either
   keep it a per-user agent or make the inbox configurable (env/flag)
   before considering a system-level daemon. Sleeping cats are a
   first-class case (the homelab2 wake-then-propagate gap), so the
   unit must survive suspend/resume without restarts. Keep the flake
   free of OS-conditional deps: modules ship as passthrough attributes
   (`nixosModules`, and the plist as a template the README documents).

Shipped recently (context for a fresh session): daemon drain at
shutdown (Run tracks its background goroutines — receipt relays, spool
sweeps, deliveries, roster syncs, accepted connections, the pairing
invite teardown and the health server — in a WaitGroup
and, on ctx cancel, closes live connections and waits for in-flight
work before returning; a caller that cancels Run and waits for it back
gets a daemon that no longer touches disk, the network, or Logf; the
work itself stays uncancelable by design, drain only waits, bounded by
the transfer deadlines — this is what let the test harness stop
racing teardown: TestStorerPullByFetch's CI flake was niko's receipt
relay landing on milo after the test ended, panicking t.Logf and
writing receipts.json into the TempDir RemoveAll was deleting; the
drain also exposed a latent nil-deref in handleFetch — a fetcher
that reads the offer and then drops the conn left ReadMsg returning
a nil message the old code dereferenced, which teardown's
closeLiveConns made CI-frequent — fixed and pinned by
TestHandleFetchClientVanishes),
delivery receipts
(receivers seal {transferID, fileName, deliveredAt} to the ORIGINAL
SENDER's node key and relay direct-or-via-storer like any small
transfer — Offer.Receipt flags the stream, store.Meta preserves the
flag across storer relays so the target routes it to the receipts.json
ledger instead of the inbox; `clow status` shows the ledger; sealed
payload bounded at 4 KiB, digest-checked), and the hardening batch:
receive-time free-space check with a 64 MiB reserve (refused sends
retry when space returns; storer pulls keep the file held), a
daemon-wide transfer cap of 4 concurrent streams (busy receivers
refuse; deferred sends retry), idle tailcat-client eviction after
10 minutes (a cached client holds a live WireGuard engine per peer),
a Hello wire-version field (logged, never refused — no negotiation,
upgrades never partition), fuzz targets for ReadMsg/parsePairCode/
inboxPath (FuzzInboxPath found a real infinite loop: a NUL byte in a
file name made os.Stat fail EINVAL, not IsNotExist, spinning the
collision loop forever — names are now control-stripped and the loop
bounded), the storer-visible offer metadata documented in the README
(sealing it remains future work), instant status paths (status
never probes — it answers from a daemon-side path cache that
refreshPaths maintains in the background: every roster cat pinged in
parallel once at daemon start and on every poll tick, 1s deadline per
probe, so route lines appear within a second of startup even before
any handshake records liveness, offline cats cost nothing user-
visible, and a disco ping still upgrades relayed paths to direct;
the on-demand probing this replaced held `clow status` for seconds
waiting out 3s probe deadlines on offline cats, and an intermediate
cut that skipped probing cats not seen within 10 minutes lost route
lines for the first minutes after a daemon restart, when liveness is
still empty; pinned by TestRefreshPathsProbesEveryCat,
TestPathSnapshotNeverProbes and TestRefreshPathsProbeTimeoutIsBounded
via a fake Pinger), and the startup sync burst (syncBurst dials every
roster peer in parallel once at Run start, 10s per dial, so a cat
that wakes up converges in seconds instead of waiting for the
one-peer-per-tick round-robin to reach the one online peer that
knows what changed — the homelab2 incident's 3.5-minute gap was
exactly this: it woke, but needed macbook's round-robin tick to
learn the newly deployed storer before the storer's pushes stopped
being rejected as unknown keys; the burst rides the drain WaitGroup
and an offline peer costs one bounded dial in parallel, not a tick;
the first cut wrapped the whole connect in a 10s timeout, which cut a
slow-attaching tunnel mid-hello — the cap is gone, the transport's
own 10s dead-peer probe and msgTimeout do the bounding; pinned by
TestStartupSyncBurst with a waker whose poll tick is a
minute out, so only the burst can deliver), symmetric liveness
marking (a dialer marks its target seen the moment its hello reply
lands — handshakeClient — the same point the listener marks the
dialer on its hello, so a connection cut before the roster exchange
still counts both ways; before, only the listener's mark survived
mid-exchange cuts, which made one side show online while its peer
said never-seen; pinned by TestDialerMarksTargetSeenOnHelloReply),
and the single-instance
lock (Run takes an exclusive advisory lock on the config dir — flock
on unix, LockFileEx via golang.org/x/sys on Windows — held until Run
returns and dying with the process, so a crashed daemon's leftover
lock file is relockable; a second Run on the same cat refuses
instead of stealing the live daemon's IPC socket and running two
engines with the same node key, which wedges the tunnel — the
two-keypair invariant's failure mode, hit in the wild when a restart
crossed a live daemon; the lock conflicts even between two Runs in
one process, and three restart tests that silently ran a second
daemon on a live dir now stop the first; pinned by
TestSecondDaemonOnSameDirRefuses). Also shipped
earlier today:
duplicate-name
hardening (pairing refuses a join whose name another key already
claims, with an explicit refusal the joiner understands so it does not
sweep or optimistically commit; roster Get picks the newest Updated
when a name is claimed twice; mergeRemote logs the moment a collision
materializes via sync — two parallel invites from different inviters
can both claim a name, and LWW is per-key so both entries persist;
`clow cats`/`status` tag [duplicate name], and Send warns when the
target is ambiguous; the roster stays add-only: resolution is social,
one cat re-inits with a fresh name), `clow distrust` /
`clow trust` (local, one-directional blocklist in blocked.json, the
same ledger forget uses — never propagated: Send, deliver and
storer-relay selection skip the cat locally, serveConn refuses its
connections, relayed offers whose From names it are refused
(best-effort: relayed offers carry a name, not a key), Poll skips it
as a storer, and it stays visible in `clow cats`/`clow status` with a
[distrusted] tag), a nix flake
`packages.default` building the clow binary (buildGoModule pinned to
nixpkgs' go_1_27 — go.mod requires 1.27.1 and the deps own the floor,
so the default `go` alias is not enough; vendorHash pinned in the
flake; `nix build .#default` → result/bin/clow; the devShell now ships
go 1.27.1 natively, no toolchain download), a confirmed pairing
exchange (intro / intro / ack / commit-confirm: the inviter commits
only on the joiner's ack — a lost reply leaves the invite alive for
the joiner's retry — and the joiner returns only on the inviter's
confirmation, so both rosters are durable before `clow join` exits),
a pprof pass (CLOWDER_PPROF
/ `clow daemon --pprof` serves net/http/pprof on the health endpoint,
opt-in only; a loopback end-to-end benchmark, BenchmarkLoopbackSend1MiB,
profiling found the receive path dominated by per-chunk allocations in
envelope.OpenStream — fixed by reusing the ciphertext, plaintext and
nonce buffers, pinned by TestOpenStreamAllocationBound: end-to-end
allocations per 1 MiB transfer dropped 2.46 MB to 0.36 MB; CPU is
file-IO-bound (75% syscalls), XChaCha20-Poly1305 is ~6%, so the cipher
is not worth optimizing; goroutine/mutex/block profiles of a busy
multi-peer daemon remain unprofiled), a self-hosted DERP map
config (CLOWDER_DERPMAP_URL / `clow daemon --derp-map`, plumbed into the
server, dials and both pairing sides; loopback-tested against an
httptest-served map), a dead-peer dial fix (tailcat's meow Ping is
one-shot per client, so a cached client reported a just-offline peer
alive and the dial wedged in netstack SYN retries for the whole
delivery context, starving the storer fallback; Dial now probes with a
real disco ping and bounds the tunnel dial), a black-box end-to-end
integration test (integration_test.go at the repo root: the test
binary re-execs itself as the real clow binary, three real daemon
processes pair with real codes, send direct, relay via a storer while
the target is offline and pull it back with `clow fetch`; run with
CLOWDER_INTEGRATION=1 like the daemon package's Integration tests),
the engine-leak integration test redesigned to count wireguard
goroutines and poll for them to drain (the old one-shot global
NumGoroutine sample measured in-flight churn, not leaks), a `persist/`
package consolidating every durable-file write (atomic temp+rename,
LoadJSON/SaveJSON ledger helpers; roster, blocklist and me/identity
saves are now atomic, where the roster and blocklist were
torn-write-vulnerable before), protocol.Conn Answer/Ack helpers
replacing hand-built Messages, an HTTP health endpoint
for container probes (`clow daemon --health` / CLOWDER_HEALTH_ADDR,
serving 200 "ok" on / and /healthz, closed with the daemon), a
daemon-test harness fix (TestMain points TMPDIR at a short /tmp path;
the default macOS TMPDIR overflowed the unix socket path limit, so
long-named tests died at the IPC listen), storer capacity
(`clow storer on --max 10G`, required to enable; deposits are refused
when they would overflow, with an atomic first-come-first-served
reservation so concurrent deposits never oversubscribe; delivery
frees space and refused sends retry later), duplicate-transfer
prevention (in-flight claims per outbox entry and per received offer
ID — the retry ticker, sweep and pull races no longer restart big
transfers; the deliver context now spans stream-sized transfers),
size-lie enforcement (a sealed stream must match its announced size
exactly or the receive is killed without an ack), a 64 MiB real-DERP
integration test (CLOWDER_INTEGRATION), in-flight transfer
progress in `clow status` (percent per queued send, plus receiving
rows; counted per 64 KiB chunk on the sealed stream, both directions),
container support
(Dockerfile; daemon auto-initializes on first start; CLOWDER_NAME and
CLOWDER_STORER env vars; k8s guidance in the README), dropbox storer
mode (`clow storer dropbox`: third-party storage only — refuses
deliveries to itself, cannot send or fetch for itself; flag propagates
via roster), storer push sweep (a storer delivers held files as soon as
their target is online and known, plus a sweep on the poll tick —
`clow fetch` remains as a manual pull), `clow rotate` (new pre-shared
key/address under the same identity, announced via roster sync, fails
unless one peer acknowledged; the daemon restart serves the new
address), passive liveness ("online / seen Xm ago" in `clow status`,
updated on every successful handshake), `clow forget` (roster entry
plus outbox), dash-numbered inbox collisions, transfer stats, the
two-keypair fix (see invariants).

Known caveats: the golangci-lint-action version in CI (v7 + v2.13.2) is
unverified until a green run is observed; storer spools have TTL but no
size quota; no resume of interrupted transfers; rosters created before
the two-keypair fix must be re-paired (`clow forget` + invite/join).
