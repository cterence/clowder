// Package daemon is the clowder mesh runtime: identity, protocol serving,
// roster sync, direct/storer delivery, and the clow IPC socket.
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

	"github.com/cterence/clowder/envelope"
	"github.com/cterence/clowder/persist"
	"github.com/cterence/clowder/protocol"
	"github.com/cterence/clowder/roster"
	"github.com/cterence/clowder/store"
)

const DefaultPort = 2569

const (
	defaultRetryEvery = 30 * time.Second
	defaultPollEvery  = 60 * time.Second
	// A cat with the app open should not wait a poll tick for a held
	// file; the sweep is cheap when the spool is empty.
	defaultSpoolSweepEvery = 5 * time.Second

	msgTimeout             = 2 * time.Minute
	streamTimeout          = 30 * time.Minute
	maxConcurrentTransfers = 4
	// Concurrent serves one authenticated peer may hold. Legit peak: all
	// four transfer slots plus a sync, a leave and headroom.
	maxPeerConns = 8
	// Post-handshake roster pushes one connection may run; the honest
	// rate is one or two (leave re-broadcast, pairing fan-out).
	maxRosterMerges = 10
	// A ping is a cold dial away from netcheck + relay attach (~3s on
	// the real mesh); bound it so the CLI answer comes in seconds.
	pingTimeout = 15 * time.Second
	// Free-space floor beyond the announced size, so a sender cannot fill the disk.
	recvReserve = 64 << 20
)

// streamIdle bounds a receive by inactivity, not total duration: a
// sender that goes silent mid-stream (cancelled, killed, partitioned
// without closing) must not hold the receiver's partial file for the
// whole stream timeout. Overridable in tests.
var streamIdle = 2 * time.Minute

// idleReader pushes the connection deadline out on every read: a live
// stream is never cut, a silent one dies at streamIdle and its partial
// file is wiped.
type idleReader struct {
	pc *protocol.Conn
	r  io.Reader
}

func (i idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		_ = i.pc.SetDeadline(time.Now().Add(streamIdle))
	}
	return n, err
}

// Dir is required; created by Init, or holding compatible state.
type Config struct {
	Dir string
	// HTTP probe endpoint ("/", "/healthz"); empty disables it.
	HealthAddr string
	// pprof under /debug/pprof/; requires HealthAddr. Off by default:
	// profiling leaks internals.
	Pprof bool
	// Overrides where tailcat fetches its DERP map (server, dials, pairing).
	DERPMapURL string
	RetryEvery,
	PollEvery,
	// SpoolSweepEvery is the storer push cadence, independent of the
	// roster poll: a cat coming online gets its held files within it.
	SpoolSweepEvery time.Duration
	Logf func(format string, args ...any)
}

func (c *Config) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

type Daemon struct {
	cfg Config
	tr  Transport

	env *Env

	mu    sync.Mutex // guards meCat
	meCat roster.Cat

	ros   *roster.Roster
	spool *store.Spool
	ob    *outbox
	stats *statsKeeper
	prog  *progressKeeper
	slots chan struct{}
	inbox string

	// Background work Run drains before returning: transfers and relays are
	// uncancelable by design, bounded by the stream deadlines.
	bg      sync.WaitGroup
	connsMu sync.Mutex
	// Accepted connections still being served; Run closes them at drain
	// instead of waiting out a peer's msg deadline.
	liveConns map[*protocol.Conn]struct{}

	// Concurrent serves per authenticated peer key, capped at
	// maxPeerConns so one cat cannot pile unbounded work onto us.
	peerConnsMu sync.Mutex
	peerConns   map[string]int

	pairMu   sync.Mutex
	pairSrv  *tailcat.Server
	pairLn   net.Listener
	pairDone chan struct{}

	// Unix time each key was last seen on a successful connection, either direction.
	liveness map[string]int64
	// Bounded transfer-outcome history for the status op.
	settled      map[string]string
	settledOrder []string

	// Set by Leave: refuse roster syncs (peers would repopulate the clowder
	// we left). A new pairing clears it.
	left bool
	// rosterMu serializes roster merges against the leave wipe: a sync
	// that checked left before the leave must not land its merge after
	// the wipe.
	rosterMu sync.Mutex
	// Newest leave time processed per leaver, so each leave is handled once.
	leaveSeen map[string]int64

	// Keys of forgotten cats: tailcat's AllowedClients is add-only,
	// so these are refused at the protocol level. Persisted in blocked.json.
	blocked map[string]bool

	// Step logs: long ops (join, leave) stream their progress to the
	// connected CLI. One step stream at a time, carried by the newest
	// IPC connection — decided behavior: every connection still gets
	// its own final response, so concurrent long ops lose only their
	// intermediate progress lines, nothing more.
	stepMu   sync.Mutex
	stepEmit func(string)

	// In-flight transfer claims: refuse a second concurrent attempt per ID
	// (retry/sweep races).
	deliveryClaims *claimSet
	sweeping       atomic.Bool // one spool sweep at a time
	holdProbing    atomic.Bool // one hold-probe pass at a time
	receiveClaims  *claimSet
	// Pending delivery receipts (#8), keyed by transfer ID, pushed to
	// the sender on each sync round until it confirms.
	receiptsMu sync.Mutex
	receipts   map[string]receiptOut
	// Guards reserved: the capacity check and claim are one atomic operation.
	resMu    sync.Mutex
	reserved int64
}

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
	if cfg.SpoolSweepEvery <= 0 {
		cfg.SpoolSweepEvery = defaultSpoolSweepEvery
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
		cfg:            cfg,
		tr:             tr,
		env:            env,
		ros:            ros,
		spool:          store.New(filepath.Join(cfg.Dir, "spool"), 0),
		ob:             newOutbox(filepath.Join(cfg.Dir, "outbox")),
		stats:          newStatsKeeper(cfg.Dir),
		prog:           newProgressKeeper(),
		liveness:       map[string]int64{},
		leaveSeen:      map[string]int64{},
		blocked:        loadBlocked(cfg.Dir),
		peerConns:      map[string]int{},
		slots:          make(chan struct{}, maxConcurrentTransfers),
		deliveryClaims: newClaimSet(),
		receiveClaims:  newClaimSet(),
		inbox:          inbox,
	}
	d.meCat = roster.SignCat(env.SignPriv, roster.Cat{
		Name:    env.Me.Name,
		Key:     env.Identity.Public.ServerPublic.String(),
		DialKey: env.DialIdentity.Public().String(),
		Storer:  env.Me.Storer,
		Dropbox: env.Me.Dropbox,
		Updated: time.Now().Unix(),
	})
	return d, nil
}

