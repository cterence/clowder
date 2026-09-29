package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/types/key"

	"clowder/protocol"
	"clowder/roster"
)

// TestMain points t.TempDir() at a short-path root. Every daemon test
// config dir hosts the daemon's unix IPC socket, and macOS rejects
// socket paths over ~103 bytes — the default TMPDIR (/var/folders/...)
// plus a long test name overflows that, Run dies at the IPC listen
// and peers see "connection refused". If /tmp is unusable the tests
// keep the default TMPDIR.
func TestMain(m *testing.M) {
	if root, err := os.MkdirTemp("/tmp", "clowder-test-"); err == nil {
		_ = os.Setenv("TMPDIR", root)
		code := m.Run()
		_ = os.RemoveAll(root)
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// LocalTransport runs the clowder protocol over loopback TCP for tests.
type LocalTransport struct {
	mu   sync.Mutex
	ln   net.Listener
	addr string
}

func (t *LocalTransport) Listen(ctx context.Context) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.ln = ln
	t.addr = ln.Addr().String()
	t.mu.Unlock()
	return ln, nil
}

func (t *LocalTransport) MyAddr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.addr
}

func (t *LocalTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func (t *LocalTransport) Allow(key.NodePublic) {}

// Loopback is a direct path by construction.
func (t *LocalTransport) Probe(ctx context.Context, addr string) (string, error) {
	return "direct", nil
}

func (t *LocalTransport) PeerKey(net.Addr) (key.NodePublic, bool) {
	return key.NodePublic{}, false
}

func (t *LocalTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ln == nil {
		return nil
	}
	return t.ln.Close()
}

// ---- harness ----

func startDaemon(t *testing.T, name string, overrides ...func(*Config)) *Daemon {
	t.Helper()
	dir := t.TempDir()
	if err := Init(dir, name); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return startDaemonAt(t, dir, overrides...)
}

// daemonStops maps a running loopback daemon to its stop function, so
// tests can take a cat offline mid-test.
var daemonStops sync.Map

// stopDaemon takes a loopback daemon offline: it cancels Run and waits
// for Run to return, so the daemon has drained its background work
// (receipt relays, sweeps, live connections) by the time the caller
// proceeds — a restart on the same dir or a roster write must not
// race the old Run's stragglers.
func stopDaemon(d *Daemon) {
	if f, ok := daemonStops.Load(d); ok {
		f.(func())()
	}
}

// startDaemonAt runs a daemon on a config dir that Init (or a sleeping
// offline cat) already prepared.
func startDaemonAt(t *testing.T, dir string, overrides ...func(*Config)) *Daemon {
	t.Helper()
	return runDaemon(t, dir, &LocalTransport{}, overrides...)
}

// testPollEvery is the harness sync tick; tests that need to tell an
// event-driven sync from tick convergence raise it for their daemons.
var testPollEvery = 150 * time.Millisecond

// runDaemon starts a daemon on a prepared config dir with a custom
// transport.
func runDaemon(t *testing.T, dir string, tr Transport, overrides ...func(*Config)) *Daemon {
	t.Helper()
	// Keep the default inbox (under $HOME/Downloads/clowder) inside the
	// test sandbox.
	t.Setenv("HOME", t.TempDir())
	cfg := Config{
		Dir:        dir,
		RetryEvery: 150 * time.Millisecond,
		PollEvery:  testPollEvery,
		Logf:       t.Logf,
	}
	for _, o := range overrides {
		o(&cfg)
	}
	d, err := New(cfg, tr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			<-runDone // Run drained its background work before returning
		})
	}
	t.Cleanup(stop)
	daemonStops.Store(d, stop)
	go func() {
		_ = d.Run(ctx)
		close(runDone)
	}()
	waitFor(t, func() bool { return d.Me().Addr != "" }, "daemon %s to listen", d.Me().Name)
	return d
}

// trust makes d know and allow another daemon.
func trust(t *testing.T, d *Daemon, other *Daemon) {
	t.Helper()
	c := other.Me()
	if err := d.Roster().Add(c); err != nil {
		t.Fatalf("adding peer: %v", err)
	}
	d.allowCat(c)
}

