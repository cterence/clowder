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
- Both sides of a pairing derive ephemeral keypairs from the words;
  real addresses ride the encrypted pairing channel.
- Roster sync is add-only LWW; never delete entries outside the (pending)
  tombstone mechanism.
- Keep transferred files out of memory: stream everywhere; the sealed
  stream is opaque to storers by construction.

## Pending work

Ordered; do not reorder without a reason. Update this list and the
README when something ships.

1. **Dropbox storer** — a third cat mode (`clow storer dropbox`): accepts
   deposits and serves fetches for third parties only; refuses direct
   sends to itself; cannot originate sends. Flag propagates via roster
   entries like Storer. No protocol changes needed.
2. **Leave with signed forget-me gossip** — derive an Ed25519 keypair
   from the node key seed (ed25519.NewKeyFromSeed(nodeRaw32)); announce
   the sign-public in Hello/roster entries; `clow leave` broadcasts a
   signed {leaver, timestamp} to all reachable peers; recipients drop
   the leaver (roster, allowlist, spool) and re-broadcast once. Roster
   sync must carry signed tombstones that always outrank later unsigned
   re-adds (offline peers catch up on next sync).
3. **Multiple clowders** — named clowders: per-clowder roster files,
   `--clowder` on invite/join/send, Hello carries the clowder name so a
   connection routes to the right roster. One identity, one daemon,
   clowders stay disjoint (flat sync would otherwise merge them).
   Largest refactor; do last, design tombstones against the final
   roster shape.

Known caveats: the golangci-lint-action version in CI (v7 + v2.13.2) is
unverified until a green run is observed; storer spools have TTL but no
size quota; no resume of interrupted transfers.
