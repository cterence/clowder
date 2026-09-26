// Package daemon is the clowder mesh runtime: it owns the cat's identity,
// serves the clowder protocol over a Transport (tailcat in production,
// loopback TCP in tests), syncs rosters with every peer it talks to,
// delivers outbound files directly or via storer cats, fetches held files
// from storers, and exposes a small IPC socket for the clow CLI.
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"

	"clowder/envelope"
	"clowder/persist"
	"clowder/protocol"
	"clowder/roster"
	"clowder/store"
)

// DefaultPort is the clowder protocol port on a cat's tailcat address.
const DefaultPort = 2569

const (
	defaultRetryEvery = 30 * time.Second
	defaultPollEvery  = 60 * time.Second

	// msgTimeout bounds reads of a single protocol message; long enough
	// for a slow peer's handshake, short enough to reclaim dead conns.
	msgTimeout = 2 * time.Minute
	// streamTimeout bounds a whole sealed-stream transfer.
	streamTimeout = 30 * time.Minute
	// maxConcurrentTransfers bounds daemon-wide streaming transfers.
	maxConcurrentTransfers = 4
	// recvReserve is the free-space floor a receive insists on beyond
	// the announced size, so a fill-the-disk sender cannot consume the
	// last bytes of the volume.
	recvReserve = 64 << 20
)

// Config configures a daemon. Dir is required and must have been created
// by Init (or contain a compatible identity, roster and me.json).
type Config struct {
	Dir  string
	Port uint16 // clowder protocol port; used by the tailcat transport
	// HealthAddr optionally serves HTTP container probes on / and
	// /healthz (e.g. ":8080"); empty disables the endpoint.
	HealthAddr string
	// Pprof serves net/http/pprof under /debug/pprof/ on the health
	// endpoint. Requires HealthAddr; off by default — profiling
	// endpoints leak internals and must be an explicit opt-in.
	Pprof bool
	// DERPMapURL overrides where tailcat fetches its DERP map from
	// (server, dials and pairing): a self-hosted map for air-gapped
	// clusters. Empty means tailcat's default.
	DERPMapURL string
	RetryEvery,
	PollEvery time.Duration
	Logf func(format string, args ...any)
}

func (c *Config) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Daemon runs the mesh for one cat. Create with New, run with Run.
type Daemon struct {
	cfg Config
	tr  Transport

	env *Env

	mu    sync.Mutex // guards meCat
	meCat roster.Cat

	ros      *roster.Roster
	spool    *store.Spool
	ob       *outbox
	stats    *statsKeeper
	prog     *progressKeeper
	receipts *receiptKeeper
	gauge    *transferGauge
	inbox    string

	syncSeq atomic.Int64 // round-robin cursor for peer sync

	// Pairing invite state (see pairing.go).
	pairMu   sync.Mutex
	pairSrv  *tailcat.Server
	pairLn   net.Listener
	pairDone chan struct{}

	// liveness records the unix time each cat's key was last seen on a
	// successful connection, either direction. Guarded by mu.
	liveness map[string]int64

	// blocked holds the keys of forgotten (and, later, distrusted)
	// cats. Tailcat's AllowedClients is add-only, so removal at the
	// transport layer is impossible; serveConn refuses these peers at
	// the protocol level instead. Guarded by mu, persisted in
	// blocked.json.
	blocked map[string]bool

	// inflightDelivery guards outbox entries against concurrent
	// delivery attempts (the retry ticker must not start a second
	// transfer while a big one is still streaming).
	inflightDelivery map[string]bool
	// inflightReceive refuses a second stream for a transfer ID we are
	// already receiving (retry/sweep/pull races).
	inflightReceive map[string]bool
	// resMu guards reserved: spool bytes promised to in-flight
	// deposits. The capacity check and the claim are one atomic
	// operation, so many deposits arriving at once are accepted
	// first-come-first-served, each later one only if it still fits.
	resMu    sync.Mutex
	reserved int64
}

// New loads a cat's state from cfg.Dir and wires it to a transport.
func New(cfg Config, tr Transport) (*Daemon, error) {
	if cfg.Dir == "" {
		return nil, errors.New("daemon: no config dir")
	}
	if cfg.RetryEvery <= 0 {
		cfg.RetryEvery = defaultRetryEvery
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = defaultPollEvery
	}
	env, err := Open(cfg.Dir)
	if err != nil {
		return nil, err
	}
	ros := roster.New()
	if err := ros.SetPath(filepath.Join(cfg.Dir, "roster.json")); err != nil {
		return nil, err
	}
	inbox := InboxDir(cfg.Dir)
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: creating inbox: %w", err)
	}
	d := &Daemon{
		cfg:              cfg,
		tr:               tr,
		env:              env,
		ros:              ros,
		spool:            store.New(filepath.Join(cfg.Dir, "spool"), 0),
		ob:               newOutbox(filepath.Join(cfg.Dir, "outbox")),
		stats:            newStatsKeeper(cfg.Dir),
		prog:             newProgressKeeper(),
		liveness:         map[string]int64{},
		blocked:          loadBlocked(cfg.Dir),
		receipts:         loadReceipts(cfg.Dir),
		gauge:            &transferGauge{max: maxConcurrentTransfers},
		inflightDelivery: map[string]bool{},
		inflightReceive:  map[string]bool{},
		inbox:            inbox,
	}
	d.meCat = roster.Cat{
		Name:      env.Me.Name,
		Key:       env.Identity.Public.ServerPublic.String(),
		ClientKey: env.ClientIdentity.Public().String(),
		Storer:    env.Me.Storer,
		Dropbox:   env.Me.Dropbox,
		Updated:   time.Now().Unix(),
	}
	return d, nil
}

