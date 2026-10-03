package daemon

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"

	"github.com/cterence/clowder/envelope"
	"github.com/cterence/clowder/protocol"
	"github.com/cterence/clowder/roster"
)

// signedCat builds a roster entry signed by a throwaway sign key.
func signedCat(t *testing.T, name string, updated int64) roster.Cat {
	t.Helper()
	k := tailcat.NewPrivateKey()
	c, err := roster.NewCat(name, string(k.Public.Addr()), updated)
	if err != nil {
		t.Fatalf("NewCat: %v", err)
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	return roster.SignCat(priv, c)
}

// mergeSync merges a roster message into d the way a peer sync does.
func mergeSync(t *testing.T, d *Daemon, cats ...roster.Cat) {
	t.Helper()
	d.mergeRemote(&protocol.RosterSync{Cats: cats})
}

// TestSyncRejectsUnknownUnsignedEntry pins the roster-poisoning fix: a
// sync may not introduce an unsigned entry for a key we do not hold —
// pairing (addPeerCat) is the only unsigned path into a roster.
func TestSyncRejectsUnknownUnsignedEntry(t *testing.T) {
	milo := startDaemon(t, "milo")
	cedar := blockedCat(t, "cedar", time.Now().Unix())
	mergeSync(t, milo, cedar)
	if _, ok := milo.Roster().GetByKey(cedar.Key); ok {
		t.Fatal("unknown unsigned entry was merged from a sync")
	}
}

// TestSyncLearnsSignedUnknownEntry is the positive control: signed
// entries for unknown keys still propagate.
func TestSyncLearnsSignedUnknownEntry(t *testing.T) {
	milo := startDaemon(t, "milo")
	cedar := signedCat(t, "cedar", time.Now().Unix())
	mergeSync(t, milo, cedar)
	if _, ok := milo.Roster().GetByKey(cedar.Key); !ok {
		t.Fatal("signed entry for an unknown key was dropped")
	}
}

// TestSyncRejectsKeyNotDerivedFromAddr pins the Key/Addr binding: a
// tailcat address carries the identity key, so an entry claiming a key
// its address cannot derive is a forgery, signature or not.
func TestSyncRejectsKeyNotDerivedFromAddr(t *testing.T) {
	milo := startDaemon(t, "milo")
	spoof := signedCat(t, "spoof", time.Now().Unix())
	other := blockedCat(t, "other", time.Now().Unix())
	spoof.Key = other.Key // valid tailcat addr, foreign claimed key
	mergeSync(t, milo, spoof)
	if _, ok := milo.Roster().GetByKey(spoof.Key); ok {
		t.Fatal("entry whose Key does not derive from its Addr was merged")
	}
}

// authedLocalTransport makes loopback connections claim a dial key
// the daemon treats as transport-authenticated.
type authedLocalTransport struct {
	*LocalTransport
	peer key.NodePublic
}

func (t *authedLocalTransport) PeerKey(net.Addr) (key.NodePublic, bool) {
	return t.peer, true
}

// sayHello dials d's transport and speaks a bare Hello exchange.
func sayHello(t *testing.T, d *Daemon, h *protocol.Hello) {
	t.Helper()
	conn, err := net.Dial("tcp", d.Me().Addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	pc := protocol.NewConn(conn)
	if err := pc.WriteMsg(&protocol.Message{Hello: h}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	_, _ = pc.ReadMsg()
}

// TestServeConnMarksOnlyKnownIdentities pins the liveness fix: an
// authenticated dial key may claim its own roster identity, and
// nothing else — a fake Key claim must not enter the liveness map.
func TestServeConnMarksOnlyKnownIdentities(t *testing.T) {
	cedar := startDaemon(t, "cedar")
	clientKey, err := parseKey(cedar.Me().DialKey)
	if err != nil {
		t.Fatalf("parseKey: %v", err)
	}
	miloDir := t.TempDir()
	if err := Init(miloDir, "milo"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	milo := runDaemon(t, miloDir, &authedLocalTransport{LocalTransport: &LocalTransport{}, peer: clientKey})
	addCat(t, milo, cedar.Me())

	sayHello(t, milo, &protocol.Hello{ // honest claim
		Name: "cedar", Key: cedar.Me().Key, DialKey: cedar.Me().DialKey,
		Addr: cedar.Me().Addr, Version: protocol.HelloVersion,
	})
	waitFor(t, func() bool {
		_, ok := milo.livenessSnapshot()[cedar.Me().Key]
		return ok
	}, "honest hello to mark liveness")

	ghost := blockedCat(t, "ghost", time.Now().Unix())
	sayHello(t, milo, &protocol.Hello{ // forged identity claim
		Name: "cedar", Key: ghost.Key, DialKey: cedar.Me().DialKey,
		Addr: cedar.Me().Addr, Version: protocol.HelloVersion,
	})
	time.Sleep(200 * time.Millisecond)
	if _, ok := milo.livenessSnapshot()[ghost.Key]; ok {
		t.Fatal("forged identity claim was marked seen")
	}
}

// TestMarkSeenBounded pins the liveness-map bound: however many keys
// are claimed, the map cannot grow without limit.
func TestMarkSeenBounded(t *testing.T) {
	d := startDaemon(t, "milo")
	for i := 0; i < livenessMax+10; i++ {
		d.markSeen(fmt.Sprintf("nodekey:%064d", i))
	}
	if got := len(d.livenessSnapshot()); got > livenessMax {
		t.Fatalf("liveness map holds %d entries, want <= %d", got, livenessMax)
	}
}

// TestInviteCodeStaysOutOfLogs pins the log-redaction fix: the pairing
// code is a 5-minute credential and must never appear whole in a log.
// The DERP map comes from a loopback httptest server: no test may
// reach the network (CI runners cannot verify tailcat.dev's cert).
func TestInviteCodeStaysOutOfLogs(t *testing.T) {
	const regionJSON = `{"Regions": {"99": {
		"RegionID": 99, "RegionCode": "tst", "RegionName": "test relay",
		"Nodes": [{"Name": "t1", "RegionID": 99, "RegionCode": "tst", "HostName": "127.0.0.1:1"}]
	}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(regionJSON))
	}))
	defer srv.Close()

	var mu sync.Mutex
	var logs strings.Builder
	d := startDaemon(t, "milo", func(c *Config) {
		c.DERPMapURL = srv.URL
		c.Logf = func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprintf(&logs, format, args...)
			logs.WriteByte('\n')
		}
	})
	code, err := d.StartInvite(context.Background())
	if err != nil {
		t.Fatalf("StartInvite: %v", err)
	}
	t.Cleanup(d.stopInvite)
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logs.String(), code) {
		t.Fatalf("full pairing code %q appears in the daemon log", code)
	}
}

// TestIPCPathWithColonStaysUnix pins the IPC fix: a socket path
// containing ':' must never silently switch to unauthenticated TCP.
func TestIPCPathWithColonStaysUnix(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "we:ird")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "clow.sock")
	ln, err := listenIPC(path)
	if err != nil {
		t.Fatalf("listenIPC: %v", err)
	}
	defer func() { _ = ln.Close() }()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("socket file missing: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("IPC path with ':' is not a unix socket (mode %v)", fi.Mode())
	}
	if _, err := dialIPC(path); err != nil {
		t.Fatalf("dialIPC: %v", err)
	}
}

// TestPeerConnCapDropsExcess pins the per-peer connection cap: one
// authenticated peer may hold only maxPeerConns concurrent connections,
// so a churner cannot pile unbounded serves onto one daemon.
func TestPeerConnCapDropsExcess(t *testing.T) {
	cedar, _ := offlineCat(t)
	clientKey, err := parseKey(cedar.DialKey)
	if err != nil {
		t.Fatalf("parseKey: %v", err)
	}
	miloDir := t.TempDir()
	if err := Init(miloDir, "milo"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	milo := runDaemon(t, miloDir, &authedLocalTransport{LocalTransport: &LocalTransport{}, peer: clientKey})
	ck := clientKey.String()

	n := maxPeerConns + 2
	conns := make([]net.Conn, n)
	for i := range conns {
		var err error
		conns[i], err = net.Dial("tcp", milo.Me().Addr)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		defer func() { _ = conns[i].Close() }()
	}
	var hellos atomic.Int32
	var wg sync.WaitGroup
	for _, conn := range conns {
		wg.Add(1)
		go func(conn net.Conn) {
			defer wg.Done()
			pc := protocol.NewConn(conn)
			if err := pc.WriteMsg(&protocol.Message{Hello: &protocol.Hello{
				Name: "churner", Key: ck, DialKey: ck, Addr: "127.0.0.1:1", Version: protocol.HelloVersion,
			}}); err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, err := pc.ReadMsg(); err == nil {
				hellos.Add(1)
			}
		}(conn)
	}
	wg.Wait()
	if got := hellos.Load(); got != int32(maxPeerConns) {
		t.Fatalf("got %d hello replies, want exactly %d", got, maxPeerConns)
	}
}

// TestRosterMergeCapCutsConn pins the post-handshake merge throttle:
// a peer may push at most maxRosterMerges roster updates on one
// connection; beyond that the connection is cut, so a churner cannot
// run unbounded merges (each a full signature verify pass plus a
// possible roster write).
func TestRosterMergeCapCutsConn(t *testing.T) {
	milo := startDaemon(t, "milo")
	conn, err := net.Dial("tcp", milo.Me().Addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	pc := protocol.NewConn(conn)
	me := milo.Me()
	if err := pc.WriteMsg(&protocol.Message{Hello: &protocol.Hello{
		Name: "churner", Key: me.Key, DialKey: me.DialKey, Addr: me.Addr, Version: protocol.HelloVersion,
	}}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if _, err := pc.ReadMsg(); err != nil {
		t.Fatalf("hello reply: %v", err)
	}
	if err := pc.WriteMsg(&protocol.Message{Roster: &protocol.RosterSync{}}); err != nil {
		t.Fatalf("handshake roster: %v", err)
	}
	if _, err := pc.ReadMsg(); err != nil {
		t.Fatalf("handshake roster reply: %v", err)
	}
	for i := 0; i < maxRosterMerges; i++ {
		if err := pc.WriteMsg(&protocol.Message{Roster: &protocol.RosterSync{}}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	// One push beyond the cap cuts the connection: the next read fails.
	if err := pc.WriteMsg(&protocol.Message{Roster: &protocol.RosterSync{}}); err != nil {
		t.Fatalf("over-cap push: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := pc.ReadMsg(); err == nil {
		t.Fatal("connection served a merge beyond the cap")
	}
}

// TestRelayedOfferBlockedByKey pins the key-based blocklist fix: a held
// file from a forgotten cat must be refused by its target even though
// the relaying storer — not the sender — delivers it, and the roster
// no longer resolves the forgotten name.
func TestRelayedOfferBlockedByKey(t *testing.T) {
	milo := startDaemon(t, "milo")
	storer := startDaemon(t, "storer", func(c *Config) { c.SpoolSweepEvery = time.Hour })
	if err := storer.SetStorer(true, 1<<30); err != nil {
		t.Fatal(err)
	}
	trust(t, milo, storer)
	trust(t, storer, milo)
	niko, nikoDir := offlineCat(t)
	addCat(t, milo, niko)
	addCat(t, storer, niko)

	src := writeSource(t, "nap for a sleeping cat")
	if _, err := milo.Send("niko", src); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, func() bool { return storer.Spool().Count() == 1 }, "storer to hold the file")

	// niko wakes, knows milo, then forgets it: milo is blocked by key,
	// but no roster entry resolves the name anymore.
	nikoD := startDaemonAt(t, nikoDir)
	addCat(t, nikoD, milo.Me())
	trust(t, nikoD, storer)
	trust(t, storer, nikoD)
	if _, ok := nikoD.Forget("milo"); !ok {
		t.Fatal("forget milo")
	}

	storer.sweepSpoolFor(context.Background(), niko.Key)
	if _, ok := inboxFile(t, nikoD, "nap.txt"); ok {
		t.Fatal("held file from a blocked sender landed in the inbox")
	}
	if storer.Spool().Count() != 1 {
		t.Fatal("storer dropped the refused file")
	}
}

// TestStorerPerSenderQuota pins the per-sender spool share: one cat
// cannot churn a storer's whole capacity for a full TTL — a quarter of
// it is all any single sender may hold.
func TestStorerPerSenderQuota(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	logf := func(f string, a ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	milo := startDaemon(t, "milo", func(c *Config) {
		c.PollEvery = time.Hour
		c.Logf = logf
	})
	storer := startDaemon(t, "storer", func(c *Config) { c.SpoolSweepEvery = time.Hour })
	plain := strings.Repeat("nap", 1000) // 3000 B
	share := envelope.SealedSize(int64(len(plain)))
	// Capacity four times one sealed file: the per-sender share is
	// exactly one deposit.
	if err := storer.SetStorer(true, 4*share); err != nil {
		t.Fatal(err)
	}
	trust(t, milo, storer)
	trust(t, storer, milo)
	niko, _ := offlineCat(t)
	addCat(t, milo, niko)
	addCat(t, storer, niko)

	if _, err := milo.Send("niko", writeSource(t, plain)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, func() bool { return storer.Spool().Count() == 1 }, "first deposit to fill the sender's share")

	// The second deposit exceeds the share: refused, the spool stays at one.
	if _, err := milo.Send("niko", writeSource(t, plain)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.Join(logs, "\n"), "still pending")
	}, "over-quota deposit to be refused")
	if storer.Spool().Count() != 1 {
		t.Fatalf("storer holds %d files, want 1 (over-quota deposit must be refused)", storer.Spool().Count())
	}
}

// TestOfferSignatureRequired pins #7: an offer must be signed by its
// sender's pinned sign key — a spoofed From or a forged offer is
// refused, and storers refuse unsigned deposits (the relayed offer
// keeps its signature for the final recipient to verify).
func TestOfferSignatureRequired(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, fluff, milo)

	newOffer := func() *protocol.Offer {
		return &protocol.Offer{
			ID: strings.Repeat("a", 32), FileName: "nap.txt", Size: 10,
			From: "milo", FromKey: milo.Me().Key,
			TargetKey: fluff.Me().Key, TargetName: "fluff",
		}
	}
	try := func(o *protocol.Offer) *protocol.Answer {
		client, server := net.Pipe()
		go func() {
			_ = fluff.handleOffer(protocol.NewConn(server), &protocol.Hello{Name: "milo", Key: milo.Me().Key}, "", o)
			_ = server.Close()
		}()
		cpc := protocol.NewConn(client)
		m, err := cpc.ReadMsg()
		if err != nil {
			t.Fatalf("reading answer: %v", err)
		}
		_ = client.Close()
		if m.Answer == nil {
			t.Fatal("receiver sent no answer")
		}
		return m.Answer
	}

	if a := try(newOffer()); a.OK || a.Reason != "offer signature does not verify" {
		t.Fatalf("unsigned offer: ok=%v reason=%q, want refused", a.OK, a.Reason)
	}
	spoofed := newOffer()
	_, wrong, _ := ed25519.GenerateKey(nil)
	protocol.SignOffer(wrong, spoofed)
	if a := try(spoofed); a.OK || a.Reason != "offer signature does not verify" {
		t.Fatalf("spoofed offer: ok=%v reason=%q, want refused", a.OK, a.Reason)
	}
	stranger := newOffer()
	stranger.FromKey = tailcat.NewPrivateKey().Public.ServerPublic.String()
	if a := try(stranger); a.OK || a.Reason != "unknown sender" {
		t.Fatalf("unknown sender: ok=%v reason=%q, want refused", a.OK, a.Reason)
	}

	// Storers refuse unsigned deposits: the per-sender quota needs the
	// key, and the final recipient verifies the signature.
	box := startDaemon(t, "box")
	if err := box.SetStorer(true, 1<<30); err != nil {
		t.Fatal(err)
	}
	niko, _ := offlineCat(t)
	addCat(t, box, niko)
	unsigned := newOffer()
	unsigned.TargetKey = niko.Key
	unsigned.TargetName = "niko"
	client, server := net.Pipe()
	go func() {
		_ = box.handleOffer(protocol.NewConn(server), &protocol.Hello{Name: "milo", Key: milo.Me().Key}, "", unsigned)
		_ = server.Close()
	}()
	cpc := protocol.NewConn(client)
	m, err := cpc.ReadMsg()
	if err != nil {
		t.Fatalf("reading storer answer: %v", err)
	}
	_ = client.Close()
	if m.Answer == nil || m.Answer.OK || m.Answer.Reason != "storer refuses unsigned offers" {
		t.Fatalf("unsigned deposit: %+v, want refused", m.Answer)
	}
}

// TestSyncPinIsProvisional pins the #12 binding: a sync-carried
// sign-key pin for a cat we have never talked to is provisional. A
// paired cat cannot pre-register a victim's key under its own sign
// key: the victim's authenticated Hello re-pins and takes its live
// entry, stale LWW timestamps included.
func TestSyncPinIsProvisional(t *testing.T) {
	milo := startDaemon(t, "milo")
	evil := startDaemon(t, "evil")
	niko, nikoDir := offlineCat(t)

	// evil pre-registers niko's key under its own sign key, with a
	// newer timestamp — the forged entry rides a plain sync.
	forged := roster.SignCat(evil.env.SignPriv, roster.Cat{
		Name: "niko", Addr: "127.0.0.1:1", Key: niko.Key,
		DialKey: niko.DialKey, Updated: time.Now().Unix() + 1,
	})
	milo.mergeRemote(&protocol.RosterSync{Cats: []roster.Cat{forged}})
	got, ok := milo.ros.GetByKey(niko.Key)
	if !ok || got.SignKey != forged.SignKey {
		t.Fatalf("forged entry did not land (ok=%v sign=%v)", ok, got.SignKey)
	}

	// niko wakes and syncs: its authenticated Hello re-pins.
	nikoD := startDaemonAt(t, nikoDir)
	addCat(t, nikoD, milo.Me())
	waitFor(t, func() bool {
		c, ok := milo.ros.GetByKey(niko.Key)
		return ok && c.SignKey == nikoD.Me().SignKey && c.Addr == nikoD.Me().Addr
	}, "niko's authenticated Hello to re-pin its sign key")

	// The re-pin sticks: a newer forged push can no longer shadow niko.
	milo.mergeRemote(&protocol.RosterSync{Cats: []roster.Cat{roster.SignCat(evil.env.SignPriv, roster.Cat{
		Name: "niko", Addr: "127.0.0.1:1", Key: niko.Key,
		DialKey: niko.DialKey, Updated: time.Now().Unix() + 100,
	})}})
	c, _ := milo.ros.GetByKey(niko.Key)
	if c.SignKey != nikoD.Me().SignKey {
		t.Fatal("a forged sync push overwrote the re-pinned sign key")
	}
}

// TestStorerQuotaCountsAuthenticatedDialer pins the #26 fix: the
// per-sender quota counts the transport-authenticated dialer, not the
// offer's self-declared FromKey — the storer checks the signature for
// presence only, so rotating FromKeys must not buy extra share.
func TestStorerQuotaCountsAuthenticatedDialer(t *testing.T) {
	dialPub := key.NewNode().Public()
	boxDir := t.TempDir()
	if err := Init(boxDir, "box"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	box := runDaemon(t, boxDir, &authedLocalTransport{LocalTransport: &LocalTransport{}, peer: dialPub})
	const size = 100
	if err := box.SetStorer(true, 4*size); err != nil { // share: one deposit
		t.Fatal(err)
	}

	conn, err := net.Dial("tcp", box.Me().Addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	pc := protocol.NewConn(conn)
	dial := dialPub.String()
	if err := pc.WriteMsg(&protocol.Message{Hello: &protocol.Hello{
		Name: "churner", Key: dial, DialKey: dial,
		Addr: "127.0.0.1:1", Version: protocol.HelloVersion,
	}}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if _, err := pc.ReadMsg(); err != nil {
		t.Fatalf("hello reply: %v", err)
	}
	if err := pc.WriteMsg(&protocol.Message{Roster: &protocol.RosterSync{}}); err != nil {
		t.Fatalf("roster: %v", err)
	}
	if _, err := pc.ReadMsg(); err != nil {
		t.Fatalf("roster reply: %v", err)
	}

	deposit := func(i int, fromKey string) *protocol.Answer {
		o := &protocol.Offer{
			ID:         fmt.Sprintf("%032d", i),
			FileName:   "nap.txt",
			Size:       size,
			From:       "churner",
			FromKey:    fromKey,
			SHA256:     strings.Repeat("0", 64),
			TargetKey:  "nodekey:" + strings.Repeat("ff", 32),
			TargetName: "niko",
			Sig:        make([]byte, 64), // present, never verified here
		}
		if err := pc.WriteMsg(&protocol.Message{Offer: o}); err != nil {
			t.Fatalf("offer: %v", err)
		}
		m, err := pc.ReadMsg()
		if err != nil {
			t.Fatalf("answer: %v", err)
		}
		if m.Answer == nil {
			t.Fatal("storer sent no answer")
		}
		if m.Answer.OK {
			if _, err := pc.Writer().Write(make([]byte, size)); err != nil {
				t.Fatalf("stream: %v", err)
			}
			ack, err := pc.ReadMsg()
			if err != nil {
				t.Fatalf("ack: %v", err)
			}
			if ack.Ack == nil || ack.Ack.Kind != protocol.AckStored {
				t.Fatalf("want stored ack, got %v", ack.Ack)
			}
		}
		return m.Answer
	}

	if a := deposit(1, "nodekey:"+strings.Repeat("aa", 32)); !a.OK {
		t.Fatalf("first deposit refused: %s", a.Reason)
	}
	rotated := deposit(2, "nodekey:"+strings.Repeat("bb", 32))
	if rotated.OK || !strings.Contains(rotated.Reason, "per-sender quota") {
		t.Fatalf("a rotated FromKey bought extra share: ok=%v reason=%q", rotated.OK, rotated.Reason)
	}
}