func (d *Daemon) Me() roster.Cat {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.meCat
}

func (d *Daemon) InboxDir() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inbox
}

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

func (d *Daemon) Roster() *roster.Roster { return d.ros }

func (d *Daemon) Spool() *store.Spool { return d.spool }

// Forget removes the cat and its pending sends (local only; entries are
// never removed by propagation). who is a node key, the short key prefix
// `clow status` displays, or a name — when two cats claim one name, only
// the key is unambiguous.
func (d *Daemon) Forget(who string) (roster.Cat, bool) {
	c, ok := d.resolveCat(who)
	if !ok {
		return roster.Cat{}, false
	}
	c, ok = d.ros.RemoveKey(c.Key)
	if !ok {
		return c, false
	}
	if err := d.blockCat(c); err != nil {
		d.cfg.logf("clowder: persisting blocklist after forgetting %s: %v", c.Name, err)
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

// resolveCat finds a cat by name, full key, or the short key prefix
// status displays — the handle forget and ping accept.
func (d *Daemon) resolveCat(who string) (roster.Cat, bool) {
	c, ok := d.ros.GetByKey(who)
	if !ok {
		c, ok = d.ros.Get(who)
	}
	if !ok {
		c, ok = d.ros.GetByPrefix(who)
	}
	return c, ok
}

// Ping proves a cat is reachable with the same authenticated handshake
// a sync uses — a real round trip, so liveness is marked both ways —
// and reports how long it took and whether the path is direct or via
// a DERP relay.
func (d *Daemon) Ping(ctx context.Context, who string) (string, error) {
	c, ok := d.resolveCat(who)
	if !ok {
		return "", fmt.Errorf("no cat named %s", who)
	}
	start := time.Now()
	pc, err := d.connect(ctx, c, pingTimeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = pc.Close() }()
	// The handshake's dial probe triggers direct-path discovery, so
	// classifying after it reports the path the next dial would take.
	// A probe that fails anyway is not a ping failure: the handshake
	// already proved the cat online.
	path, _ := d.tr.Probe(ctx, c.Addr)
	msg := fmt.Sprintf("%s online, round trip %s", c.Name, time.Since(start).Round(time.Millisecond))
	if path != "" {
		msg += ", " + path
	}
	return msg, nil
}

// Cancel drops pending sends: one by transfer ID, or all when id is
// empty. An in-flight attempt is aborted through its claim.
func (d *Daemon) Cancel(id string) (int, error) {
	ids := []string{}
	if id != "" {
		ids = append(ids, id)
	} else {
		for _, e := range d.ob.All() {
			ids = append(ids, e.ID)
		}
	}
	n := 0
	for _, x := range ids {
		d.deliveryClaims.cancel(x)
		if err := d.ob.Delete(x); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// livenessMax bounds the liveness map: honest rosters stay far below
// it, and whatever a malicious peer claims cannot grow it without end.
const livenessMax = 4096

func (d *Daemon) markSeen(key string) {
	if key == "" {
		return
	}
	d.mu.Lock()
	d.liveness[key] = time.Now().Unix()
	if len(d.liveness) > livenessMax {
		oldestKey, oldest := "", int64(1<<62)
		for k, v := range d.liveness {
			if v < oldest {
				oldestKey, oldest = k, v
			}
		}
		delete(d.liveness, oldestKey)
	}
	d.mu.Unlock()
}

// settle records how a transfer ended, bounded to settledMax entries:
// a watcher that misses the window (daemon restart) gets no answer and
// falls back to hedging.
const settledMax = 100

func (d *Daemon) settle(id, outcome string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.settled == nil {
		d.settled = map[string]string{}
	}
	if _, ok := d.settled[id]; !ok {
		d.settledOrder = append(d.settledOrder, id)
	}
	d.settled[id] = outcome
	if len(d.settledOrder) > settledMax {
		delete(d.settled, d.settledOrder[0])
		d.settledOrder = d.settledOrder[1:]
	}
}

func (d *Daemon) settledSnapshot() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.settled) == 0 {
		return nil
	}
	out := make(map[string]string, len(d.settled))
	for k, v := range d.settled {
		out[k] = v
	}
	return out
}

func (d *Daemon) blockedSnapshot() map[string]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.blocked) == 0 {
		return nil
	}
	out := make(map[string]bool, len(d.blocked))
	for k := range d.blocked {
		out[k] = true
	}
	return out
}

