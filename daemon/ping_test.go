package daemon

import (
	"strings"
	"testing"
	"time"
)

// Ping is a real authenticated round trip: both cats end up marked
// live (the handshake marks liveness symmetrically), the unknown-name
// refusal matches forget's, and the key prefix status displays is a
// valid handle too.
func TestPing(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	resp := milo.handleIPC(Request{Op: "ping", Target: "fluff"})
	if !resp.OK {
		t.Fatalf("ping(fluff) failed: %s", resp.Error)
	}
	if !strings.Contains(resp.Message, "fluff") {
		t.Fatalf("ping message = %q, want it to name the cat", resp.Message)
	}
	if milo.SeenAt(fluff.Me().Key) == 0 {
		t.Fatal("ping did not mark fluff live on milo")
	}
	if fluff.SeenAt(milo.Me().Key) == 0 {
		t.Fatal("ping did not mark milo live on fluff (liveness is symmetric)")
	}

	prefix := strings.TrimPrefix(fluff.Me().Key, "nodekey:")[:8]
	if resp := milo.handleIPC(Request{Op: "ping", Target: prefix}); !resp.OK {
		t.Fatalf("ping(key prefix %s) failed: %s", prefix, resp.Error)
	}

	if resp := milo.handleIPC(Request{Op: "ping", Target: "nobody"}); resp.OK {
		t.Fatal("ping of an unknown cat succeeded")
	}
	if resp := milo.handleIPC(Request{Op: "ping"}); resp.OK {
		t.Fatal("ping without a target succeeded")
	}
}

// A ping to a cat that is gone must fail in bounded time, not hang
// the CLI for the IPC deadline.
func TestPingBoundedOffline(t *testing.T) {
	milo := startDaemon(t, "milo")
	one, addr := offlineCat(t) // never listens
	one.Addr = addr
	addCat(t, milo, one)

	start := time.Now()
	resp := milo.handleIPC(Request{Op: "ping", Target: one.Name})
	if resp.OK {
		t.Fatal("ping of a cat that never listens succeeded")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("offline ping took %s, want it bounded well under the IPC deadline", elapsed)
	}
}