// Me returns this cat's roster entry as currently known.
func (d *Daemon) Me() roster.Cat {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.meCat
}

// InboxDir returns the directory received files land in.
func (d *Daemon) InboxDir() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inbox
}

// SetInbox changes the directory received files land in and persists it,
// so it survives daemon restarts.
func (d *Daemon) SetInbox(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("daemon: resolving inbox path: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return fmt.Errorf("daemon: creating inbox: %w", err)
	}
	d.mu.Lock()
	d.inbox = abs
	d.mu.Unlock()
	return SetInboxAt(d.cfg.Dir, abs)
}

// Roster returns the cat's roster, for inspection (clow cats) and tests.
func (d *Daemon) Roster() *roster.Roster { return d.ros }

// Spool returns the storer spool, for inspection and tests.
func (d *Daemon) Spool() *store.Spool { return d.spool }

// Forget removes a cat from the roster and drops any pending outbox
// sends destined to it (local, manual operation; entries are never
// removed by propagation in v1).
func (d *Daemon) Forget(name string) (roster.Cat, bool) {
	c, ok := d.ros.RemoveName(name)
	if !ok {
		return c, false
	}
	// The allowlist cannot drop the cat's key (tailcat is add-only),
	// so block it at the protocol level.
	if err := d.blockCat(c); err != nil {
		d.cfg.logf("clowder: persisting blocklist after forgetting %s: %v", name, err)
	}
	for _, e := range d.ob.All() {
		if e.TargetKey == c.Key {
			if err := d.ob.Delete(e.ID); err != nil {
				d.cfg.logf("clowder: dropping outbox entry %s: %v", e.ID, err)
			}
		}
	}
	return c, true
}

// markSeen records that the cat with the given key was just seen on a
// successful connection.
func (d *Daemon) markSeen(key string) {
	if key == "" {
		return
	}
	d.mu.Lock()
	d.liveness[key] = time.Now().Unix()
	d.mu.Unlock()
}

// SeenAt returns when a cat's key was last seen, or 0 if never.
func (d *Daemon) SeenAt(key string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.liveness[key]
}

// livenessSnapshot copies the last-seen times for the status op.
func (d *Daemon) livenessSnapshot() map[string]int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]int64, len(d.liveness))
	for k, v := range d.liveness {
		out[k] = v
	}
	return out
}

// claimDelivery reports whether a delivery for the entry may start,
// claiming it if so. A second attempt for an already-streaming entry
// (from the retry ticker racing a slow transfer) returns false.
func (d *Daemon) claimDelivery(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflightDelivery[id] {
		return false
	}
	d.inflightDelivery[id] = true
	return true
}

func (d *Daemon) releaseDelivery(id string) {
	d.mu.Lock()
	delete(d.inflightDelivery, id)
	d.mu.Unlock()
}

// claimReceive reports whether a transfer ID may start being received,
// claiming it if so.
func (d *Daemon) claimReceive(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflightReceive[id] {
		return false
	}
	d.inflightReceive[id] = true
	return true
}

func (d *Daemon) releaseReceive(id string) {
	d.mu.Lock()
	delete(d.inflightReceive, id)
	d.mu.Unlock()
}

// SetStorer declares or retracts this cat's storer role and persists it.
func (d *Daemon) SetStorer(on bool, capacity int64) error {
	return d.setStorerMode(on, false, capacity)
}

// SetDropbox switches this cat to dropbox mode (a storer that only
// serves third parties) or back off entirely.
func (d *Daemon) SetDropbox(on bool, capacity int64) error {
	return d.setStorerMode(on, on, capacity)
}

// setStorerMode applies the storer/dropbox flags with the spool
// capacity in bytes and persists them. Enabling requires a capacity;
// disabling ignores it.
func (d *Daemon) setStorerMode(storer, dropbox bool, capacity int64) error {
	if storer && capacity <= 0 {
		return errors.New("daemon: enabling the storer role requires a capacity (e.g. --max 10G)")
	}
	d.mu.Lock()
	d.meCat.Storer = storer
	d.meCat.Dropbox = dropbox
	d.meCat.Capacity = capacity
	d.meCat.Updated = time.Now().Unix()
	d.mu.Unlock()
	me := d.env.Me
	me.Storer = storer
	me.Dropbox = dropbox
	me.Capacity = capacity
	return saveMe(d.cfg.Dir, me)
}

