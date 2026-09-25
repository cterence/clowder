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

    go build -o clow .          # or: nix develop
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

## Commands

    clow init [--name NAME] [--dir DIR] [--inbox DIR]   create the identity
    clow daemon [--port N]                              run the mesh daemon
    clow invite                                         5-word pairing code (5 min, one join)
    clow join <CODE>                                    pair with the inviter
    clow send <CAT> <FILE>                              async send (queues if offline)
    clow fetch                                         pull files storers hold for me
    clow inbox [--set DIR]                             list received files / change inbox
    clow cats                                           list the clowder
    clow storer on|off                                  volunteer to hold files for others
    clow outbox clear                                   drop pending sends
    clow status                                         config, stats, outbox, spool, roster
    clow reset [--yes]                                  wipe this cat (identity, rosters)

## Design

See [docs/specs/2026-09-25-clowder-design.md](docs/specs/2026-09-25-clowder-design.md)
for the architecture: the protocol, the envelope format (age-style
chunked sealing over the cat's tailcat node key), the roster merge
rules, and the storer spool lifecycle.
