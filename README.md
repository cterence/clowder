# Clowder

A clowder is a group of cats. Clowder is a decentralized, asynchronous
file-transfer mesh between your machines, built on
[tailcat](https://github.com/tailscale/tailcat) (Tailscale's data plane
without the control plane: WireGuard-encrypted, DERP-relayed,
NAT-traversing, no accounts).

- **No addresses to copy.** Cats trust each other via `clow invite` /
  `clow join` pairing codes: eight words plus a number, read to each
  other once.
- **Asynchronous sends.** If the target is online the file goes straight
  there; if not, any cat that declared itself a **storer** holds the
  encrypted file until the target is online again. `clow send` shows the
  transfer's progress by default (`--async` queues and returns).
  Storers can't read what they hold: every file is end-to-end sealed to
  its recipient, in memory-bounded 64 KiB chunks.
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

Received files go to your OS downloads dir, one folder per clowder
(`~/Downloads/clowder/default` by default; change with `clow inbox
--set DIR`). Config lives under `$CLOWDER_DIR` (the clowder base) or
the user config home (`~/.config/clowder` on Linux,
`~/Library/Application Support/clowder` on macOS), with each clowder
in its own directory under the base.

Building needs Go >= 1.27.1 (a dependency floor, not ours to lower).
For Android, cross-compile from a machine with that toolchain — the
on-device one cannot auto-download it (golang.org/toolchain ships no
android build):

    GOOS=android GOARCH=arm64 go build -o clow .

## Multiple clowders

A clowder is a config dir: one daemon process per clowder, and the
clowder name is purely local — it picks the directory and never rides
the wire. A second clowder on the same machine:

    clow --clowder work init --name laptop-work
    clow --clowder work daemon       # or another nix instance

The name resolves `--clowder` flag > `$CLOWDER` env > `default` (a
no-flag command always means the default clowder — `clow send …` as
today). Names are lowercase slugs (`^[a-z][a-z0-9-]{0,31}$`) because
they become directory names and service unit suffixes. Two clowders on
one host coexist on the same port: the tailcat listener is virtual, on
each daemon's own overlay — no host socket is bound. Cross-clowder cat
sameness is a non-goal: each clowder is its own identity and trust
domain, so rotate or leave one without touching the others. A pre-
nesting config dir migrates into `default/` automatically on first
run.

## Config layout

State lives under the clowder's directory: `$CLOWDER_DIR/<clowder>`
(default base: the OS user config home — `~/.config/clowder` on Linux,
`~/Library/Application Support/clowder` on macOS). The files are
deliberately separate: secrets never share a
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

Every command takes a global flag before the command word:
`clow [--clowder NAME] <command> …` names the local clowder
(default: `$CLOWDER`, else `default`).

    clow init [--name NAME] [--dir CONFIG_DIR] [--inbox INBOX_DIR]
                                                        create the identity
    clow daemon [--port N] [--health ADDR] [--derp-map URL]
                                                        run the mesh daemon
    clow invite                                         8-word pairing code (5 min, one join)
    clow join <CODE>                                    pair with the inviter
    clow send [--async] [--clipboard] [--storer] <CAT> <FILE-OR-DIR>...
                                                        send files or a
                                                        directory tree (one
                                                        transfer per file, the
                                                        tree's relative paths
                                                        recreated in the
                                                        target's inbox; symlinks
                                                        are refused), or the
                                                        clipboard with
                                                        --clipboard; watches
                                                        progress unless --async;
                                                        --storer skips the direct
                                                        attempt and its dial timeout
    clow inbox [--set DIR]                              list received files / change inbox
    clow storer [--max SIZE] on|off|dropbox                           volunteer to hold files for others
                                                        (dropbox: third parties only)
    clow cancel [<ID>]                                   cancel a pending send (all when no ID given)
    clow leave                                         depart: signed goodbye, cats drop you
    clow forget <CAT-OR-KEY>                            drop a cat from the roster (it
                                                        stays dropped: syncs re-adding
                                                        it are refused; the KEY — or its
                                                        prefix as shown by clow status —
                                                        picks one of two same-named cats)
    clow status [--addresses]                           config, stats, outbox, spool,
                                                        roster (--addresses also prints
                                                        tailcat addresses)
    clow reset [--yes]                                  wipe this cat (identity, rosters);
                                                        a local wipe — run clow leave
                                                        first to depart, and stop the
                                                        daemon first
    clow version                                        print the build revision

## Running in a container

The image runs the daemon unprivileged; tailcat is userspace-only (no
TUN, no host routes, no root) and the mesh opens **no inbound ports** —
it only needs outbound access to the DERP relays (TCP 443) and to
peers' public endpoints. So no Service is needed for the mesh itself.

## Running as a service

NixOS gets a `services.clowder.instances` module from the flake — one
systemd unit per clowder, keyed by its local name:

    {
      inputs.clowder.url = "git+ssh://git@github.com/cterence/clowder.git";
      # in your NixOS config:
      imports = [ clowder.nixosModules.default ];
      services.clowder.instances.default = {
        enable = true;
        name = "server-cat";          # optional, defaults to the hostname
        storer = "on";                 # optional: on | off | dropbox
        maxCapacity = "10G";           # optional, with storer
        healthAddr = "127.0.0.1:8080"; # optional, GET /healthz
      };
      # a second clowder on the same host is another instance:
      services.clowder.instances.work.enable = true;
    }