// Run listens, serves connections, retries the outbox, polls storers,
// syncs rosters, and serves the IPC socket, until ctx is canceled.
func (d *Daemon) Run(ctx context.Context) error {
	ln, err := d.tr.Listen(ctx)
	if err != nil {
		return fmt.Errorf("daemon: listening: %w", err)
	}
	d.mu.Lock()
	d.meCat.Addr = d.tr.MyAddr()
	me := d.meCat
	d.mu.Unlock()
	d.cfg.logf("clowder: cat %s listening at %s", me.Name, me.Addr)

	for _, c := range d.ros.All() {
		d.allowCat(c)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serveAccepted(conn)
		}
	}()

	ipcLn, err := listenIPC(filepath.Join(d.cfg.Dir, "clow.sock"))
	if err != nil {
		_ = ln.Close()
		_ = d.tr.Close()
		return fmt.Errorf("daemon: IPC socket: %w", err)
	}
	go func() {
		for {
			conn, err := ipcLn.Accept()
			if err != nil {
				return
			}
			go d.serveIPCConn(conn)
		}
	}()

	var healthLn net.Listener
	var healthSrv *http.Server
	if d.cfg.HealthAddr != "" {
		healthLn, healthSrv, err = listenHealth(d.cfg.HealthAddr, d.cfg.Pprof)
		if err != nil {
			_ = ln.Close()
			_ = ipcLn.Close()
			_ = d.tr.Close()
			return err
		}
		go func() { _ = healthSrv.Serve(healthLn) }()
		d.cfg.logf("clowder: health endpoint on %s", healthLn.Addr().String())
	} else if d.cfg.Pprof {
		_ = ln.Close()
		_ = ipcLn.Close()
		_ = d.tr.Close()
		return errors.New("daemon: Pprof requires the health endpoint (set HealthAddr)")
	}

	retry := time.NewTicker(d.cfg.RetryEvery)
	defer retry.Stop()
	poll := time.NewTicker(d.cfg.PollEvery)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = ln.Close()
			_ = ipcLn.Close()
			if healthSrv != nil {
				_ = healthSrv.Close()
			}
			_ = d.tr.Close()
			return nil
		case <-retry.C:
			go d.retryOutbox()
		case <-poll.C:
			// No periodic storer polling: the push sweep delivers
			// held files to online targets on its own, and `clow
			// fetch` remains as a manual pull. This keeps the
			// background traffic to one roster sync per tick plus
			// spool pushes only while files are held.
			go d.syncPeers(context.WithoutCancel(ctx))
			go d.sweepSpool(context.WithoutCancel(ctx))
		}
	}
}

// Send queues a file for asynchronous delivery to the named cat and
// returns the transfer ID. Delivery is attempted in the background:
// directly to the target first, then via any reachable storer, retrying
// on the daemon's ticker until one succeeds.
func (d *Daemon) Send(targetName, path string) (string, error) {
	if d.Me().Dropbox {
		return "", errors.New("dropbox cats cannot send files")
	}
	cat, ok := d.ros.Get(targetName)
	if !ok {
		return "", fmt.Errorf("unknown cat %q (known: add it first)", targetName)
	}
	if d.isBlockedKey(cat.Key) {
		return "", fmt.Errorf("distrusted cat %q (run: clow trust %s)", targetName, targetName)
	}
	if slices.Contains(d.ros.Duplicates(), targetName) {
		d.cfg.logf("clowder: %q is claimed by more than one cat; sending to the newest", targetName)
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("reading file: %w", err)
	}
	id := newID()
	e := Entry{
		ID:         id,
		TargetName: cat.Name,
		TargetKey:  cat.Key,
		SourcePath: path,
		FileName:   filepath.Base(path),
		AddedAt:    time.Now().Unix(),
	}
	if err := d.ob.Put(e); err != nil {
		return "", err
	}
	go d.deliver(e)
	return id, nil
}

// Poll asks every known storer for files held for this cat and fetches
// them. It runs periodically from Run and on demand over IPC.
func (d *Daemon) Poll(ctx context.Context) {
	if d.Me().Dropbox {
		return // a dropbox takes no deliveries for itself
	}
	for _, s := range d.ros.Storers() {
		if d.isBlockedKey(s.Key) {
			continue // never pull from a cat we distrust
		}
		if err := d.fetchFrom(ctx, s); err != nil {
			d.cfg.logf("clowder: fetching from storer %s: %v", s.Name, err)
		}
	}
}

// ---- serving ----

