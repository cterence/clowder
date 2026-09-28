package daemon

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"clowder/store"
)

// leaveTopology builds a-b-c, with b in the middle: a knows only b,
// c knows only b, so anything a says reaches c only through b's
// re-broadcast or the tombstones riding b's roster sync.
func TestLeaveDropsLeaverAndRebroadcasts(t *testing.T) {
	a := startDaemon(t, "a")
	b := startDaemon(t, "b")
	c := startDaemon(t, "c")
	trust(t, a, b)
	trust(t, b, a)
	trust(t, b, c)
	trust(t, c, b)
	aKey := a.Me().Key
	waitFor(t, func() bool { _, ok := c.Roster().GetByKey(aKey); return ok },
		"c to learn a via b's sync")

	// b holds a file for a and owes a a send: both must die with the leave.
	if err := b.spool.Put(store.Meta{ID: "sp1", FileName: "nap.txt", Size: 3,
		TargetKey: aKey, TargetName: "a", From: "b"}, strings.NewReader("nap")); err != nil {
		t.Fatal(err)
	}
	if err := b.ob.Put(Entry{ID: "ob1", TargetKey: aKey, TargetName: "a",
		FileName: "nap.txt", SourcePath: writeSource(t, "nap")}); err != nil {
		t.Fatal(err)
	}

	n, err := a.Leave()
	if err != nil {
		t.Fatal(err)
	}
	// The announce is synchronous: both b and c answer, and a cat cut
	// at the 3s deadline (none here) would still learn via the
	// re-broadcast path pinned by TestLeaveReachesOfflinePeerViaSync.
	if n < 1 {
		t.Fatalf("leave announced to %d cats, want at least 1", n)
	}

	waitFor(t, func() bool { _, ok := b.Roster().GetByKey(aKey); return !ok },
		"b to drop the leaver")
	// c only hears the leave through b's re-broadcast.
	waitFor(t, func() bool { _, ok := c.Roster().GetByKey(aKey); return !ok },
		"c to drop the leaver via b's re-broadcast")
	// Roster-drop and spool/outbox cleanup are one applyTombstone, but
	// the cleanup is its tail: wait for it, don't assume zero lag.
	waitFor(t, func() bool {
		if len(b.spool.List(aKey)) != 0 {
			return false
		}
		for _, e := range b.ob.All() {
			if e.TargetKey == aKey {
				return false
			}
		}
		return b.isBlockedKey(aKey)
	}, "b to drop the leaver's held file, pending send and allow")

	// The leaver's own roster is wiped and stays wiped while b keeps
	// syncing to it on every poll tick.
	if len(a.Roster().All()) != 0 {
		t.Fatal("leaver kept its roster")
	}
	time.Sleep(500 * time.Millisecond)
	if len(a.Roster().All()) != 0 {
		t.Fatal("b's syncs repopulated the leaver's roster")
	}
}

// A peer that was offline through the whole leave learns it on wake:
// the tombstone rides the roster sync, and the stale entry the sleeper
// pushes back must not resurrect the leaver anywhere.
func TestLeaveReachesOfflinePeerViaSync(t *testing.T) {
	a := startDaemon(t, "a")
	b := startDaemon(t, "b")
	cDir := t.TempDir()
	if err := Init(cDir, "c"); err != nil {
		t.Fatal(err)
	}
	c := startDaemonAt(t, cDir)
	trust(t, a, b)
	trust(t, b, a)
	trust(t, b, c)
	trust(t, c, b)
	aKey := a.Me().Key
	waitFor(t, func() bool { _, ok := c.Roster().GetByKey(aKey); return ok },
		"c to learn a via b's sync")
	stopDaemon(c)

	if _, err := a.Leave(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, ok := b.Roster().GetByKey(aKey); return !ok },
		"b to drop the leaver")

	// The same cat wakes up: its sync with b must deliver the
	// tombstone, not a resurrection.
	woken := startDaemonAt(t, cDir)
	waitFor(t, func() bool { _, ok := woken.Roster().GetByKey(aKey); return !ok },
		"woken c to drop the leaver")
	time.Sleep(300 * time.Millisecond)
	if _, ok := woken.Roster().GetByKey(aKey); ok {
		t.Fatal("leaver resurrected on the woken cat")
	}
	if _, ok := b.Roster().GetByKey(aKey); ok {
		t.Fatal("leaver resurrected on b")
	}
}

// Leave waits only for cats that answer: a wedged peer is cut at
// leaveTimeout, and when nobody at all answers the leave fails and
// wipes nothing, so the command can simply be retried.
func TestLeaveBoundedByWedgedPeer(t *testing.T) {
	old := leaveTimeout
	leaveTimeout = 200 * time.Millisecond
	t.Cleanup(func() { leaveTimeout = old })

	a := startDaemon(t, "a")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // accepted but never answered
			mu.Unlock()
		}
	}()
	wedged, _ := offlineCat(t)
	wedged.Addr = ln.Addr().String()
	addCat(t, a, wedged)

	start := time.Now()
	if _, err := a.Leave(); err == nil {
		t.Fatal("leave with only a wedged peer succeeded, want failure (nobody heard it)")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("leave took %s with a wedged peer, want it cut at leaveTimeout", elapsed)
	}
	// Nobody heard the leave: local state must be untouched so the
	// command can be retried.
	if _, ok := a.Roster().GetByKey(wedged.Key); !ok {
		t.Fatal("leave wiped the roster even though nobody heard it")
	}
}

// The IPC op the clow CLI drives.
func TestLeaveIPCOp(t *testing.T) {
	a := startDaemon(t, "a")
	b := startDaemon(t, "b")
	trust(t, a, b)
	trust(t, b, a)
	resp := a.handleIPC(Request{Op: "leave"})
	if !resp.OK {
		t.Fatalf("leave op: %+v", resp)
	}
	waitFor(t, func() bool { _, ok := b.Roster().GetByKey(a.Me().Key); return !ok },
		"b to drop the leaver")
}
