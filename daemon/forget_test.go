package daemon

import (
	"crypto/ed25519"
	"testing"
	"time"

	"clowder/protocol"
	"clowder/roster"
)

// Forget is a local kick-out: the entry must stay out even while
// peers that still know the cat keep syncing its (signed) entry to
// us — the blocklist must refuse roster entries, not just
// connections.
func TestForgottenCatStaysOutDespiteSync(t *testing.T) {
	a := startDaemon(t, "a")
	b := startDaemon(t, "b")
	c := startDaemon(t, "c")
	trust(t, a, b)
	trust(t, b, a)
	trust(t, b, c)
	trust(t, c, b)
	cKey := c.Me().Key
	waitFor(t, func() bool { _, ok := a.Roster().GetByKey(cKey); return ok },
		"a to learn c via b's sync")

	if _, ok := a.Forget("c"); !ok {
		t.Fatal("Forget(c) failed")
	}
	waitFor(t, func() bool { _, ok := a.Roster().GetByKey(cKey); return !ok },
		"a to drop the forgotten cat")

	// b keeps syncing its roster (which still has c) to a on every
	// poll tick — the cat must not come back.
	time.Sleep(600 * time.Millisecond)
	if _, ok := a.Roster().GetByKey(cKey); ok {
		t.Fatal("forgotten cat was re-merged by a peer's roster sync")
	}
}

// Distrust keeps the entry but blocks the cat: its entry must stop
// tracking updates too, and trust must resume them.
func TestDistrustedCatEntryFreezesUntilTrust(t *testing.T) {
	a := startDaemon(t, "a")
	b := startDaemon(t, "b")
	c := startDaemon(t, "c")
	trust(t, a, b)
	trust(t, b, a)
	trust(t, b, c)
	trust(t, c, b)
	cKey := c.Me().Key
	waitFor(t, func() bool { _, ok := a.Roster().GetByKey(cKey); return ok },
		"a to learn c via b's sync")

	if _, err := a.Distrust("c"); err != nil {
		t.Fatalf("Distrust(c): %v", err)
	}
	// A newer signed update for the distrusted key arriving via sync
	// must not replace the frozen entry: build one from c's current
	// entry with a bumped name.
	frozen, _ := a.Roster().GetByKey(cKey)
	update := frozen
	update.Name = "renamed-c"
	update.Updated = frozen.Updated + 10
	update = roster.SignCat(cSignPriv(t, c), update)
	a.mergeRemote(&protocol.RosterSync{Cats: []roster.Cat{update}})
	got, _ := a.Roster().GetByKey(cKey)
	if got.Name != frozen.Name {
		t.Fatalf("distrusted cat's entry tracked an update: %q", got.Name)
	}

	// Trust resumes normal merging.
	if _, err := a.Trust("c"); err != nil {
		t.Fatalf("Trust(c): %v", err)
	}
	a.mergeRemote(&protocol.RosterSync{Cats: []roster.Cat{update}})
	got, _ = a.Roster().GetByKey(cKey)
	if got.Name != "renamed-c" {
		t.Fatalf("trusted cat did not resume merging updates: %q", got.Name)
	}
}

// cSignPriv derives the daemon-under-test's Ed25519 sign key the way
// Open does, for signing third-party roster updates in tests.
func cSignPriv(t *testing.T, d *Daemon) ed25519.PrivateKey {
	t.Helper()
	raw := d.env.Identity.Private.Raw32()
	return ed25519.NewKeyFromSeed(raw[:])
}