// serveConn runs the protocol on one accepted connection.
func (d *Daemon) serveConn(pc *protocol.Conn, authKey key.NodePublic, authed bool) {
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	m, err := pc.ReadMsg()
	if err != nil || m.Hello == nil {
		return
	}
	peer := m.Hello
	if d.isBlockedKey(peer.Key) || d.isBlockedKey(peer.ClientKey) {
		d.cfg.logf("clowder: refusing connection from blocked cat %s", peer.Name)
		return
	}
	if authed {
		claimed, err := parseKey(peer.ClientKey)
		if err != nil || claimed != authKey {
			d.cfg.logf("clowder: closing conn from %s: claimed client key %q does not match authenticated %s", peer.Name, peer.ClientKey, authKey)
			return
		}
	}
	if peer.Version > protocol.HelloVersion {
		// No negotiation: serve anyway, so an upgrade never partitions
		// the clowder — but say so, before the protocol ossifies into
		// silent incompatibility.
		d.cfg.logf("clowder: %s speaks protocol version %d, newer than ours (%d); serving anyway", peer.Name, peer.Version, protocol.HelloVersion)
	}
	d.markSeen(peer.Key)
	if err := pc.WriteMsg(&protocol.Message{Hello: d.helloMsg()}); err != nil {
		return
	}
	// The peer sends its roster, then we send ours.
	m, err = pc.ReadMsg()
	if err != nil || m.Roster == nil {
		return
	}
	d.mergeRemote(m.Roster.Cats)
	if err := pc.WriteMsg(&protocol.Message{Roster: d.rosterMsg()}); err != nil {
		return
	}

	for {
		_ = pc.SetDeadline(time.Now().Add(msgTimeout))
		m, err := pc.ReadMsg()
		if err != nil {
			return
		}
		switch {
		case m.Offer != nil:
			if !d.handleOffer(pc, peer, m.Offer) {
				return
			}
		case m.Roster != nil:
			// Post-handshake roster push (e.g. an address rotation
			// announcement). Merged; no reply, to avoid sync ping-pong.
			d.mergeRemote(m.Roster.Cats)
		case m.Pending != nil && m.Pending.Query:
			d.handlePending(pc, peer)
		case m.Fetch != nil:
			if !d.handleFetch(pc, peer, m.Fetch) {
				return
			}
		default:
			d.cfg.logf("clowder: unexpected %s message from %s", m.Kind(), peer.Name)
			return
		}
	}
}

// handleOffer receives either a direct delivery (target is us) or a
// storer deposit (target is a third cat). It reports whether the
// connection may continue.
func (d *Daemon) handleOffer(pc *protocol.Conn, from *protocol.Hello, o *protocol.Offer) bool {
	if o.Receipt && o.TargetKey == d.Me().Key {
		return d.receiveReceipt(pc, o)
	}
	if o.TargetKey == d.Me().Key {
		return d.receiveDirect(pc, from, o)
	}
	return d.receiveAsStorer(pc, o)
}

// receiveDirect decrypts a sealed stream into the inbox and acks
// delivery.
func (d *Daemon) receiveDirect(pc *protocol.Conn, from *protocol.Hello, o *protocol.Offer) bool {
	refuse := func(reason string) bool {
		return pc.Answer(o.ID, false, reason) == nil
	}
	if d.Me().Dropbox {
		// A dropbox relays files for others; it takes none for itself.
		return refuse("dropbox cat: no personal deliveries")
	}
	if d.isBlockedName(o.From) {
		// The connection itself is fine (this is a storer relaying),
		// but the sender named in the offer is distrusted.
		return refuse("sender is distrusted")
	}
	// The disk is not fillable by a sender: refuse when the announced
	// stream plus a reserve would not fit.
	if free, ok := freeSpace(d.InboxDir()); ok && free < uint64(o.Size)+recvReserve {
		return refuse(fmt.Sprintf("receiver is low on disk (%s free)", HumanBytes(int64(free))))
	}
	if !d.claimReceive(o.ID) {
		// A duplicate stream for a transfer already in flight (the
		// sender retried, or the sweep raced the pull): refuse it.
		return refuse("transfer already in progress")
	}
	defer d.releaseReceive(o.ID)
	if !d.gauge.tryStart() {
		return refuse("receiver busy, try again soon")
	}
	defer d.gauge.done()
	if err := pc.Answer(o.ID, true, ""); err != nil {
		return false
	}
	_ = pc.SetDeadline(time.Now().Add(streamTimeout))
	d.prog.start(Progress{
		ID:        o.ID,
		FileName:  o.FileName,
		Peer:      from.Name,
		Receiving: true,
		Total:     o.Size,
		Started:   time.Now().Unix(),
	})
	defer d.prog.end(o.ID)
	plainSize, err := d.saveIncoming(o, countingReader{k: d.prog, id: o.ID, r: io.LimitReader(pc.Reader(), o.Size)}, d.env.Identity.Private)
	if err != nil {
		d.cfg.logf("clowder: receiving %s from %s: %v", o.FileName, from.Name, err)
		return false
	}
	d.stats.add(func(s *Stats) { s.Received++; s.ReceivedBytes += plainSize })
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	if err := pc.Ack(o.ID, protocol.AckDelivered); err != nil {
		return false
	}
	d.cfg.logf("clowder: received %s (%s) from %s", o.FileName, HumanBytes(plainSize), from.Name)
	go d.sendReceipt(from.Name, o.ID, o.FileName)
	return true
}