// offlineCat builds a roster entry for a cat whose daemon is not
// running, persisting its identity so it can wake up later as the same
// cat. The name is always niko: the tests treat it as a fixture.
func offlineCat(t *testing.T) (roster.Cat, string) {
	t.Helper()
	const name = "niko"
	dir := t.TempDir()
	if err := Init(dir, name); err != nil {
		t.Fatalf("Init: %v", err)
	}
	env, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	signPub := env.SignPriv.Public().(ed25519.PublicKey)
	return roster.Cat{
		Name:      name,
		Addr:      "127.0.0.1:1", // nothing listens here
		Key:       env.Identity.Public.ServerPublic.String(),
		ClientKey: env.ClientIdentity.Public().String(),
		SignKey:   hex.EncodeToString(signPub),
		Updated:   time.Now().Unix(),
	}, dir
}

// addCat records a raw roster entry.
func addCat(t *testing.T, d *Daemon, c roster.Cat) {
	t.Helper()
	if err := d.Roster().Add(c); err != nil {
		t.Fatalf("adding cat %s: %v", c.Name, err)
	}
	d.allowCat(c)
}

func waitFor(t *testing.T, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", fmt.Sprintf(format, args...))
}

func writeSource(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nap.txt")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func inboxFile(t *testing.T, d *Daemon, name string) (string, bool) {
	t.Helper()
	p := filepath.Join(d.InboxDir(), name)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// ---- tests ----

func TestDirectSend(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	src := writeSource(t, "direct nap data")
	if _, err := milo.Send("fluff", src); err != nil {
		t.Fatalf("Send: %v", err)
	}

	waitFor(t, func() bool {
		got, ok := inboxFile(t, fluff, "nap.txt")
		return ok && got == "direct nap data"
	}, "fluff to receive the file")

	waitFor(t, func() bool { return len(milo.ob.All()) == 0 }, "milo's outbox to drain")
}

func TestStorerRelayForOfflineCat(t *testing.T) {
	milo := startDaemon(t, "milo")
	storer := startDaemon(t, "storer")
	if err := storer.SetStorer(true, 1<<30); err != nil {
		t.Fatal(err)
	}
	trust(t, milo, storer)
	trust(t, storer, milo)

	// A target cat that is offline (never started), known to milo and
	// the storer by identity.
	niko, nikoDir := offlineCat(t)
	addCat(t, milo, niko)
	addCat(t, storer, niko)

	src := writeSource(t, "nap for a sleeping cat")
	if _, err := milo.Send("niko", src); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The storer ends up holding the sealed stream.
	waitFor(t, func() bool { return storer.Spool().Count() == 1 }, "storer to hold the file")
	waitFor(t, func() bool { return len(milo.ob.All()) == 0 }, "milo's outbox to drain")

	// The spooled stream must not contain the plaintext.
	b, err := os.ReadFile(filepath.Join(storer.cfg.Dir, "spool", storer.Spool().List(niko.Key)[0].ID+".blob"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "nap for a sleeping cat") {
		t.Fatal("spooled stream contains plaintext")
	}

	// Now niko wakes up — same persisted identity — and receives the
	// held file WITHOUT polling: the storer's sweep pushes it once
	// the target is online and known.
	nikoD := startDaemonAt(t, nikoDir)
	trust(t, nikoD, storer)
	trust(t, storer, nikoD)

	waitFor(t, func() bool {
		got, ok := inboxFile(t, nikoD, "nap.txt")
		return ok && got == "nap for a sleeping cat"
	}, "niko to receive the held file via the storer's push sweep")
	waitFor(t, func() bool { return storer.Spool().Count() == 0 }, "storer to drop the delivered file")
}

func TestRosterPropagation(t *testing.T) {
	// milo <-> fluff, fluff <-> niko. milo never talks to niko directly,
	// but must learn about it through fluff.
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	niko := startDaemon(t, "niko")
	trust(t, milo, fluff)
	trust(t, fluff, milo)
	trust(t, fluff, niko)
	trust(t, niko, fluff)

	nikoKey := niko.Me().Key
	waitFor(t, func() bool {
		_, ok := milo.Roster().GetByKey(nikoKey)
		return ok
	}, "milo to learn about niko via fluff")

	c, ok := milo.Roster().GetByKey(nikoKey)
	if !ok || c.Name != "niko" {
		t.Fatalf("milo's entry for niko = %+v", c)
	}
}

// TestStartupSyncBurst pins the wake path: a cat that starts cold
// (woke up, daemon restarted) must learn what changed while it was
// out within seconds, by dialing every roster peer at Run start —
// not by waiting for the one-peer-per-tick round-robin to reach the
// one online peer that knows. The waker's poll tick is a minute out,
// so only the startup burst can deliver the third cat.
func TestStartupSyncBurst(t *testing.T) {
	macbook := startDaemon(t, "macbook")
	niko, _ := offlineCat(t)
	addCat(t, macbook, niko)

	wakerDir := t.TempDir()
	if err := Init(wakerDir, "waker"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seed := roster.New()
	if err := seed.SetPath(filepath.Join(wakerDir, "roster.json")); err != nil {
		t.Fatalf("SetPath: %v", err)
	}
	if err := seed.Add(macbook.Me()); err != nil {
		t.Fatalf("seeding waker's roster with macbook: %v", err)
	}

	// The steady-state sync (one peer per poll tick) cannot fire for a
	// minute: anything the waker learns during the test comes from the
	// startup burst.
	waker, err := New(Config{
		Dir:        wakerDir,
		RetryEvery: 150 * time.Millisecond,
		PollEvery:  time.Minute,
		Logf:       t.Logf,
	}, &LocalTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// macbook must allow the waker's client key for the burst's
	// handshake to complete (they were paired before the waker slept).
	addCat(t, macbook, waker.Me())

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-runDone // Run drained its background work before returning
	})
	go func() {
		_ = waker.Run(ctx)
		close(runDone)
	}()
	waitFor(t, func() bool { return waker.Me().Addr != "" }, "waker to listen")

	waitFor(t, func() bool {
		_, ok := waker.Roster().GetByKey(niko.Key)
		return ok
	}, "waker to learn niko via the startup sync burst")
}

// dialRecordingTransport records every Dial attempt, so tests can
// assert which roster entries a sync tried to reach.
type dialRecordingTransport struct {
	*LocalTransport
	mu    sync.Mutex
	dials map[string]int
}

func (d *dialRecordingTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dials[addr]++
	d.mu.Unlock()
	return d.LocalTransport.Dial(ctx, addr)
}

func (d *dialRecordingTransport) dialed() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]int, len(d.dials))
	for k, v := range d.dials {
		out[k] = v
	}
	return out
}

