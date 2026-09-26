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
  5 words (50 bits) + the inviter's DERP region, valid 5 minutes. The
  region is a fast-path hint, not a requirement: `clow join` tries it
  first, then sweeps the other regions (relay presence can flap away
  from the encoded one on networks with two nearby relays), so a wrong
  hint costs seconds, not the pairing.
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

## Pending work

Ordered by complexity, easiest first; do not reorder without a
reason. Update this list and the README when something ships.

1. **Nix packaging** — a flake `packages.default` building the clow
   binary (buildGoModule; needs a vendorHash — compute with a first
   `nix build`, then pin). Keep the devShell as the default output.
   BLOCKED (2026-09-26): buildGoModule needs go >= 1.27.1 — clowder,
   tailcat and tailscale.com all declare `go 1.27.1`, and Go refuses
   to build a module whose go directive exceeds the running toolchain.
   nixpkgs unstable ships go 1.26.7 (`go`) and 1.27rc2 (`go_1_27`),
   and an rc sorts BELOW 1.27.1. Revisit when nixpkgs carries
   go >= 1.27.1; the go.mod directive cannot be lowered (the deps own
   the floor).
2. **Distrust a cat (local, one-directional)** — `clow distrust <CAT>`
   / `clow trust <CAT>` to undo: a persisted blocklist (keys, in
   blocked.json, NOT propagated — one cat's decision, unlike the
   planned signed-leave gossip). A distrusted cat is refused both
   ways: Send and storer-relay selection skip it locally; serveConn
   closes immediately from distrusted peers; incoming Offers whose
   From names a distrusted cat are refused (best-effort: relayed
   offers carry a name, not a key). Kept visible in `clow cats` with a
   [distrusted] tag, unlike forget. NOTE: tailcat's AllowedClients is
   add-only (no RemoveAllowedClient) — so forget today leaves the
   cat's key able to connect; the serveConn-level check must back
   both features (or upstream tailcat grows a removal API).
3. **Delivery receipts** — approved design, not yet built: when a
   target receives a file (direct or via storer) it seals a tiny
   receipt {transferID, fileName, deliveredAt} to the sender's node key
   with its own (sealed-box authenticated, storer-opaque) and relays it
   direct-or-via-storer like any small message; the sender keeps a
   receipts.json ledger shown in `clow status`, closing the loop for
   sends that left the outbox while the sender was offline.
4. **Hardening batch** (from the 2026-09-26 design review):
   - Inbox quota / free-space check on receive (a trusted cat can
     fill the receiver's disk today; storers have capacity, direct
     receivers do not).
   - Offer metadata is visible to storers (file name, size, digest,
     sender, target): document explicitly, or seal the offer payload
     on the storer path.
   - Wire protocol version field in Hello before the protocol
     ossifies (no version negotiation today).
   - Duplicate roster names: Get(name) is map-iteration order; prefer
     newest Updated and flag duplicates in `clow cats`.
   - Idle tailcat-client eviction (engines accumulate per peer; the
     close machinery exists).
   - Global transfer concurrency cap (claims are per-ID only).
   - Fuzz targets for ReadMsg/parsePairCode/inboxPath (untrusted
     input decode paths).
5. **Pairing hardening: offline guessability + PAKE** — the pairing
   words currently derive the WireGuard static keys and PSK directly
   (derivePairing, daemon/pairing.go), and the file comment claims
   guessing is "active-only, nothing verifiable offline". That claim
   is doubtful: WireGuard's handshake MAC1 is keyed by the
   responder's static public key, which is itself derived from the
   words, so anyone who records one pairing handshake on the
   invite's DERP region can test candidate word codes offline —
   the 50-bit code is weaker than designed, and this is exactly the
   attack a PAKE exists to prevent (see magic-wormhole's SPAKE2).
   Phase 1 (cheap): verify the MAC1 brute-force reasoning against a
   captured tailcat pairing handshake, and rate-limit/alert on join
   failures meanwhile. Phase 2, if confirmed: run a real PAKE
   (SPAKE2 or OPAQUE) over the pairing channel and derive the
   tunnel keys from its output instead of from the words, or grow
   the code length as a stopgap. Touches pairing + transport: full
   CLOWDER_INTEGRATION run required after.
6. **Leave with signed forget-me gossip + roster entry signing** —
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
7. **Android app client** — a clowder client for Android. tailcat has
   Android support (see its android_linux.go and INSTALL.md), so the
   shape is: the daemon packages as an Android library (aar) or runs in
   a foreground service, with a thin UI for init/invite/join/send/
   inbox. Decide the UI approach (plainCompose/gomobile) before
   starting; the daemon package itself must not grow Android deps.
   (GOOS=android GOARCH=arm64 cross-compiles cleanly today; CI keeps
   it green. On-device Termux cannot auto-download go >= 1.27.1 — no
   android build of golang.org/toolchain — so build on a real box.)
8. **Multiple clowders** — named clowders: per-clowder roster files,
   `--clowder` on invite/join/send, Hello carries the clowder name so a
   connection routes to the right roster. One identity, one daemon,
   clowders stay disjoint (flat sync would otherwise merge them).
   Largest refactor; do last, design tombstones against the final
   roster shape. Revisit sync scaling here too (full-roster sync is
   O(N²) bytes per cycle; one-peer-per-tick means propagation latency
   grows linearly).

Shipped recently (context for a fresh session): a pprof pass (CLOWDER_PPROF
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