// receiveAsStorer spools a sealed stream for an offline target. The
// stream stays opaque: the storer cannot decrypt it.
func (d *Daemon) receiveAsStorer(pc *protocol.Conn, o *protocol.Offer) bool {
	me := d.Me()
	if !me.Storer {
		return pc.Answer(o.ID, false, "not a storer") == nil
	}
	if me.Capacity <= 0 {
		return pc.Answer(o.ID, false, "storer has no capacity set") == nil
	}
	if !d.tryReserve(me.Capacity, o.Size) {
		reason := fmt.Sprintf("storer full (%s of %s held)", HumanBytes(d.heldBytes()), HumanBytes(me.Capacity))
		return pc.Answer(o.ID, false, reason) == nil
	}
	defer d.releaseReserve(o.Size)
	if err := pc.Answer(o.ID, true, ""); err != nil {
		return false
	}
	_ = pc.SetDeadline(time.Now().Add(streamTimeout))
	meta := store.Meta{
		ID:         o.ID,
		FileName:   o.FileName,
		Size:       o.Size,
		SHA256:     o.SHA256,
		From:       o.From,
		TargetKey:  o.TargetKey,
		TargetName: o.TargetName,
		Receipt:    o.Receipt,
	}
	if err := d.spool.Put(meta, io.LimitReader(pc.Reader(), o.Size)); err != nil {
		d.cfg.logf("clowder: spooling %s for %s: %v", o.FileName, o.TargetName, err)
		return false
	}
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	if err := pc.Ack(o.ID, protocol.AckStored); err != nil {
		return false
	}
	d.stats.add(func(s *Stats) { s.Spooled++ })
	d.cfg.logf("clowder: holding %s from %s for %s", o.FileName, o.From, o.TargetName)
	// The target may be online already: try to push right away
	// instead of waiting for the next sweep.
	go d.sweepSpoolFor(context.Background(), o.TargetKey)
	return true
}

// tryReserve atomically claims spool capacity for a deposit of the
// given size: the check and the claim happen under one lock, so
// concurrent deposits are accepted first-come-first-served and a
// deposit is only accepted if it still fits.
func (d *Daemon) tryReserve(capacity, size int64) bool {
	d.resMu.Lock()
	defer d.resMu.Unlock()
	if capacity <= 0 || d.spool.Usage()+d.reserved+size > capacity {
		return false
	}
	d.reserved += size
	return true
}

// releaseReserve gives a deposit's reservation back (stream ended,
// failed, or refused mid-flight).
func (d *Daemon) releaseReserve(size int64) {
	d.resMu.Lock()
	d.reserved -= size
	if d.reserved < 0 {
		d.reserved = 0
	}
	d.resMu.Unlock()
}

// heldBytes reports the spool usage plus reservations, for refusal
// messages.
func (d *Daemon) heldBytes() int64 {
	d.resMu.Lock()
	defer d.resMu.Unlock()
	return d.spool.Usage() + d.reserved
}

// handlePending answers a "what are you holding for me?" query from the
// peer identified by hello.
func (d *Daemon) handlePending(pc *protocol.Conn, hello *protocol.Hello) {
	files := d.spool.List(hello.Key)
	pf := make([]protocol.PendingFile, 0, len(files))
	for _, f := range files {
		pf = append(pf, protocol.PendingFile{
			ID:       f.ID,
			FileName: f.FileName,
			Size:     f.Size,
			From:     f.From,
			SHA256:   f.SHA256,
			StoredAt: f.StoredAt,
		})
	}
	if err := pc.WriteMsg(&protocol.Message{Pending: &protocol.Pending{Files: pf}}); err != nil {
		return
	}
}

// handleFetch replays one held sealed stream to its target and deletes it
// once delivery is acked. The requester (hello) must be the file's
// target. It reports whether the connection may continue.
func (d *Daemon) handleFetch(pc *protocol.Conn, hello *protocol.Hello, f *protocol.Fetch) bool {
	meta, r, err := d.spool.Open(f.ID)
	if err != nil {
		return false
	}
	defer r.Close()
	if meta.TargetKey != hello.Key {
		d.cfg.logf("clowder: refusing fetch of %s by %s (held for %s)", f.ID, hello.Name, meta.TargetName)
		return false
	}
	offer := &protocol.Offer{
		ID:         meta.ID,
		FileName:   meta.FileName,
		Size:       meta.Size,
		From:       meta.From,
		SHA256:     meta.SHA256,
		TargetKey:  meta.TargetKey,
		TargetName: meta.TargetName,
		Receipt:    meta.Receipt,
	}
	if err := pc.WriteMsg(&protocol.Message{Offer: offer}); err != nil {
		return false
	}
	m, err := pc.ReadMsg()
	if err != nil || m.Answer == nil || !m.Answer.OK {
		return m.Answer != nil
	}
	_ = pc.SetDeadline(time.Now().Add(streamTimeout))
	if _, err := io.CopyN(pc.Writer(), r, meta.Size); err != nil {
		d.cfg.logf("clowder: replaying %s: %v", meta.ID, err)
		return false
	}
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	m, err = pc.ReadMsg()
	if err != nil || m.Ack == nil || m.Ack.Kind != protocol.AckDelivered {
		return false
	}
	if err := d.spool.Delete(meta.ID); err != nil {
		d.cfg.logf("clowder: deleting delivered %s: %v", meta.ID, err)
	}
	d.stats.add(func(s *Stats) { s.Fetched++ })
	d.cfg.logf("clowder: delivered held %s to %s", meta.FileName, d.Me().Name)
	return true
}

