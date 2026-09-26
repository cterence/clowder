package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tailscale/tailcat"

	"clowder/roster"
)

// blockedCat builds a roster entry with a fresh identity for the merge
// clock tests.
func blockedCat(t *testing.T, name string, updated int64) roster.Cat {
	t.Helper()
	k := tailcat.NewPrivateKey()
	c, err := roster.NewCat(name, string(k.Public.Addr()), updated)
	if err != nil {
		t.Fatalf("NewCat: %v", err)
	}
	return c
}

func TestMergeRejectsFutureTimestamps(t *testing.T) {
	r := roster.New()
	a := blockedCat(t, "a", time.Now().Add(1*time.Hour).Unix()) // impossible clock
	changed, err := r.Merge([]roster.Cat{a})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatal("far-future timestamp was merged, want rejected")
	}
	if _, ok := r.Get("a"); ok {
		t.Fatal("far-future entry is in the roster")
	}

	// A modest skew within the tolerance still applies.
	b := blockedCat(t, "b", time.Now().Add(2*time.Minute).Unix())
	if _, err := r.Merge([]roster.Cat{b}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("b"); !ok {
		t.Fatal("entry within clock skew was rejected")
	}
}

func TestForgetBlocksReconnect(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	// Sanity: fluff can send to milo before the forget.
	src := writeSource(t, "hello first")
	if _, err := fluff.Send("milo", src); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, ok := inboxFile(t, milo, "nap.txt")
		return ok
	}, "the first send to arrive")

	// milo forgets fluff: fluff's connections must now be refused even
	// though the tailcat allowlist cannot drop its key.
	if _, ok := milo.Forget("fluff"); !ok {
		t.Fatal("Forget failed")
	}
	if !milo.isBlockedKey(fluff.Me().Key) {
		t.Fatal("forgotten cat's key is not blocked")
	}

	blocked := writeSource(t, "should not arrive")
	if _, err := fluff.Send("milo", blocked); err != nil {
		t.Fatal(err)
	}
	// The blocked attempt must stay queued on fluff and never deliver.
	waitFor(t, func() bool {
		for _, e := range fluff.ob.All() {
			if e.TargetKey == milo.Me().Key {
				return true
			}
		}
		return false
	}, "the send to the blocking cat to stay queued")
	entries, err := os.ReadDir(milo.InboxDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox has %d files after the block, want 1 (no new arrival)", len(entries))
	}

	// The blocklist survives a restart.
	milo2 := startDaemonAt(t, milo.cfg.Dir)
	if !milo2.isBlockedKey(fluff.Me().Key) {
		t.Fatal("blocklist did not survive restart")
	}
}

func TestDistrustRefusesBothWays(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	// Sanity: a send works before the distrust.
	src := writeSource(t, "hello first")
	if _, err := fluff.Send("milo", src); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, ok := inboxFile(t, milo, "nap.txt")
		return ok
	}, "the first send to arrive")

	// milo distrusts fluff: the roster entry stays, both keys are
	// blocked, and milo's own sends to fluff refuse.
	if _, err := milo.Distrust("fluff"); err != nil {
		t.Fatalf("Distrust: %v", err)
	}
	if _, ok := milo.Roster().Get("fluff"); !ok {
		t.Fatal("distrust removed the roster entry, want it kept")
	}
	if !milo.isBlockedKey(fluff.Me().Key) || !milo.isBlockedKey(fluff.Me().ClientKey) {
		t.Fatal("distrusted cat's keys are not both blocked")
	}
	if got := milo.distrustedNames(); len(got) != 1 || got[0] != "fluff" {
		t.Fatalf("distrustedNames() = %v, want [fluff]", got)
	}
	if _, err := milo.Send("fluff", src); err == nil {
		t.Fatal("Send to a distrusted cat succeeded")
	}

	// Inbound: fluff's connection is refused, its send stays queued.
	blocked := writeSource(t, "should not arrive yet")
	if _, err := fluff.Send("milo", blocked); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, e := range fluff.ob.All() {
			if e.TargetKey == milo.Me().Key {
				return true
			}
		}
		return false
	}, "the send to the distrusting cat to stay queued")

	// Trust undoes it: the queued send delivers on its next retry.
	if _, err := milo.Trust("fluff"); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	if milo.isBlockedKey(fluff.Me().Key) {
		t.Fatal("Trust did not unblock")
	}
	waitFor(t, func() bool {
		_, ok := inboxFile(t, milo, "nap-1.txt")
		return ok
	}, "the queued send to deliver after trust")
}

func TestDistrustSkipsStorerRelay(t *testing.T) {
	milo := startDaemon(t, "milo")
	box := startDaemon(t, "box")
	if err := box.SetStorer(true, 10<<20); err != nil {
		t.Fatal(err)
	}
	niko, _ := offlineCat(t)
	trust(t, milo, box)
	addCat(t, milo, niko)

	// milo distrusts the only storer: sends must stay queued rather
	// than relay through it.
	if _, err := milo.Distrust("box"); err != nil {
		t.Fatalf("Distrust: %v", err)
	}
	src := writeSource(t, "no relay for you")
	if _, err := milo.Send("niko", src); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, e := range milo.ob.All() {
			if e.TargetKey == niko.Key {
				return true
			}
		}
		return false
	}, "the send to stay queued")
	if box.Spool().Count() != 0 {
		t.Fatalf("distrusted storer holds %d files, want 0", box.Spool().Count())
	}
}

func TestRelayedOfferFromDistrustedSenderRefused(t *testing.T) {
	milo := startDaemon(t, "milo")
	box := startDaemon(t, "box")
	if err := box.SetStorer(true, 10<<20); err != nil {
		t.Fatal(err)
	}
	niko, nikoDir := offlineCat(t)
	trust(t, milo, box)
	addCat(t, milo, niko)
	addCat(t, box, niko)

	// milo's file rides the storer while niko is offline.
	src := writeSource(t, "held for a cat that distrusts the sender")
	if _, err := milo.Send("niko", src); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return box.Spool().Count() == 1 },
		"the storer to hold the file")

	// niko wakes up and distrusts milo: the storer (not distrusted)
	// can connect, but the offer names a distrusted sender and must
	// be refused — the file stays held, the inbox stays empty.
	nikoD := startDaemonAt(t, nikoDir)
	addCat(t, nikoD, milo.Me()) // niko must know milo to distrust it by name
	trust(t, nikoD, box)
	trust(t, box, nikoD) // box learns niko's live address; its sweep pushes
	if _, err := nikoD.Distrust("milo"); err != nil {
		t.Fatalf("Distrust: %v", err)
	}
	nikoD.Poll(context.Background())
	waitFor(t, func() bool { return box.Spool().Count() == 1 },
		"the refused file to stay held")
	des, err := os.ReadDir(nikoD.InboxDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(des) != 0 {
		t.Fatalf("inbox has %d files, want 0", len(des))
	}
}