func (d *Daemon) SeenAt(key string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.liveness[key]
}

func (d *Daemon) livenessSnapshot() map[string]int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]int64, len(d.liveness))
	for k, v := range d.liveness {
		out[k] = v
	}
	return out
}

// In-flight claim set: a second claim for the same ID is refused, and
// cancel() aborts a live attempt through its context.
type claimSet struct {
	mu sync.Mutex
	m  map[string]context.CancelFunc
}

func newClaimSet() *claimSet { return &claimSet{m: map[string]context.CancelFunc{}} }

func (c *claimSet) claim(id string) (context.Context, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[id]; ok {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.m[id] = cancel
	return ctx, true
}

func (c *claimSet) release(id string) {
	c.mu.Lock()
	if f := c.m[id]; f != nil {
		f()
		delete(c.m, id)
	}
	c.mu.Unlock()
}

func (c *claimSet) cancel(id string) {
	c.mu.Lock()
	if f := c.m[id]; f != nil {
		f()
	}
	c.mu.Unlock()
}

func (d *Daemon) SetStorer(on bool, capacity int64) error {
	return d.setStorerMode(on, false, capacity)
}

func (d *Daemon) SetDropbox(on bool, capacity int64) error {
	return d.setStorerMode(on, on, capacity)
}

func (d *Daemon) setStorerMode(storer, dropbox bool, capacity int64) error {
	if storer && capacity <= 0 {
		return errors.New("daemon: enabling the storer role requires a capacity (e.g. --max 10G)")
	}
	d.mu.Lock()
	d.meCat.Storer = storer
	d.meCat.Dropbox = dropbox
	d.meCat.Capacity = capacity
	d.meCat.Updated = time.Now().Unix()
	d.meCat = roster.SignCat(d.env.SignPriv, d.meCat)
	d.mu.Unlock()
	me := d.env.Me
	me.Storer = storer
	me.Dropbox = dropbox
	me.Capacity = capacity
	return saveMe(d.cfg.Dir, me)
}

func (d *Daemon) Run(ctx context.Context) error {
	// One daemon per config dir: two engines on one node key wedge the tunnel.
	unlock, err := lockDir(d.cfg.Dir)
	if err != nil {
		return err
	}
	defer unlock()
	// Nothing is in flight under the lock: sweep atomic-write
	// leftovers from a previous daemon killed mid-receive.
	tmps, _ := filepath.Glob(filepath.Join(d.InboxDir(), ".tmp-*"))
	for _, tmp := range tmps {
		_ = os.Remove(tmp)
	}
	ln, err := d.tr.Listen(ctx)
	if err != nil {
		return fmt.Errorf("daemon: listening: %w", err)
	}
	d.mu.Lock()
	d.meCat.Addr = d.tr.MyAddr()
	d.meCat = roster.SignCat(d.env.SignPriv, d.meCat)
	me := d.meCat
	d.mu.Unlock()
	d.cfg.logf("clowder: cat %s listening at %s", me.Name, me.Addr)

	for _, c := range d.ros.All() {
		d.allowCat(c)
	}

	d.goBg(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			d.goBg(func() { d.serveAccepted(conn) })
		}
	})

	ipcLn, err := listenIPC(filepath.Join(d.cfg.Dir, "clow.sock"))
	if err != nil {
		_ = ln.Close()
		_ = d.tr.Close()
		return fmt.Errorf("daemon: IPC socket: %w", err)
	}
	d.goBg(func() {
		for {
			conn, err := ipcLn.Accept()
			if err != nil {
				return
			}
			d.goBg(func() { d.serveIPCConn(conn) })
		}
	})

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
		d.goBg(func() { _ = healthSrv.Serve(healthLn) })
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
	spool := time.NewTicker(d.cfg.SpoolSweepEvery)
	defer spool.Stop()

	d.goBg(func() { d.syncPeers(context.WithoutCancel(ctx)) })

	for {
		select {
		case <-ctx.Done():
			_ = ln.Close()
			_ = ipcLn.Close()
			if healthSrv != nil {
				_ = healthSrv.Close()
			}
			_ = d.tr.Close()
			// Drain: nothing new can arrive; closing live conns bounds the wait
			// locally, not by a peer's msg deadline.
			d.closeLiveConns()
			d.bg.Wait()
			return nil
		case <-retry.C:
			d.goBg(d.retryOutbox)
		case <-poll.C:
			d.goBg(func() { d.syncPeers(context.WithoutCancel(ctx)) })
			d.goBg(func() { d.probeHeld(context.WithoutCancel(ctx)) })
		case <-spool.C:
			// One sweep at a time: a target's dialing timeout must
			// not stack attempts.
			d.goBg(func() {
				if d.sweeping.CompareAndSwap(false, true) {
					defer d.sweeping.Store(false)
					d.sweepSpool(context.WithoutCancel(ctx))
				}
			})
		}
	}
}