// saveIncoming decrypts a sealed stream from src into the inbox under a
// unique name, verifying the announced plaintext digest. It returns the
// plaintext size.
func (d *Daemon) saveIncoming(o *protocol.Offer, src io.Reader, recipient key.NodePrivate) (int64, error) {
	inbox := d.InboxDir()
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		return 0, fmt.Errorf("creating inbox: %w", err)
	}
	name := inboxPath(inbox, o.FileName)
	ex := &exactReader{r: src}
	var gotSize int64
	if err := persist.WriteFunc(name, func(w io.Writer) error {
		var gotSha string
		var err error
		gotSize, gotSha, err = envelope.OpenStream(recipient, ex, w)
		if err != nil {
			return err
		}
		// The sealed stream must be exactly as long as announced: a
		// sender lying about the size (a modified client) gets the
		// connection killed and no delivery ack. Nothing lands in
		// the inbox unless every check passes.
		if ex.n != o.Size {
			return fmt.Errorf("protocol violation: sealed stream was %d bytes, %d announced", ex.n, o.Size)
		}
		if gotSha != o.SHA256 {
			return fmt.Errorf("digest mismatch: got %s, announced %s", gotSha, o.SHA256)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return gotSize, nil
}

// exactReader counts the bytes consumed from a stream.
type exactReader struct {
	r io.Reader
	n int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.n += int64(n)
	return n, err
}

// inboxPath picks a non-existing name for a received file, numbering
// collisions with a dash before the extension (nap-1.txt): a dot would
// read as an extension, which is especially confusing for files that
// never had one. Dotfiles keep their whole name as the stem.
func inboxPath(dir, name string) string {
	// Control characters are stripped before anything else: os.Stat
	// fails with EINVAL (not IsNotExist) on e.g. a NUL byte, which
	// would spin the collision loop below forever (found by
	// FuzzInboxPath).
	clean := strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, name)
	clean = filepath.Base(filepath.Clean(clean))
	if clean == "" || clean == "." || clean == ".." || clean == "/" {
		clean = "file"
	}
	stem, ext := clean, ""
	if !strings.HasPrefix(clean, ".") {
		if e := filepath.Ext(clean); e != "" {
			stem, ext = strings.TrimSuffix(clean, e), e
		}
	}
	p := filepath.Join(dir, clean)
	for i := 1; ; i++ {
		if _, err := os.Stat(p); err == nil {
			// taken: try the next number
		} else if os.IsNotExist(err) {
			return p
		} else {
			// Unverifiable (permissions, invalid name): fall through
			// and keep numbering rather than spinning forever on the
			// same broken name.
			_ = err
		}
		if i > 1<<16 {
			// Absurd collision count: pick something unique instead
			// of looping unboundedly.
			return filepath.Join(dir, fmt.Sprintf("clow-%d%s", time.Now().UnixNano(), ext))
		}
		p = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
	}
}

// transferGauge caps concurrent streaming transfers (sends and
// receives) across the whole daemon: claims are per-ID only, so
// without a global cap a flood of concurrent offers can pin every
// core and exhaust memory in sealing buffers.
type transferGauge struct {
	mu     sync.Mutex
	active int
	max    int
}

func (g *transferGauge) tryStart() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active >= g.max {
		return false
	}
	g.active++
	return true
}

func (g *transferGauge) done() {
	g.mu.Lock()
	g.active--
	g.mu.Unlock()
}

// ---- outbound ----

// retryOutbox attempts delivery of every pending entry. Each attempt
// has its own timeout, so no context is needed here.
func (d *Daemon) retryOutbox() {
	for _, e := range d.ob.All() {
		go d.deliver(e)
	}
}

// deliver tries the target directly, then each known storer, and removes
// the outbox entry on success.
func (d *Daemon) deliver(e Entry) {
	if !d.claimDelivery(e.ID) {
		return // already streaming this entry
	}
	defer d.releaseDelivery(e.ID)
	if !d.gauge.tryStart() {
		d.cfg.logf("clowder: %s to %s deferred: %d transfers already in flight", e.FileName, e.TargetName, maxConcurrentTransfers)
		return
	}
	defer d.gauge.done()

	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
	defer cancel()

	if d.isBlockedKey(e.TargetKey) {
		d.cfg.logf("clowder: %s to %s skipped: target is distrusted", e.FileName, e.TargetName)
		return
	}
	if cat, ok := d.ros.GetByKey(e.TargetKey); ok {
		if err := d.deliverDirect(ctx, cat, e); err == nil {
			_ = d.ob.Delete(e.ID)
			return
		} else {
			d.cfg.logf("clowder: direct to %s failed: %v", cat.Name, err)
		}
	}
	for _, s := range d.ros.Storers() {
		if s.Key == e.TargetKey || d.isBlockedKey(s.Key) {
			continue // never relay through a cat we distrust
		}
		if err := d.deliverViaStorer(ctx, s, e); err == nil {
			_ = d.ob.Delete(e.ID)
			return
		} else {
			d.cfg.logf("clowder: via storer %s failed: %v", s.Name, err)
		}
	}
	d.cfg.logf("clowder: %s to %s still pending", e.FileName, e.TargetName)
}

// deliverDirect streams a sealed file straight to its target.
func (d *Daemon) deliverDirect(ctx context.Context, cat roster.Cat, e Entry) error {
	return d.deliverStream(ctx, cat, e, cat.Key, cat.Name, protocol.AckDelivered)
}

