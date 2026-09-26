package daemon

import (
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