// Queues a file for background delivery: direct to the target first, then
// via any reachable storer, retried on the ticker until one succeeds.
func (d *Daemon) Send(targetName, path string) (string, error) {
	return d.queueSend(targetName, path, false)
}

// SendVia queues a file for delivery straight through a storer, skipping
// the direct attempt (and its dial timeout) entirely: `clow send --storer`.
func (d *Daemon) SendVia(targetName, path string) (string, error) {
	return d.queueSend(targetName, path, true)
}

func (d *Daemon) queueSend(targetName, path string, viaStorer bool) (string, error) {
	if d.Me().Dropbox {
		return "", errors.New("dropbox cats cannot send files")
	}
	cat, ok := d.ros.Get(targetName)
	if !ok {
		return "", fmt.Errorf("unknown cat %q", targetName)
	}
	if d.isBlockedKey(cat.Key) {
		return "", fmt.Errorf("cat %q is on the local blocklist (forgotten; re-pair to bring it back)", targetName)
	}
	if slices.Contains(d.ros.Duplicates(), targetName) {
		// A name claimed by two keys is not a safe send target:
		// LWW picks the newest silently. Refuse; one of the two cats
		// should re-init with a fresh name and re-pair.
		return "", fmt.Errorf("cat name %q is claimed by more than one cat; one of them should re-init with a fresh name", targetName)
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
		ViaStorer:  viaStorer,
	}
	if err := d.ob.Put(e); err != nil {
		return "", err
	}
	d.goBg(func() { d.deliver(e) })
	return id, nil
}

// setStepLog installs (or with nil removes) the sink for a long op's
// progress lines, set by the IPC connection currently being served.
func (d *Daemon) setStepLog(f func(string)) {
	d.stepMu.Lock()
	d.stepEmit = f
	d.stepMu.Unlock()
}

// stepf reports one step of a long op to the CLI, when one is watching.
func (d *Daemon) stepf(format string, args ...any) {
	d.stepMu.Lock()
	f := d.stepEmit
	d.stepMu.Unlock()
	if f != nil {
		f(fmt.Sprintf(format, args...))
	}
}

func (d *Daemon) goBg(f func()) {
	d.bg.Add(1)
	go func() {
		defer d.bg.Done()
		f()
	}()
}

func (d *Daemon) trackConn(pc *protocol.Conn) func() {
	d.connsMu.Lock()
	if d.liveConns == nil {
		d.liveConns = map[*protocol.Conn]struct{}{}
	}
	d.liveConns[pc] = struct{}{}
	d.connsMu.Unlock()
	return func() {
		d.connsMu.Lock()
		delete(d.liveConns, pc)
		d.connsMu.Unlock()
	}
}

func (d *Daemon) closeLiveConns() {
	d.connsMu.Lock()
	conns := make([]*protocol.Conn, 0, len(d.liveConns))
	for pc := range d.liveConns {
		conns = append(conns, pc)
	}
	// Clear first: closing runs deferred unregisters, which re-lock connsMu.
	d.liveConns = nil
	d.connsMu.Unlock()
	for _, pc := range conns {
		_ = pc.Close()
	}
}

// ---- serving ----

func (d *Daemon) serveConn(pc *protocol.Conn, authKey key.NodePublic, authed bool) {
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	m, err := pc.ReadMsg()
	if err != nil || m.Hello == nil {
		return
	}
	peer := m.Hello
	if d.isBlockedKey(peer.Key) || d.isBlockedKey(peer.DialKey) {
		d.cfg.logf("clowder: refusing connection from blocked cat %s", peer.Name)
		return
	}
	if authed {
		claimed, err := parseKey(peer.DialKey)
		if err != nil || claimed != authKey {
			d.cfg.logf("clowder: closing conn from %s: claimed dial key %q does not match authenticated %s", peer.Name, peer.DialKey, authKey)
			return
		}
	}
	if peer.Version > protocol.HelloVersion {
		// Serve newer peers anyway: an upgrade must never partition the clowder.
		d.cfg.logf("clowder: %s speaks protocol version %d, newer than ours (%d); serving anyway", peer.Name, peer.Version, protocol.HelloVersion)
	}
	if !authed || d.helloIdentityKnown(peer) {
		d.markSeen(peer.Key)
	}
	if err := pc.WriteMsg(&protocol.Message{Hello: d.helloMsg()}); err != nil {
		return
	}
	m, err = pc.ReadMsg()
	if err != nil || m.Roster == nil {
		return
	}
	d.mergeRemote(m.Roster)
	// The authenticated peer's own Hello pins its sign key and takes
	// its live entry verbatim (#12): a sync-carried pin for a cat we
	// never talked to is provisional.
	d.acceptSelfEntry(peer, m.Roster)
	if err := pc.WriteMsg(&protocol.Message{Roster: d.rosterReplyFor(peer)}); err != nil {
		return
	}

	merges := 0
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
			// Post-handshake roster push (e.g. a rotation). No reply: avoid
			// sync ping-pong. A churner may not run unbounded merges —
			// each is a full signature verify pass plus a roster write.
			if merges >= maxRosterMerges {
				d.cfg.logf("clowder: cutting %s: more than %d roster pushes on one connection", peer.Name, maxRosterMerges)
				return
			}
			merges++
			d.mergeRemote(m.Roster)
		case m.Ack != nil && m.Ack.Kind == protocol.AckHolding:
			// Hold probe: prove the spool still has the transfer.
			if err := pc.Answer(m.Ack.ID, d.spool.Has(m.Ack.ID), "not held"); err != nil {
				return
			}
		case m.Leave != nil:
			// Signed forget-me: apply and re-broadcast; the connection continues.
			d.handleLeave(m.Leave)
		case m.Receipt != nil:
			// Signed delivery proof (#8): clears the storer-held entry.
			if !d.handleReceipt(pc, m.Receipt) {
				return
			}
		default:
			d.cfg.logf("clowder: unexpected %s message from %s", m.Kind(), peer.Name)
			return
		}
	}
}