// deliverViaStorer streams a sealed file to a storer for an offline
// target.
func (d *Daemon) deliverViaStorer(ctx context.Context, storer roster.Cat, e Entry) error {
	return d.deliverStream(ctx, storer, e, e.TargetKey, e.TargetName, protocol.AckStored)
}

// deliverStream performs one Offer/stream/Ack exchange toward peer,
// where wantAck is the ack that ends the transfer successfully.
func (d *Daemon) deliverStream(ctx context.Context, peer roster.Cat, e Entry, targetKey, targetName, wantAck string) error {
	digest, size, err := fileDigest(e.SourcePath)
	if err != nil {
		return err
	}
	src, err := os.Open(e.SourcePath)
	if err != nil {
		return err
	}
	defer src.Close()

	o := &protocol.Offer{
		ID:         e.ID,
		FileName:   e.FileName,
		Size:       envelope.SealedSize(size),
		From:       d.Me().Name,
		SHA256:     digest,
		TargetKey:  targetKey,
		TargetName: targetName,
	}
	if err := d.sendSealed(ctx, peer, o, digest, src, wantAck, true); err != nil {
		return err
	}
	// Stats are plaintext bytes: what the user actually sent, not the
	// sealed stream size.
	d.stats.add(func(s *Stats) { s.Sent++; s.SentBytes += size })
	return nil
}

// sendSealed performs one Offer/Answer/sealed-stream/Ack exchange
// toward a peer, sealing to targetKey and streaming src. Shared by
// file deliveries (outbox entries) and delivery receipts; track enables
// in-status progress for the former only.
func (d *Daemon) sendSealed(ctx context.Context, peer roster.Cat, o *protocol.Offer, digest string, src io.Reader, wantAck string, track bool) error {
	pc, err := d.connect(ctx, peer)
	if err != nil {
		return err
	}
	defer func() { _ = pc.Close() }()

	if err := pc.WriteMsg(&protocol.Message{Offer: o}); err != nil {
		return err
	}
	m, err := pc.ReadMsg()
	if err != nil {
		return err
	}
	if m.Answer == nil || !m.Answer.OK {
		reason := "peer refused"
		if m.Answer != nil && m.Answer.Reason != "" {
			reason = m.Answer.Reason
		}
		return errors.New(reason)
	}
	_ = pc.SetDeadline(time.Now().Add(streamTimeout))
	targetPub, err := parseKey(o.TargetKey)
	if err != nil {
		return err
	}
	var w = pc.Writer()
	if track {
		d.prog.start(Progress{
			ID:       o.ID,
			FileName: o.FileName,
			Peer:     o.TargetName,
			Total:    o.Size,
			Started:  time.Now().Unix(),
		})
		defer d.prog.end(o.ID)
		w = countingWriter{k: d.prog, id: o.ID, w: pc.Writer()}
	}
	_, sealedSha, err := envelope.SealStream(d.env.Identity.Private, targetPub, w, src)
	if err != nil {
		return err
	}
	if sealedSha != digest {
		return errors.New("payload changed during send")
	}
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	m, err = pc.ReadMsg()
	if err != nil {
		return err
	}
	if m.Ack == nil || m.Ack.Kind != wantAck {
		return fmt.Errorf("wanted %s ack, got %v", wantAck, m.Ack)
	}
	d.cfg.logf("clowder: sent %s (%s) to %s", o.FileName, HumanBytes(o.Size), o.TargetName)
	return nil
}

// fetchFrom asks one storer what it holds for us and fetches everything.
func (d *Daemon) fetchFrom(ctx context.Context, s roster.Cat) error {
	pc, err := d.connect(ctx, s)
	if err != nil {
		return err
	}
	defer func() { _ = pc.Close() }()

	if err := pc.WriteMsg(&protocol.Message{Pending: &protocol.Pending{Query: true}}); err != nil {
		return err
	}
	m, err := pc.ReadMsg()
	if err != nil {
		return err
	}
	if m.Pending == nil {
		return errors.New("storer did not answer pending query")
	}
	for _, f := range m.Pending.Files {
		if err := d.fetchOne(pc, f); err != nil {
			d.cfg.logf("clowder: fetching %s from %s: %v", f.FileName, s.Name, err)
			return err
		}
	}
	return nil
}