Each instance runs as the `clowder` system user with its state in
`/var/lib/clowder/<name>` (a fresh directory auto-inits a new cat), restarts on
failure, and survives suspend/resume — no watchdog, because a watchdog's
clock counts sleep time and would kill a healthy daemon on wake. Hang
detection belongs to external probes of `healthAddr` (distinct per
instance when enabled). The CLI reaches an instance's daemon through
the same socket:

    sudo -u clowder env CLOWDER_DIR=/var/lib/clowder CLOWDER=default clow status

On macOS, nix-darwin gets the same `services.clowder.instances`
module from `clowder.darwinModules.default` — it wires a per-user
launchd agent per instance (`KeepAlive` + `RunAtLoad`, so it survives
suspend/resume without restarts) and installs/loads them on switch.
Home Manager gets `clowder.homeManagerModules.default`: a launchd
agent on macOS, a systemd user unit on Linux, running as your own user
so `clow invite`/`clow status` work as always — the daemon shares your config
dir and downloads inbox. Don't enable an instance on a host that also
runs the system-level NixOS instance of the same name: two daemons are
two cats.

## Android

An Android client lives in `android/`: the daemon ships as the same
`GOOS=android` binary, exec'd by a service that runs it while the app
is on screen (promoting to a dataSync foreground service only while a
transfer is in flight), with a plain Compose UI speaking the CLI's
unix-socket IPC and a header clowder switcher. See
`android/README.md`.


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
  rosters — run `clow leave` before decommissioning a cat (reset is
  a purely local wipe and does not announce anything). A StatefulSet
  fits best.
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

## Walkthroughs

### Two laptops and a file

On the first machine (once):

    clow init --name laptop
    clow daemon                 # leave it running

On the second machine, same two steps with a different name, then pair
once — the code is eight words plus a region number, valid five
minutes, one use:

    laptop$  clow invite
    printing: clow join hazel-meadow-quartz-amber-ember-303
    phone$   clow join hazel-meadow-quartz-amber-ember-303

Both rosters sync automatically from here: a third cat that joins
later discovers everyone at once. Send a file — the command follows
the transfer and returns once it is delivered (or a storer holds it
for an offline target; Ctrl-C cancels the send):

    laptop$  clow send phone photo.jpg
    queued photo.jpg for phone (id 9f3a...)
    sending photo.jpg to phone:  64% of 2.1 MB at 8.2 MB/s
    sent photo.jpg to phone (delivered, or held by a storer until it is online)
    phone$   clow inbox
    2026-09-26 14:02:11  ~/Downloads/clowder/photo.jpg

### Adding a storer

A storer is just a paired cat with the role enabled: it holds sealed
files for offline targets and pushes them as soon as the target is
online. Pair the third
machine as above, then:

    server$  clow storer --max 10G on
    storer duty on: 10.0 GiB capacity

Two things to know about the role:

- The capacity is a hard reservation: deposits that would overflow it
  are refused, and the sender retries later (or via another storer).
- Held files expire after 7 days unclaimed (`clow status` shows the
  spool).

A **dropbox** storer is third-party storage only — it holds files for
others but takes no deliveries itself and cannot send its own:

    server$  clow storer dropbox --max 100G

For an always-on storer, run the container instead — the role is
declared as desired state on every start, so a restart reasserts it:

    docker run -d --name clowder-storer \
        -v clowcfg:/config \
        -e CLOWDER_NAME=storer \
        -e CLOWDER_STORER=on \
        -e CLOWDER_MAX=10G \
        clowder

Pair it once via `kubectl exec` (or `docker exec`), and remember the
config volume: a pod that restarts with empty storage comes back as a
brand-new cat.

## Privacy: what storers see

Storers hold sealed streams they cannot open, but the Offer metadata
relayed through them is visible to the storer by design: file name,
size, plaintext digest, and the sender's and target's declared names.
Choosing trustworthy storers (or running your own with `clow storer`)
is the mitigation; sealing the offer metadata itself is tracked in
AGENTS.md.

## Rotating keys

All three keypairs (identity, dial, sign) live in the config dir and
none rotates in place — rotation is re-incarnation with a re-pair:

    clow leave             # while the daemon runs: signed goodbye,
                           # every cat drops the old keys
    clow reset --yes       # daemon stopped: wipes the config dir
    clow init <NAME>       # fresh identity; the old name or a new one
                           # (a same-name re-pair outranks the leave
                           # tombstone)
    clow daemon            # then invite/join with any one member —
                           # roster sync carries the new you to the rest

Two accepted risks, by decision: the identity files rest unencrypted
on disk (the OS's disk encryption owns that layer; a passphrase would
only move the secret to a prompt), and `clow forget`'s blocklist stays
local — a propagated one would let a paired cat poison other cats'
views, which signatures cannot arbitrate against their own signer.

## Design

See [docs/specs/2026-09-25-clowder-design.md](docs/specs/2026-09-25-clowder-design.md)
for the architecture: the protocol, the envelope format (age-style
chunked sealing over the cat's tailcat node key), the roster merge
rules, and the storer spool lifecycle.