// handleOffer receives a direct delivery or a storer deposit; the return
// value says whether the connection may continue.
func (d *Daemon) handleOffer(pc *protocol.Conn, from *protocol.Hello, o *protocol.Offer) bool {
	if !validID(o.ID) {
		// IDs name spool files; anything but our own 32-hex format —
		// traversal sequences included — is refused.
		return pc.Answer(o.ID, false, "invalid transfer ID") == nil
	}
	if o.TargetKey == d.Me().Key {
		return d.receiveDirect(pc, from, o)
	}
	return d.receiveAsStorer(pc, o)
}

func (d *Daemon) receiveDirect(pc *protocol.Conn, from *protocol.Hello, o *protocol.Offer) bool {
	refuse := func(reason string) bool {
		return pc.Answer(o.ID, false, reason) == nil
	}
	if d.Me().Dropbox {
		return refuse("dropbox cat: no personal deliveries")
	}
	if d.isBlockedSender(o) {
		// The relaying storer is fine; the sender is blocked.
		return refuse("sender is blocked")
	}
	// End-to-end sender authentication (#7): the offer must be signed
	// by the sender's pinned sign key — From is display, not proof.
	sender, ok := d.ros.GetByKey(o.FromKey)
	if !ok {
		return refuse("unknown sender")
	}
	if !protocol.VerifyOffer(o, sender.SignKey) {
		return refuse("offer signature does not verify")
	}
	// Refuse when the announced stream plus a reserve would not fit.
	if free, ok := freeSpace(d.InboxDir()); ok && free < uint64(o.Size)+recvReserve {
		return refuse(fmt.Sprintf("receiver is low on disk (%s free)", HumanBytes(int64(free))))
	}
	if _, ok := d.receiveClaims.claim(o.ID); !ok {
		// A duplicate stream for a transfer ID already in flight.
		return refuse("transfer already in progress")
	}
	defer d.receiveClaims.release(o.ID)
	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	default:
		return refuse("receiver busy, try again soon")
	}
	if err := pc.Answer(o.ID, true, ""); err != nil {
		return false
	}
	_ = pc.SetDeadline(time.Now().Add(streamIdle))
	d.prog.start(Progress{
		ID:        o.ID,
		FileName:  o.FileName,
		Peer:      from.Name,
		Receiving: true,
		Total:     o.Size,
		Started:   time.Now().Unix(),
	})
	defer d.prog.end(o.ID)
	plainSize, err := d.saveIncoming(o, countingReader{k: d.prog, id: o.ID, r: io.LimitReader(idleReader{pc: pc, r: pc.Reader()}, o.Size)}, d.env.Identity.Private)
	if err != nil {
		d.cfg.logf("clowder: receiving %s from %s: %v", o.FileName, from.Name, err)
		return false
	}
	d.stats.add(func(s *Stats) { s.Received++; s.ReceivedBytes += plainSize })
	// The sender keeps a storer-held send until this receipt verifies
	// (#8): delivery is proven by the recipient, never by an ack.
	d.rememberReceipt(o)
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	if err := pc.Ack(o.ID, protocol.AckDelivered); err != nil {
		return false
	}
	d.cfg.logf("clowder: received %s (%s) from %s", o.FileName, HumanBytes(plainSize), from.Name)
	return true
}

