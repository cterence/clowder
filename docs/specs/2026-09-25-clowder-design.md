# Clowder Design

Clowder is a decentralized, asynchronously-delivered file transfer mesh built
on tailcat. Binary name: `clow`.

## Concepts

- **Cat**: a member of the clowder. Identified by a persistent tailcat node
  keypair (`tailcat.PrivateKey` persisted as JSON in the config dir). Declares
  a human-readable, roster-unique name.
- **Roster**: each cat's local list of known cats (name, tailcat Addr, storer
  flag, last-updated timestamp). Trust is pairwise at the connection level
  (`Server.AllowedClients`), but membership propagates via roster sync.
- **Storer**: a cat that declared itself willing to hold encrypted files for
  offline recipients.
- **Outbox**: sender-side queue of sealed, not-yet-delivered files.
- **Spool**: storer-side store of sealed files awaiting the target's fetch.
- **Inbox**: receiver-side directory where decrypted files land.

## Tailcat mapping

| Clowder concept | Tailcat primitive |
|---|---|
| Stable cat identity/address | persisted `tailcat.PrivateKey` (node key + PSK + RegionID -1) |
| Trust | `Server.AllowedClients` from roster node keys |
| Liveness probe | `Client.Ping` (meow over DERP) |
| Connections | `Server.Listen` on a fixed port; `Client.DialTCPPort` |
| E2E file encryption | `key.NodePrivate.SealTo`/`OpenFrom` (sealed box) — no extra keyset |

## Protocol

CBOR messages, 4-byte big-endian length framing, over the tailcat TCP stream
on a fixed port (default 2569).

1. On connect, both sides send `Hello {Name, Addr, Storer}`, then both send
   their full roster (`Roster []Cat`). Union merge, last-write-wins by
   `Updated` (tie-break: lexicographically greater Addr string wins).
   Identity key = the node public key embedded in `Cat.Addr`. Roster entries
   are never deleted in v1.
2. **Direct send**: sender dials target, `Offer {ID, FileName, Size,
   From, TargetKey}` → `Answer{OK}` → sealed stream (exactly Size raw
   bytes) → target decrypts streaming to its inbox, `Ack{Kind:
   delivered}`.
3. **Storer send**: target down → sender dials a storer, same `Offer`
   (target is the third cat) → storer copies the opaque sealed stream to
   its spool, `Ack{Kind: stored}`.
4. **Fetch**: a cat periodically (and on daemon start) asks every known
   storer `Pending{Query}` → `Pending{Files}` → for each: `Fetch{ID}` →
   storer replays the sealed stream → recipient decrypts, saves, `Ack{Kind:
   delivered}` → storer deletes from spool. Additionally the storer runs a
   **push sweep** on its poll tick and right after each deposit: any held
   file whose target is in the storer's roster is offered directly over a
   fresh connection, so an online target receives its files without polling.
5. Sealed streams are chunked (64 KiB XChaCha20-Poly1305 chunks under a
   per-stream file key sealed to the recipient's node key, age's STREAM
   construction), so arbitrarily large files transfer without either side
   holding them in memory, and a storer provides availability, not
   confidentiality: it spools and replays the stream without ever being
   able to decrypt it.

If neither target nor any storer is reachable, the send stays in the
sender's outbox (referencing the source file) and the daemon retries
(target first, then storers) on a ticker, re-sealing the file per attempt.

## Components

- `envelope/` — chunked streaming seal/open using the tailscale sealed
  box for the header (which carries the file key) and XChaCha20-Poly1305
  chunks; the sealed stream is self-contained and storer-opaque.
- `roster/` — `Cat`, LWW merge, JSON persistence, name lookup.
- `protocol/` — message types, CBOR framing, read/write helpers.
- `store/` — storer spool: put/pending/fetch/delete/TTL sweep. Atomic writes.
- `daemon/` — the mesh runtime: connection serving, outbox retry loop,
  fetch loop, roster sync, IPC socket. Depends on a `Transport` interface so
  tests run over loopback TCP while production uses tailcat.
- `main.go` — `clow` CLI: `init`, `add`, `send`, `fetch`, `cats`, `storer`,
  `daemon`. Thin IPC client for everything except `init`.

## CLI surface

    clow init [--name NAME] [--dir DIR]      # create identity, print key info
    clow daemon [--port N]                   # run the mesh daemon
    clow add <ADDR> --name NAME              # trust a cat (out of band)
    clow cats                                # list roster + me
    clow send <CAT> <FILE>                   # async send
    clow fetch                               # poll storers now
    clow storer on|off                       # declare storer role
    clow status                              # outbox, spool and roster summary

Config dir default: `$XDG_CONFIG_HOME/clowder` or `~/.config/clowder`.
Files: `identity.json`, `roster.json`, `outbox/`, `spool/`, `inbox/`, `clow.sock`.

## Non-goals / v1 limits

- Files transfer as chunked sealed streams (64 KiB chunks, envelope
  package): no size limit, memory bounded by one chunk per side.
- No resume of partially transferred streams: a failed transfer restarts
  from the first chunk.
- No roster deletions; renames are LWW.
- No gossip: full roster sync on every connection (scales to ~1000 cats).
- Storer delivery is push-first (sweep) with pull as fallback; a target
  that races both paths may see a dash-numbered duplicate file.
- Tests use loopback TCP through the Transport interface; a live tailcat/DERP
  smoke test is out of scope for CI and must be run manually.
- Reusing the WireGuard node key for file sealing is accepted (the key is
  already a Curve25519 identity key; separation would buy little here).
