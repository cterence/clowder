package daemon

import (
	"net"
	"testing"
	"time"
)

// TestEvictIdleSkipsInFlight pins the eviction guard: an idle client
// is reclaimed, but one with an open dial survives the idle window (a
// streaming transfer outlives it), and closing the dial releases the
// address again.
func TestEvictIdleSkipsInFlight(t *testing.T) {
	tr := &TailcatTransport{}
	tr.clientFor("peer") // creates the cached client

	// Idle with no open dial: reclaimed.
	tr.mu.Lock()
	tr.lastUse["peer"] = time.Now().Add(-time.Hour)
	tr.mu.Unlock()
	tr.evictIdle(time.Minute)
	if _, ok := tr.clients["peer"]; ok {
		t.Fatal("idle client not evicted")
	}

	// Idle but with an open dial: survives.
	tr.clientFor("peer")
	tr.mu.Lock()
	tr.lastUse["peer"] = time.Now().Add(-time.Hour)
	tr.inflight["peer"]++
	tr.mu.Unlock()
	tr.evictIdle(time.Minute)
	if _, ok := tr.clients["peer"]; !ok {
		t.Fatal("evicted a client with an open dial")
	}

	// Closing the dial releases the address and the client is
	// reclaimed again.
	c1, c2 := net.Pipe()
	dc := &dialConn{Conn: c1, t: tr, addr: "peer"}
	_ = c2.Close()
	if err := dc.Close(); err != nil {
		t.Fatal(err)
	}
	tr.mu.Lock()
	if n := tr.inflight["peer"]; n != 0 {
		t.Fatalf("inflight = %d, want 0", n)
	}
	tr.lastUse["peer"] = time.Now().Add(-time.Hour)
	tr.mu.Unlock()
	tr.evictIdle(time.Minute)
	if _, ok := tr.clients["peer"]; ok {
		t.Fatal("client not evicted after the dial closed")
	}
}