// receiveAsStorer spools a sealed stream for an offline target; it stays
// opaque to us.
func (d *Daemon) receiveAsStorer(pc *protocol.Conn, o *protocol.Offer) bool {
	me := d.Me()
	if !me.Storer {
		return pc.Answer(o.ID, false, "not a storer") == nil
	}
	// Storers refuse unsigned deposits (#7): the per-sender quota needs
	// the sender key, and the final recipient verifies the signature.
	if o.FromKey == "" || len(o.Sig) == 0 {
		return pc.Answer(o.ID, false, "storer refuses unsigned offers") == nil
	}
	if me.Capacity <= 0 {
		return pc.Answer(o.ID, false, "storer has no capacity set") == nil
	}
	if !d.tryReserve(me.Capacity, o.Size) {
		reason := fmt.Sprintf("storer full (%s of %s held)", HumanBytes(d.heldBytes()), HumanBytes(me.Capacity))
		return pc.Answer(o.ID, false, reason) == nil
	}
	defer d.releaseReserve(o.Size)
	// A single cat must not be able to churn the whole spool for a full
	// TTL: each sender holds at most a quarter of the capacity. Derived
	// from the spool on every offer, so a restart cannot reset it.
	// Unsigned (pre-key) offers attribute to nothing and bypass this.
	if share := me.Capacity / 4; o.FromKey != "" && d.spool.UsageBy(o.FromKey)+o.Size > share {
		reason := fmt.Sprintf("storer per-sender quota (%s of %s held for you)",
			HumanBytes(d.spool.UsageBy(o.FromKey)), HumanBytes(share))
		return pc.Answer(o.ID, false, reason) == nil
	}
	_ = pc.SetDeadline(time.Now().Add(streamIdle))
	meta := store.Meta{
		ID:         o.ID,
		FileName:   o.FileName,
		Size:       o.Size,
		SHA256:     o.SHA256,
		From:       o.From,
		FromKey:    o.FromKey,
		TargetKey:  o.TargetKey,
		TargetName: o.TargetName,
		Sig:        o.Sig,
	}
	if err := pc.Answer(o.ID, true, ""); err != nil {
		return false
	}
	if err := d.spool.Put(meta, io.LimitReader(idleReader{pc: pc, r: pc.Reader()}, o.Size)); err != nil {
		d.cfg.logf("clowder: spooling %s for %s: %v", o.FileName, o.TargetName, err)
		return false
	}
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	if err := pc.Ack(o.ID, protocol.AckStored); err != nil {
		return false
	}
	d.stats.add(func(s *Stats) { s.Spooled++ })
	d.cfg.logf("clowder: holding %s from %s for %s", o.FileName, o.From, o.TargetName)
	d.goBg(func() { d.sweepSpoolFor(context.Background(), o.TargetKey) })
	return true
}

// tryReserve claims capacity atomically with the check, so concurrent
// deposits are accepted first-come-first-served.
func (d *Daemon) tryReserve(capacity, size int64) bool {
	d.resMu.Lock()
	defer d.resMu.Unlock()
	if capacity <= 0 || d.spool.Usage()+d.reserved+size > capacity {
		return false
	}
	d.reserved += size
	return true
}

func (d *Daemon) releaseReserve(size int64) {
	d.resMu.Lock()
	d.reserved -= size
	if d.reserved < 0 {
		d.reserved = 0
	}
	d.resMu.Unlock()
}

func (d *Daemon) heldBytes() int64 {
	d.resMu.Lock()
	defer d.resMu.Unlock()
	return d.spool.Usage() + d.reserved
}

// saveIncoming decrypts into the inbox under a unique name, verifying the
// announced size and plaintext digest.
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
		// A size lie or digest mismatch kills the conn; nothing lands in the inbox.
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

type exactReader struct {
	r io.Reader
	n int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.n += int64(n)
	return n, err
}

// inboxPath picks a non-existing inbox name, numbering collisions with a
// dash before the extension (nap-1.txt — a dot would read as an extension).
// Dotfiles keep their whole name as the stem.
func inboxPath(dir, name string) string {
	// Strip control characters first: os.Stat fails with EINVAL (not IsNotExist)
	// on a NUL byte, which would spin the collision loop below.
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
		} else if os.IsNotExist(err) {
			return p
		} else {
			// Unverifiable stat: keep numbering instead of spinning.
			_ = err
		}
		if i > 1<<16 {
			// Absurd collision count: pick something unique.
			return filepath.Join(dir, fmt.Sprintf("clow-%d%s", time.Now().UnixNano(), ext))
		}
		p = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
	}
}

// ---- outbound ----

func (d *Daemon) retryOutbox() {
	for _, e := range d.ob.All() {
		if e.HeldBy != "" {
			continue // waiting on a hold probe, not a retry
		}
		d.goBg(func() { d.deliver(e) })
	}
}

