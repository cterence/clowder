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
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"

	"clowder/protocol"
	"clowder/roster"
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

// authedLocalTransport makes loopback connections claim a client key
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
// authenticated client key may claim its own roster identity, and
// nothing else — a fake Key claim must not enter the liveness map.
func TestServeConnMarksOnlyKnownIdentities(t *testing.T) {
	cedar := startDaemon(t, "cedar")
	clientKey, err := parseKey(cedar.Me().ClientKey)
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
		Name: "cedar", Key: cedar.Me().Key, ClientKey: cedar.Me().ClientKey,
		Addr: cedar.Me().Addr, Version: protocol.HelloVersion,
	})
	waitFor(t, func() bool {
		_, ok := milo.livenessSnapshot()[cedar.Me().Key]
		return ok
	}, "honest hello to mark liveness")

	ghost := blockedCat(t, "ghost", time.Now().Unix())
	sayHello(t, milo, &protocol.Hello{ // forged identity claim
		Name: "cedar", Key: ghost.Key, ClientKey: cedar.Me().ClientKey,
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