// TestSyncPeersDialsEveryPeer pins the steady-state sync shape: one
// syncPeers call attempts EVERY roster entry, in parallel. The old
// one-peer-per-tick round-robin left a given peer unseen for N ticks
// — with four cats that was ~4 minutes, aging even an always-online
// peer past the online window and stretching wake convergence.
func TestSyncPeersDialsEveryPeer(t *testing.T) {
	dt := &dialRecordingTransport{LocalTransport: &LocalTransport{}, dials: map[string]int{}}
	dir := t.TempDir()
	if err := Init(dir, "milo"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	milo := runDaemon(t, dir, dt)

	fluff := startDaemon(t, "fluff")
	addCat(t, fluff, milo.Me())
	cedar := blockedCat(t, "cedar", time.Now().Unix())
	addCat(t, fluff, cedar) // milo does not know cedar yet
	ghost, _ := offlineCat(t)
	ghost2 := blockedCat(t, "birch", time.Now().Unix())
	addCat(t, milo, fluff.Me())
	addCat(t, milo, ghost)  // offline: nothing listens
	addCat(t, milo, ghost2) // offline: nothing listens

	milo.syncPeers(context.Background())

	dialed := dt.dialed()
	for _, c := range []roster.Cat{fluff.Me(), ghost, ghost2} {
		if dialed[c.Addr] == 0 {
			t.Errorf("syncPeers skipped %s; every roster entry must be reached each sync", c.Name)
		}
	}
	if _, ok := milo.Roster().GetByKey(cedar.Key); !ok {
		t.Error("milo did not learn cedar from fluff in one sync")
	}
}

// TestSecondDaemonOnSameDirRefuses pins the single-instance rule: a
// second daemon on the same cat (same config dir, same identity)
// cross-writes the roster and every ledger and runs a second engine
// with the same node key — the tunnel wedges (see the two-keypair
// invariant). Before the lock it would steal the live daemon's IPC
// socket (listenIPC removes the path first) and run happily.
func TestSecondDaemonOnSameDirRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, "milo"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	first := startDaemonAt(t, dir)

	second, err := New(Config{
		Dir:        dir,
		RetryEvery: 150 * time.Millisecond,
		PollEvery:  150 * time.Millisecond,
		Logf:       t.Logf,
	}, &LocalTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- second.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "already running") {
			t.Fatalf("second Run on the same cat: %v, want the single-instance refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second daemon is running on the same cat; want refusal")
	}
	_ = first
}

