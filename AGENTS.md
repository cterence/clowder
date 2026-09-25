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
    roster/          cats, LWW merge, persistence; identity = node key
    protocol/        CBOR messages, 4-byte framing; sealed streams are raw bytes
    store/           storer spool: atomic writes, TTL, delete-on-ack
    daemon/          the runtime: serve/send/fetch/sync, pairing, IPC, stats,
                     TailcatTransport (production) + LocalTransport (tests)
    docs/specs/      the design doc (protocol, envelope, roster rules)

## Key invariants

- Trust is established ONLY via pairing (`clow invite`/`clow join`);
  tailcat addresses are never exchanged by hand. Pairing codes are
  5 words (50 bits) + the inviter's DERP region, valid 5 minutes.
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

Ordered; do not reorder without a reason. Update this list and the
README when something ships.

1. **Leave with signed forget-me gossip** — derive an Ed25519 keypair
   from the node key seed (ed25519.NewKeyFromSeed(nodeRaw32)); announce
   the sign-public in Hello/roster entries; `clow leave` broadcasts a
   signed {leaver, timestamp} to all reachable peers; recipients drop
   the leaver (roster, allowlist, spool) and re-broadcast once. Roster
   sync must carry signed tombstones that always outrank later unsigned
   re-adds (offline peers catch up on next sync).
2. **Multiple clowders** — named clowders: per-clowder roster files,
   `--clowder` on invite/join/send, Hello carries the clowder name so a
   connection routes to the right roster. One identity, one daemon,
   clowders stay disjoint (flat sync would otherwise merge them).
   Largest refactor; do last, design tombstones against the final
   roster shape.
3. **Android app client** — a clowder client for Android. tailcat has
   Android support (see its android_linux.go and INSTALL.md), so the
   shape is: the daemon packages as an Android library (aar) or runs in
   a foreground service, with a thin UI for init/invite/join/send/
   inbox. Decide the UI approach (plainCompose/gomobile) before
   starting; the daemon package itself must not grow Android deps.
4. **Self-hosted DERP map config** — an env/flag (e.g.
   CLOWDER_DERPMAP_URL) plumbed into the tailcat Server/Clients
   (DERPMapURL) so air-gapped clusters can run their own DERP relays.
   Needed for serious Kubernetes use; see the README container section.
5. **HTTP health endpoint** for container probes (currently exec
   `clow status`).

Shipped recently (context for a fresh session): container support
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
