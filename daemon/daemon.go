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
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/types/key"

	"clowder/envelope"
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
)

// Config configures a daemon. Dir is required and must have been created
// by Init (or contain a compatible identity, roster and me.json).
type Config struct {
	Dir  string
	Port uint16 // clowder protocol port; used by the tailcat transport
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

	ros   *roster.Roster
	spool *store.Spool
	ob    *outbox
	inbox string

	syncSeq atomic.Int64 // round-robin cursor for peer sync
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
	inbox := filepath.Join(cfg.Dir, "inbox")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: creating inbox: %w", err)
	}
	d := &Daemon{
		cfg:   cfg,
		tr:    tr,
		env:   env,
		ros:   ros,
		spool: store.New(filepath.Join(cfg.Dir, "spool"), 0),
		ob:    newOutbox(filepath.Join(cfg.Dir, "outbox")),
		inbox: inbox,
	}
	d.meCat = roster.Cat{
		Name:    env.Me.Name,
		Key:     env.Identity.Public.ServerPublic.String(),
		Storer:  env.Me.Storer,
		Updated: time.Now().Unix(),
	}
	return d, nil
}

// Me returns this cat's roster entry as currently known.
func (d *Daemon) Me() roster.Cat {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.meCat
}

// Roster returns the cat's roster, for inspection (clow cats) and tests.
func (d *Daemon) Roster() *roster.Roster { return d.ros }

// Spool returns the storer spool, for inspection and tests.
func (d *Daemon) Spool() *store.Spool { return d.spool }

// SetStorer declares or retracts this cat's storer role and persists it.
func (d *Daemon) SetStorer(on bool) error {
	d.mu.Lock()
	d.meCat.Storer = on
	d.meCat.Updated = time.Now().Unix()
	d.mu.Unlock()
	me := d.env.Me
	me.Storer = on
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
	d.mu.Unlock()
	d.cfg.logf("clowder: cat %s listening at %s", d.meCat.Name, d.meCat.Addr)

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

	retry := time.NewTicker(d.cfg.RetryEvery)
	defer retry.Stop()
	poll := time.NewTicker(d.cfg.PollEvery)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = ln.Close()
			_ = ipcLn.Close()
			_ = d.tr.Close()
			return nil
		case <-retry.C:
			go d.retryOutbox()
		case <-poll.C:
			go d.Poll(context.WithoutCancel(ctx))
			go d.syncPeers(context.WithoutCancel(ctx))
		}
	}
}