func (d *Daemon) deliver(e Entry) {
	ctx, ok := d.deliveryClaims.claim(e.ID)
	if !ok {
		return // already streaming this entry
	}
	defer d.deliveryClaims.release(e.ID)
	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	default:
		d.cfg.logf("clowder: %s to %s deferred: %d transfers already in flight", e.FileName, e.TargetName, maxConcurrentTransfers)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()

	if d.isBlockedKey(e.TargetKey) {
		d.cfg.logf("clowder: %s to %s skipped: target is on the blocklist", e.FileName, e.TargetName)
		return
	}
	// --storer skips the direct attempt: no dial, no timeout — the file
	// goes straight to a storer for the target to collect.
	if cat, ok := d.ros.GetByKey(e.TargetKey); ok && !e.ViaStorer {
		if err := d.deliverStream(ctx, cat, e, cat.Key, cat.Name, protocol.AckDelivered); err == nil {
			d.settle(e.ID, "delivered")
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
		if err := d.deliverStream(ctx, s, e, e.TargetKey, e.TargetName, protocol.AckStored); err == nil {
			d.settle(e.ID, "stored via "+s.Name)
			// AckStored is not proof the storer kept the file: the entry
			// stays until a probe confirms the storer still holds it.
			e.HeldBy = s.Key
			if err := d.ob.Put(e); err != nil {
				d.cfg.logf("clowder: recording holding storer for %s: %v", e.ID, err)
			}
			return
		} else {
			d.cfg.logf("clowder: via storer %s failed: %v", s.Name, err)
		}
	}
	d.cfg.logf("clowder: %s to %s still pending", e.FileName, e.TargetName)
}

// sourceDigest returns the source file's digest and size, hashing only
// when the file changed since the last attempt (or on the first one);
// the cached values ride the outbox entry, so retries over a stable
// file never re-read it. A file changed mid-send still fails the
// end-to-end check in sendSealed.
func (d *Daemon) sourceDigest(e *Entry) (string, int64, error) {
	fi, err := os.Stat(e.SourcePath)
	if err != nil {
		return "", 0, err
	}
	if e.SourceSHA256 != "" && e.SourceSize == fi.Size() && e.SourceModNs == fi.ModTime().UnixNano() {
		return e.SourceSHA256, e.SourceSize, nil
	}
	digest, size, err := fileDigest(e.SourcePath)
	if err != nil {
		return "", 0, err
	}
	after, err := os.Stat(e.SourcePath)
	if err != nil {
		return "", 0, err
	}
	if after.Size() == fi.Size() && after.ModTime().Equal(fi.ModTime()) {
		e.SourceSHA256, e.SourceSize, e.SourceModNs = digest, size, fi.ModTime().UnixNano()
		if err := d.ob.Put(*e); err != nil {
			d.cfg.logf("clowder: caching digest for %s: %v", e.ID, err)
		}
	}
	return digest, size, nil
}

func (d *Daemon) deliverStream(ctx context.Context, peer roster.Cat, e Entry, targetKey, targetName, wantAck string) error {
	digest, size, err := d.sourceDigest(&e)
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
		FromKey:    d.Me().Key,
		SHA256:     digest,
		TargetKey:  targetKey,
		TargetName: targetName,
	}
	// The recipient verifies this against the roster-pinned sign key:
	// no cat can send in another's name (#7).
	protocol.SignOffer(d.env.SignPriv, o)
	if err := d.sendSealed(ctx, peer, o, src, wantAck, true); err != nil {
		return err
	}
	// Stats are plaintext bytes, not sealed-stream bytes.
	d.stats.add(func(s *Stats) { s.Sent++; s.SentBytes += size })
	return nil
}

