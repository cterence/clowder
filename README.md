# Clowder

A clowder is a group of cats. Clowder is a decentralized, asynchronous
file-transfer mesh between your machines, built on
[tailcat](https://github.com/tailscale/tailcat) (Tailscale's data plane
without the control plane: WireGuard-encrypted, DERP-relayed,
NAT-traversing, no accounts).

- **No addresses to copy.** Cats trust each other via `clow invite` /
  `clow join` pairing codes: five words plus a number, read to each
  other once.
- **Asynchronous sends.** If the target is online the file goes straight
  there; if not, any cat that declared itself a **storer** holds the
  encrypted file until the target fetches it. Storers can't read what
  they hold: every file is end-to-end sealed to its recipient, in
  memory-bounded 64 KiB chunks.
- **Self-healing mesh.** Every connection syncs rosters, so every cat
  converges on the same member list.

## Quickstart

    go build -o clow .          # or: nix build .#default (result/bin/clow)
    clow init --name laptop     # once per machine
    clow daemon                 # keep running

On the other machine, same thing, then:

    clow invite                 # prints: clow join hazel-meadow-quartz-amber-ember-303
    clow join hazel-meadow-quartz-amber-ember-303   # on the other machine

    clow send macbook DSC01234.jpg
    clow inbox                  # where received files landed

Received files go to your OS downloads dir (`~/Downloads/clowder` by
default; change with `clow inbox --set DIR`). Config lives under
`$CLOWDER_DIR` or the user config home (`~/.config/clowder` on Linux,
`~/Library/Application Support/clowder` on macOS).

Building needs Go >= 1.27.1 (a dependency floor, not ours to lower).
For Android, cross-compile from a machine with that toolchain — the
on-device one cannot auto-download it (golang.org/toolchain ships no
android build):

    GOOS=android GOARCH=arm64 go build -o clow .

## Config layout

State lives under `$CLOWDER_DIR` (default: the OS user config home —
`~/.config/clowder` on Linux, `~/Library/Application Support/clowder`
on macOS). The files are deliberately separate: secrets never share a
file with frequently-rewritten state, and every ledger has a default
path if lost — except the identity.

| file | what it is | secret | safe to delete (daemon stopped) |
|---|---|---|---|
| `identity.json` | node keypair, pre-shared key, region hint — the cat's identity and address | yes | no — losing it makes you a brand-new cat |
| `clientkey.json` | outbound-dial keypair that peers allowlist | yes | no — peers must re-pair |
| `me.json` | declared name, storer role, inbox dir | no | no — re-init to rebuild |
| `roster.json` | the cats you know (LWW-merged on every sync) | no | yes — re-syncs from peers |
| `stats.json` | lifetime transfer counters | no | yes — counters restart at zero |
| `blocked.json` | keys of forgotten cats | no | yes — forget state is lost |
| `outbox/` | pending sends, one file per transfer | no | yes — drops queued sends |
| `spool/` | sealed streams held for others (storer duty) | no | yes — drops held files |

## Commands

    clow init [--name NAME] [--dir CONFIG_DIR] [--inbox INBOX_DIR]
                                                        create the identity
    clow daemon [--port N] [--health ADDR] [--derp-map URL]
                                                        run the mesh daemon
    clow invite                                         5-word pairing code (5 min, one join)
    clow join <CODE>                                    pair with the inviter
    clow send <CAT> <FILE>                              async send (queues if offline)
    clow fetch                                          pull files storers hold for me
    clow inbox [--set DIR]                              list received files / change inbox
    clow cats                                           list the clowder
    clow storer on|off|dropbox                          volunteer to hold files for others
                                                        (dropbox: third parties only)
    clow outbox clear                                   drop pending sends
    clow rotate                                         new address, announced to the clowder
    clow forget <CAT>                                   drop a cat from the roster
    clow distrust <CAT>                                 block a cat locally (both ways, no gossip)
    clow trust <CAT>                                    undo distrust
    clow status                                         config, stats, outbox, spool,
                                                        receipts, roster
    clow reset [--yes]                                  wipe this cat (identity, rosters)

## Running in a container

The image runs the daemon unprivileged; tailcat is userspace-only (no
TUN, no host routes, no root) and the mesh opens **no inbound ports** —
it only needs outbound access to the DERP relays (TCP 443) and to
peers' public endpoints. So no Service is needed for the mesh itself.

    docker build -t clowder .
    docker run -d --name clowder -v clowcfg:/config \
        -e CLOWDER_NAME=whiskers -e CLOWDER_STORER=dropbox clowder

The daemon auto-initializes a fresh cat on first start. Everything else
runs through the CLI inside the container:

    kubectl exec clowder-0 -- clow invite
    kubectl exec clowder-0 -- clow status

Environment: `CLOWDER_DIR` (config path), `CLOWDER_NAME` (cat name;
defaults to the hostname, i.e. the pod name), `CLOWDER_STORER`
(`on`/`off`/`dropbox`, applied on every start), `CLOWDER_HEALTH_ADDR`
(optional HTTP probe address, e.g. `:8080`; also `clow daemon --health`),
`CLOWDER_DERPMAP_URL` (URL of a JSON DERP map to use instead of
tailcat's default; also `clow daemon --derp-map`), `CLOWDER_PPROF`
(`1` serves net/http/pprof under `/debug/pprof/` on the health
endpoint; opt-in, requires `CLOWDER_HEALTH_ADDR`).

Things to know before running it on Kubernetes:

- **Mount a persistent volume** on the config dir. The identity lives
  there; a pod that restarts with an empty volume comes back as a
  brand-new cat, and the old one stays as a zombie in everyone's
  rosters (until signed-leave ships). A StatefulSet fits best.
- **One replica per cat**: clowder is a mesh of individual identities,
  not a horizontally-scaled service.
- **DERP reachability**: the default DERP map is fetched from
  tailcat.dev at startup. In an air-gapped or egress-restricted
  cluster, run your own DERP relays and point the daemon at your map
  with `CLOWDER_DERPMAP_URL` (or `clow daemon --derp-map`).
- **Clocks matter**: roster merges are last-write-wins on timestamps,
  so keep node clocks sane (NTP).
- **NAT**: outbound UDP enables direct peer-to-peer paths when the CNI
  allows it; otherwise everything relays over DERP, which always
  works but is slower.
- **Probes**: set `CLOWDER_HEALTH_ADDR` (e.g. `:8080`) and point
  liveness/readiness probes at `http://<pod>:8080/healthz` — it answers
  `200 ok` while the daemon runs. The address needs a `containerPort`
  but no Service; without it, an exec probe on `clow status` works.

## Privacy: what storers see

Storers hold sealed streams they cannot open, but the Offer metadata
relayed through them is visible to the storer by design: file name,
size, plaintext digest, and the sender's and target's declared names.
A storer also sees delivery receipts' existence (a tiny transfer
flagged as a receipt) but not their contents — the receipt payload is
sealed to the original sender. Choosing trustworthy storers (or
running your own with `clow storer`) is the mitigation; sealing the
offer metadata itself is tracked in AGENTS.md.

## Design

See [docs/specs/2026-09-25-clowder-design.md](docs/specs/2026-09-25-clowder-design.md)
for the architecture: the protocol, the envelope format (age-style
chunked sealing over the cat's tailcat node key), the roster merge
rules, and the storer spool lifecycle.