// Send queues a file for asynchronous delivery to the named cat and
// returns the transfer ID. Delivery is attempted in the background:
// directly to the target first, then via any reachable storer, retrying
// on the daemon's ticker until one succeeds.
func (d *Daemon) Send(targetName, path string) (string, error) {
	cat, ok := d.ros.Get(targetName)
	if !ok {
		return "", fmt.Errorf("unknown cat %q (known: add it first)", targetName)
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
	for _, s := range d.ros.Storers() {
		if err := d.fetchFrom(ctx, s); err != nil {
			d.cfg.logf("clowder: fetching from storer %s: %v", s.Name, err)
		}
	}
}

// AddCat records a cat in the roster (as `clow add` does) and allows it
// to connect.
func (d *Daemon) AddCat(name, addr string) error {
	c, err := roster.NewCat(name, addr, time.Now().Unix())
	if err != nil {
		return err
	}
	if err := d.ros.Add(c); err != nil {
		return err
	}
	d.allowCat(c)
	return nil
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
	if authed {
		claimed, err := parseKey(peer.Key)
		if err != nil || claimed != authKey {
			d.cfg.logf("clowder: closing conn with claimed key %s != authenticated %s", peer.Key, authKey)
			return
		}
	}
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
	if o.TargetKey == d.Me().Key {
		return d.receiveDirect(pc, from, o)
	}
	return d.receiveAsStorer(pc, o)
}

// receiveDirect decrypts a sealed stream into the inbox and acks
// delivery.
func (d *Daemon) receiveDirect(pc *protocol.Conn, from *protocol.Hello, o *protocol.Offer) bool {
	if err := pc.WriteMsg(&protocol.Message{Answer: &protocol.Answer{ID: o.ID, OK: true}}); err != nil {
		return false
	}
	_ = pc.SetDeadline(time.Now().Add(streamTimeout))
	if err := d.saveIncoming(o, io.LimitReader(pc.Reader(), o.Size), d.env.Identity.Private); err != nil {
		d.cfg.logf("clowder: receiving %s from %s: %v", o.FileName, from.Name, err)
		return false
	}
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	if err := pc.WriteMsg(&protocol.Message{Ack: &protocol.Ack{ID: o.ID, Kind: protocol.AckDelivered}}); err != nil {
		return false
	}
	d.cfg.logf("clowder: received %s from %s", o.FileName, from.Name)
	return true
}

// receiveAsStorer spools a sealed stream for an offline target. The
// stream stays opaque: the storer cannot decrypt it.
func (d *Daemon) receiveAsStorer(pc *protocol.Conn, o *protocol.Offer) bool {
	if !d.Me().Storer {
		if err := pc.WriteMsg(&protocol.Message{Answer: &protocol.Answer{
			ID: o.ID, OK: false, Reason: "not a storer",
		}}); err != nil {
			return false
		}
		return true
	}
	if err := pc.WriteMsg(&protocol.Message{Answer: &protocol.Answer{ID: o.ID, OK: true}}); err != nil {
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
	}
	if err := d.spool.Put(meta, io.LimitReader(pc.Reader(), o.Size)); err != nil {
		d.cfg.logf("clowder: spooling %s for %s: %v", o.FileName, o.TargetName, err)
		return false
	}
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	if err := pc.WriteMsg(&protocol.Message{Ack: &protocol.Ack{ID: o.ID, Kind: protocol.AckStored}}); err != nil {
		return false
	}
	d.cfg.logf("clowder: holding %s from %s for %s", o.FileName, o.From, o.TargetName)
	return true
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
	d.cfg.logf("clowder: delivered held %s to %s", meta.FileName, d.Me().Name)
	return true
}

// saveIncoming decrypts a sealed stream from src into the inbox under a
// unique name, verifying the announced plaintext digest.
func (d *Daemon) saveIncoming(o *protocol.Offer, src io.Reader, recipient key.NodePrivate) error {
	if err := os.MkdirAll(d.inbox, 0o700); err != nil {
		return fmt.Errorf("creating inbox: %w", err)
	}
	name := inboxPath(d.inbox, o.FileName)
	tmp, err := os.CreateTemp(d.inbox, ".recv-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, gotSha, err := envelope.OpenStream(recipient, src, tmp)
	if err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if gotSha != o.SHA256 {
		return fmt.Errorf("digest mismatch: got %s, announced %s", gotSha, o.SHA256)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), name); err != nil {
		return fmt.Errorf("moving into inbox: %w", err)
	}
	return nil
}

// inboxPath picks a non-existing name for a received file.
func inboxPath(dir, name string) string {
	clean := filepath.Base(filepath.Clean(name))
	if clean == "" || clean == "." || clean == ".." || clean == "/" {
		clean = "file"
	}
	p := filepath.Join(dir, clean)
	for i := 1; ; i++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s.%d", clean, i))
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), msgTimeout)
	defer cancel()

	if cat, ok := d.ros.GetByKey(e.TargetKey); ok {
		if err := d.deliverDirect(ctx, cat, e); err == nil {
			_ = d.ob.Delete(e.ID)
			return
		} else {
			d.cfg.logf("clowder: direct to %s failed: %v", cat.Name, err)
		}
	}
	for _, s := range d.ros.Storers() {
		if s.Key == e.TargetKey {
			continue
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

	pc, err := d.connect(ctx, peer)
	if err != nil {
		return err
	}
	defer func() { _ = pc.Close() }()

	offer := &protocol.Offer{
		ID:         e.ID,
		FileName:   e.FileName,
		Size:       envelope.SealedSize(size),
		From:       d.Me().Name,
		SHA256:     digest,
		TargetKey:  targetKey,
		TargetName: targetName,
	}
	if err := pc.WriteMsg(&protocol.Message{Offer: offer}); err != nil {
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
	targetPub, err := parseKey(targetKey)
	if err != nil {
		return err
	}
	_, sealedSha, err := envelope.SealStream(d.env.Identity.Private, targetPub, pc.Writer(), src)
	if err != nil {
		return err
	}
	if sealedSha != digest {
		return errors.New("file changed during send")
	}
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	m, err = pc.ReadMsg()
	if err != nil {
		return err
	}
	if m.Ack == nil || m.Ack.Kind != wantAck {
		return fmt.Errorf("wanted %s ack, got %v", wantAck, m.Ack)
	}
	d.cfg.logf("clowder: sent %s to %s", e.FileName, targetName)
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
	if err := pc.WriteMsg(&protocol.Message{Answer: &protocol.Answer{ID: o.ID, OK: true}}); err != nil {
		return err
	}
	_ = pc.SetDeadline(time.Now().Add(streamTimeout))
	if err := d.saveIncoming(o, io.LimitReader(pc.Reader(), o.Size), d.env.Identity.Private); err != nil {
		return err
	}
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	if err := pc.WriteMsg(&protocol.Message{Ack: &protocol.Ack{ID: o.ID, Kind: protocol.AckDelivered}}); err != nil {
		return err
	}
	d.cfg.logf("clowder: fetched %s from storer", o.FileName)
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
	return &protocol.Hello{Name: me.Name, Key: me.Key, Addr: me.Addr, Storer: me.Storer}
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
}

func (d *Daemon) allowCat(c roster.Cat) {
	k, err := parseKey(c.Key)
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