// TestDialerMarksTargetSeenOnHelloReply pins symmetric liveness: a
// dialer marks its target seen the moment the target's hello reply
// lands — the same point the listener marks the dialer — so a
// connection cut before the roster exchange completes still counts
// both ways (the target was provably alive on the wire).
func TestDialerMarksTargetSeenOnHelloReply(t *testing.T) {
	milo := startDaemon(t, "milo")

	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	pc := protocol.NewConn(server)
	far := protocol.NewConn(client)

	const targetKey = "nodekey:target-seen"
	go func() {
		// Reply to the hello, then vanish before the roster
		// exchange: the exchange fails, but liveness is proven.
		if _, err := far.ReadMsg(); err == nil {
			if err := far.WriteMsg(&protocol.Message{Hello: &protocol.Hello{Name: "niko", Key: targetKey}}); err == nil {
				_ = far.Close()
			}
		}
	}()

	if err := milo.handshakeClient(pc, msgTimeout); err == nil {
		t.Fatal("handshakeClient succeeded against a vanishing peer, want an error")
	}
	if milo.SeenAt(targetKey) == 0 {
		t.Fatal("dialer did not mark the target seen after its hello reply; a connection cut before the roster exchange lost liveness")
	}
}

func TestForgetClearsOutbox(t *testing.T) {
	milo := startDaemon(t, "milo")
	niko, _ := offlineCat(t)
	addCat(t, milo, niko)

	src := writeSource(t, "doomed nap")
	if _, err := milo.Send("niko", src); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(milo.ob.All()) == 1 }, "outbox to hold the send")

	if _, ok := milo.Forget("niko"); !ok {
		t.Fatal("Forget failed")
	}
	if got := len(milo.ob.All()); got != 0 {
		t.Fatalf("outbox has %d entries after forget, want 0", got)
	}
	if _, ok := milo.Roster().Get("niko"); ok {
		t.Fatal("roster still has niko after forget")
	}
}

func TestStorerRefusesWhenNotStorer(t *testing.T) {
	milo := startDaemon(t, "milo")
	picky := startDaemon(t, "picky") // not a storer
	trust(t, milo, picky)
	trust(t, picky, milo)

	niko, _ := offlineCat(t)
	addCat(t, milo, niko)
	addCat(t, picky, niko)

	src := writeSource(t, "nobody will take this")
	if _, err := milo.Send("niko", src); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Give delivery attempts a moment, then verify nothing was stored
	// and the file is still pending.
	time.Sleep(500 * time.Millisecond)
	if picky.Spool().Count() != 0 {
		t.Fatal("non-storer spooled a file")
	}
	if len(milo.ob.All()) != 1 {
		t.Fatalf("outbox has %d entries, want 1 (still pending)", len(milo.ob.All()))
	}
}

func TestIPCRoundTrip(t *testing.T) {
	d := startDaemon(t, "milo")
	sock := IPCPath(d.cfg.Dir)

	waitFor(t, func() bool {
		if _, err := os.Stat(sock); err != nil {
			return false
		}
		return true
	}, "IPC socket to exist")

	// The socket must be user-only: any local process could otherwise
	// drive the daemon (including rotating its identity).
	if fi, err := os.Stat(sock); err == nil {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("IPC socket permissions are %o, want 0600", perm)
		}
	}

	resp, err := CallIPC(sock, Request{Op: "cats"})
	if err != nil {
		t.Fatalf("CallIPC: %v", err)
	}
	if !resp.OK || resp.Me == nil || resp.Me.Name != "milo" {
		t.Fatalf("cats = %+v", resp)
	}

	resp, err = CallIPC(sock, Request{Op: "storer", On: true, Max: "1G"})
	if err != nil || !resp.OK {
		t.Fatalf("storer on: %v, %+v", err, resp)
	}
	if !d.Me().Storer {
		t.Fatal("storer flag not set on the daemon")
	}

	resp, err = CallIPC(sock, Request{Op: "status"})
	if err != nil || !resp.OK || resp.Spool != 0 {
		t.Fatalf("status: %v, %+v", err, resp)
	}
}

func TestFileDigestMatchesContent(t *testing.T) {
	p := writeSource(t, "hello")
	got, size, err := fileDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	if size != 5 {
		t.Fatalf("size = %d, want 5", size)
	}
	// sha256 of "hello"
	if got != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("digest = %s", got)
	}
}

var _ = io.Discard // keep io import if unused by future edits