// fetchOne fetches a single held file over an established storer conn.
func (d *Daemon) fetchOne(pc *protocol.Conn, f protocol.PendingFile) error {
	if err := pc.WriteMsg(&protocol.Message{Fetch: &protocol.Fetch{ID: f.ID}}); err != nil {
		return err
	}
	m, err := pc.ReadMsg()
	if err != nil {
		return err
	}
	if m.Offer == nil {
		return errors.New("storer did not offer the file")
	}
	o := m.Offer
	if d.isBlockedName(o.From) {
		if err := pc.Answer(o.ID, false, "sender is distrusted"); err != nil {
			return err
		}
		return nil
	}
	// A storer may hand us a delivery receipt rather than a file
	// (fetch of the pending list includes receipts held for us).
	if o.Receipt {
		if !d.receiveReceipt(pc, o) {
			return errors.New("receipt exchange failed")
		}
		return nil
	}
	if err := pc.Answer(o.ID, true, ""); err != nil {
		return err
	}
	_ = pc.SetDeadline(time.Now().Add(streamTimeout))
	if !d.claimReceive(o.ID) {
		return errors.New("transfer already in progress")
	}
	defer d.releaseReceive(o.ID)
	d.prog.start(Progress{
		ID:        o.ID,
		FileName:  o.FileName,
		Peer:      o.From,
		Receiving: true,
		Total:     o.Size,
		Started:   time.Now().Unix(),
	})
	defer d.prog.end(o.ID)
	if _, err := d.saveIncoming(o, countingReader{k: d.prog, id: o.ID, r: io.LimitReader(pc.Reader(), o.Size)}, d.env.Identity.Private); err != nil {
		return err
	}
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	if err := pc.Ack(o.ID, protocol.AckDelivered); err != nil {
		return err
	}
	d.cfg.logf("clowder: fetched %s from storer", o.FileName)
	go d.sendReceipt(o.From, o.ID, o.FileName)
	return nil
}

// syncPeers connects to one peer (round-robin) purely to exchange
// rosters, so membership changes propagate without waiting for sends.
func (d *Daemon) syncPeers(ctx context.Context) {
	all := d.ros.All()
	if len(all) == 0 {
		return
	}
	cat := all[int(d.syncSeq.Add(1))%len(all)%len(all)]
	pc, err := d.connect(ctx, cat)
	if err != nil {
		return
	}
	_ = pc.Close()
}

// ---- handshake helpers ----

func (d *Daemon) helloMsg() *protocol.Hello {
	me := d.Me()
	return &protocol.Hello{
		Name:      me.Name,
		Key:       me.Key,
		ClientKey: me.ClientKey,
		Addr:      me.Addr,
		Storer:    me.Storer,
		Dropbox:   me.Dropbox,
		Version:   protocol.HelloVersion,
	}
}

func (d *Daemon) rosterMsg() *protocol.RosterSync {
	cats := append([]roster.Cat{d.Me()}, d.ros.All()...)
	return &protocol.RosterSync{Cats: cats}
}

// connect dials a peer and performs the hello and roster exchange.
func (d *Daemon) connect(ctx context.Context, cat roster.Cat) (*protocol.Conn, error) {
	conn, err := d.tr.Dial(ctx, cat.Addr)
	if err != nil {
		return nil, err
	}
	pc := protocol.NewConn(conn)
	if err := d.handshakeClient(pc); err != nil {
		_ = pc.Close()
		return nil, err
	}
	d.markSeen(cat.Key)
	return pc, nil
}

// handshakeClient sends our hello and roster and reads the peer's.
func (d *Daemon) handshakeClient(pc *protocol.Conn) error {
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	if err := pc.WriteMsg(&protocol.Message{Hello: d.helloMsg()}); err != nil {
		return err
	}
	m, err := pc.ReadMsg()
	if err != nil {
		return err
	}
	if m.Hello == nil {
		return errors.New("peer sent no hello")
	}
	if err := pc.WriteMsg(&protocol.Message{Roster: d.rosterMsg()}); err != nil {
		return err
	}
	m, err = pc.ReadMsg()
	if err != nil {
		return err
	}
	if m.Roster == nil {
		return errors.New("peer sent no roster")
	}
	d.mergeRemote(m.Roster.Cats)
	return nil
}

// mergeRemote merges incoming roster cats (skipping our own entry) and
// allows any new keys to connect.
func (d *Daemon) mergeRemote(cats []roster.Cat) {
	// Capture duplicates before the merge: parallel invites from
	// different inviters can both claim a name (the pairing check only
	// sees the inviter's roster), and LWW is per-key so both entries
	// persist everywhere. Surface the collision the moment it lands.
	before := d.ros.Duplicates()
	me := d.Me()
	filtered := cats[:0:0]
	for _, c := range cats {
		if c.Key == "" || c.Key == me.Key {
			continue
		}
		filtered = append(filtered, c)
	}
	changed, err := d.ros.Merge(filtered)
	if err != nil {
		d.cfg.logf("clowder: saving roster after sync: %v", err)
	}
	for _, c := range changed {
		d.allowCat(c)
		d.cfg.logf("clowder: roster learned %s (%s)", c.Name, c.Key)
	}
	for _, name := range d.ros.Duplicates() {
		if !slices.Contains(before, name) {
			d.cfg.logf("clowder: name %q is now claimed by more than one cat; name lookups pick the newest — one of them should re-init with a fresh name", name)
		}
	}
}

func (d *Daemon) allowCat(c roster.Cat) {
	// Peers dial us with their client key, not their identity key.
	allow := c.ClientKey
	if allow == "" {
		allow = c.Key
	}
	k, err := parseKey(allow)
	if err != nil {
		d.cfg.logf("clowder: bad key for %s: %v", c.Name, err)
		return
	}
	d.tr.Allow(k)
}

// ---- misc ----

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("daemon: generating id: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// fileDigest returns the hex SHA-256 and size of a file, read
// streaming.
func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