// sendSealed runs one Offer/Answer/stream/Ack exchange, shared by
// deliveries (track enables status progress).
func (d *Daemon) sendSealed(ctx context.Context, peer roster.Cat, o *protocol.Offer, src io.Reader, wantAck string, track bool) error {
	// No tighter connect bound here: a dead target is detected by the
	// dial's own disco probe (~10s), and a cold-but-online target —
	// netcheck + relay attach, >10s on slow links — needs the dial's
	// full internal bound. A uniform 10s cut the latter and broke the
	// CI integration pair-send.
	pc, err := d.connect(ctx, peer, msgTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = pc.Close() }()
	// A cancel aborts at the wire: closing the conn fails this stream
	// and the peer's, so the receiver drops its partial file instead
	// of waiting out the stream deadline.
	watching := make(chan struct{})
	defer close(watching)
	go func() {
		select {
		case <-ctx.Done():
			_ = pc.Close()
		case <-watching:
		}
	}()

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
	if sealedSha != o.SHA256 {
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

// syncPeers dials every peer in parallel, at start and on every poll
// tick, so membership and liveness converge within a tick. O(N²) bytes per
// cycle at homelab scale — pending item 3. No extra timeout: the transport
// bounds dials and the protocol bounds messages.
func (d *Daemon) syncPeers(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range d.ros.All() {
		wg.Add(1)
		go func(c roster.Cat) {
			defer wg.Done()
			pc, err := d.connect(ctx, c, msgTimeout)
			if err != nil {
				return
			}
			d.pushReceipts(pc, c.Key)
			_ = pc.Close()
		}(c)
	}
	wg.Wait()
}

// ---- handshake helpers ----

func (d *Daemon) helloMsg() *protocol.Hello {
	me := d.Me()
	return &protocol.Hello{
		Name:    me.Name,
		Key:     me.Key,
		DialKey: me.DialKey,
		SignKey: me.SignKey,
		Addr:    me.Addr,
		Storer:  me.Storer,
		Dropbox: me.Dropbox,
		Version: protocol.HelloVersion,
		// The digest of what we would push: matched hashes let the peer
		// answer with an empty roster instead (#16).
		RosterHash: d.ros.SyncHash(me),
	}
}

// rosterReplyFor builds the roster half of the handshake reply: the
// full push, or an empty one when the peer's RosterHash matches ours —
// it has nothing to learn. A hash-less (pre-#16) peer always gets the
// full roster.
func (d *Daemon) rosterReplyFor(peer *protocol.Hello) *protocol.RosterSync {
	if peer.RosterHash != "" && peer.RosterHash == d.ros.SyncHash(d.Me()) {
		return &protocol.RosterSync{}
	}
	return d.rosterMsg()
}

func (d *Daemon) rosterMsg() *protocol.RosterSync {
	cats := append([]roster.Cat{d.Me()}, d.ros.All()...)
	return &protocol.RosterSync{Cats: cats, Tombstones: d.ros.Tombstones()}
}

// connect dials and handshakes, bounding the exchange by timeout —
// most callers pass msgTimeout; leave needs a hard cap per cat, not minutes.
func (d *Daemon) connect(ctx context.Context, cat roster.Cat, timeout time.Duration) (*protocol.Conn, error) {
	conn, err := d.tr.Dial(ctx, cat.Addr)
	if err != nil {
		return nil, err
	}
	pc := protocol.NewConn(conn)
	if err := d.handshakeClient(pc, timeout); err != nil {
		_ = pc.Close()
		return nil, err
	}
	return pc, nil
}

func (d *Daemon) handshakeClient(pc *protocol.Conn, timeout time.Duration) error {
	_ = pc.SetDeadline(time.Now().Add(timeout))
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
	peer := m.Hello
	// The hello round-trip proves liveness both ways, even if the roster
	// exchange never completes.
	d.markSeen(m.Hello.Key)
	if err := pc.WriteMsg(&protocol.Message{Roster: d.rosterReplyFor(peer)}); err != nil {
		return err
	}
	m, err = pc.ReadMsg()
	if err != nil {
		return err
	}
	if m.Roster == nil {
		return errors.New("peer sent no roster")
	}
	d.mergeRemote(m.Roster)
	// Symmetric to the serve side: the server's own Hello pins its
	// sign key and heals a provisional pin (#12).
	d.acceptSelfEntry(peer, m.Roster)
	return nil
}

// mergeRemote applies tombstones first (a leave must land before the stale
// entries riding with it), then cats. A cat that has left merges nothing;
// the check and the merge run under rosterMu so a leave cannot wipe the
// roster between them and get its leaver repopulated.
func (d *Daemon) mergeRemote(sync *protocol.RosterSync) {
	d.rosterMu.Lock()
	defer d.rosterMu.Unlock()
	if d.hasLeft() {
		return
	}
	for _, t := range sync.Tombstones {
		d.handleTombstone(t, false)
	}
	cats := sync.Cats
	// Capture duplicates before the merge: parallel invites can both claim a
	// name; LWW is per-key, so both entries persist.
	before := d.ros.Duplicates()
	me := d.Me()
	filtered := cats[:0:0]
	for _, c := range cats {
		if c.Key == "" || c.Key == me.Key {
			continue
		}
		// Blocked cats stay out; re-pairing is the way back (addPeerCat bypasses
		// the merge).
		if d.isBlockedKey(c.Key) || d.isBlockedKey(c.DialKey) {
			continue
		}
		// A sync is not a trust root: an entry for a key we do not hold
		// must be signed (pairing is the only unsigned path in), and a
		// tailcat address carries the identity key, so the claimed Key
		// must be derivable from the Addr. Loopback tests use raw TCP
		// addrs, which carry nothing to check.
		if _, known := d.ros.GetByKey(c.Key); !known && !roster.VerifyEntry(c) {
			continue
		}
		if ci, err := tailcat.ParseAddr(tailcat.Addr(c.Addr)); err == nil {
			if ci.ServerPublic.String() != c.Key {
				continue
			}
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

// pinSignKey records the Hello's sign key on the peer's roster entry; the
// connection is transport-authenticated, and later merges verify against it.
// helloIdentityKnown reports whether the Hello's claimed identity key
// belongs to the authenticated client: a roster entry under that key
// whose DialKey matches. Pre-client-key entries cannot be checked
// and stay trusted as before.
func (d *Daemon) helloIdentityKnown(peer *protocol.Hello) bool {
	c, ok := d.ros.GetByKey(peer.Key)
	return ok && (c.DialKey == "" || c.DialKey == peer.DialKey)
}

// acceptSelfEntry takes the authenticated peer's own roster entry
// verbatim and pins its sign key (#12): a sign-key pin may only be
// established by its owner over an authenticated channel, so a
// sync-carried pin for a cat we never talked to is provisional — the
// cat's own Hello replaces it, stale LWW timestamps included. The
// peer's signed entry rides its roster push.
func (d *Daemon) acceptSelfEntry(peer *protocol.Hello, sync *protocol.RosterSync) {
	if peer.SignKey == "" {
		return
	}
	c, ok := d.ros.GetByKey(peer.Key)
	if !ok {
		return // never seen: pairing is the only path that introduces cats
	}
	if c.SignKey == peer.SignKey {
		return // pinned and matching
	}
	if !d.helloIdentityKnown(peer) {
		// The claimed dial key does not resolve to this identity: the
		// pin stays whatever it was (a forget or a forged entry), and
		// re-pairing is the way back.
		return
	}
	for _, own := range sync.Cats {
		if own.Key != peer.Key {
			continue
		}
		if err := d.ros.Add(own); err != nil {
			d.cfg.logf("clowder: re-pinning %s's sign key: %v", peer.Name, err)
		} else {
			d.cfg.logf("clowder: %s re-pinned from its live entry", peer.Name)
			d.allowCat(own)
		}
		return
	}
}

func (d *Daemon) allowCat(c roster.Cat) {
	// Peers dial us with their dial key, not their identity key.
	allow := c.DialKey
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

// validID reports whether s is a transfer ID: 32 hex chars, the only
// form this daemon generates.
func validID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

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
