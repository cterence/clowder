package daemon

import (
	"context"
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

	"clowder/roster"
)

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

func startDaemon(t *testing.T, name string) *Daemon {
	t.Helper()
	dir := t.TempDir()
	if err := Init(dir, name); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return startDaemonAt(t, dir)
}

// startDaemonAt runs a daemon on a config dir that Init (or a sleeping
// offline cat) already prepared.
func startDaemonAt(t *testing.T, dir string) *Daemon {
	t.Helper()
	// Keep the default inbox (under $HOME/Downloads/clowder) inside the
	// test sandbox.
	t.Setenv("HOME", t.TempDir())
	cfg := Config{
		Dir:        dir,
		RetryEvery: 150 * time.Millisecond,
		PollEvery:  150 * time.Millisecond,
		Logf:       t.Logf,
	}
	d, err := New(cfg, &LocalTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = d.Run(ctx) }()
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
// cat.
func offlineCat(t *testing.T, name string) (roster.Cat, string) {
	t.Helper()
	dir := t.TempDir()
	if err := Init(dir, name); err != nil {
		t.Fatalf("Init: %v", err)
	}
	env, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return roster.Cat{
		Name:    name,
		Addr:    "127.0.0.1:1", // nothing listens here
		Key:     env.Identity.Public.ServerPublic.String(),
		Updated: time.Now().Unix(),
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
	if err := storer.SetStorer(true); err != nil {
		t.Fatal(err)
	}
	trust(t, milo, storer)
	trust(t, storer, milo)

	// A target cat that is offline (never started), known to milo and
	// the storer by identity.
	niko, nikoDir := offlineCat(t, "niko")
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

	// Now niko wakes up — same persisted identity — and fetches from
	// the storer.
	nikoD := startDaemonAt(t, nikoDir)
	trust(t, nikoD, storer)
	trust(t, storer, nikoD)
	nikoD.Poll(context.Background())

	waitFor(t, func() bool {
		got, ok := inboxFile(t, nikoD, "nap.txt")
		return ok && got == "nap for a sleeping cat"
	}, "niko to fetch the held file")
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

func TestForgetClearsOutbox(t *testing.T) {
	milo := startDaemon(t, "milo")
	niko, _ := offlineCat(t, "niko")
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

	niko, _ := offlineCat(t, "niko")
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

	resp, err := CallIPC(sock, Request{Op: "cats"})
	if err != nil {
		t.Fatalf("CallIPC: %v", err)
	}
	if !resp.OK || resp.Me == nil || resp.Me.Name != "milo" {
		t.Fatalf("cats = %+v", resp)
	}

	resp, err = CallIPC(sock, Request{Op: "storer", On: true})
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
