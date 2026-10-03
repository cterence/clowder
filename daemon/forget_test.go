package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/cterence/clowder/protocol"
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

	// b still has c and pushes its roster on every sync round; drive one
	// directly — syncPeers returning means a merged the push — so the
	// blocklist refusal is observed, not slept past.
	b.syncPeers(context.Background())
	if _, ok := a.Roster().GetByKey(cKey); ok {
		t.Fatal("forgotten cat was re-merged by a peer's roster sync")
	}
}

// Re-pairing is the way back from forget: pairing is the trust root, so
// it must clear the blocklist, or the re-paired cat's connections stay
// refused forever (with trust/distrust gone there is no other unblock).
func TestRepairAfterForgetUnblocks(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	if _, ok := milo.Forget("fluff"); !ok {
		t.Fatal("Forget(fluff) failed")
	}
	if !milo.isBlockedKey(fluff.Me().Key) {
		t.Fatal("forgotten cat's key is not blocked")
	}

	if err := milo.addPeerCat(&protocol.PairIntro{
		Name:    fluff.Me().Name,
		Addr:    string(fluff.env.Identity.Public.Addr()),
		DialKey: fluff.Me().DialKey,
		SignKey: fluff.Me().SignKey,
	}); err != nil {
		t.Fatalf("addPeerCat: %v", err)
	}
	if milo.isBlockedKey(fluff.Me().Key) || milo.isBlockedKey(fluff.Me().DialKey) {
		t.Fatal("re-pairing did not clear the blocklist")
	}
}

// The short key `clow status` prints (8 hex chars) is a valid forget
// handle: same disambiguation as the full key, without copy-pasting
// the whole thing.
func TestForgetKeyPrefixMatchesStatusDisplay(t *testing.T) {
	milo := startDaemon(t, "milo")
	one, _ := offlineCat(t)
	two, _ := offlineCat(t)
	two.Name = one.Name
	addCat(t, milo, one)
	addCat(t, milo, two)

	prefix := strings.TrimPrefix(two.Key, "nodekey:")[:8]
	if got, ok := milo.Forget(prefix); !ok || got.Key != two.Key {
		t.Fatalf("Forget(by prefix %s) = (%s, %v), want cat %s", prefix, got.Name, ok, two.Key)
	}
	if _, ok := milo.Roster().GetByKey(two.Key); ok {
		t.Fatal("prefix-forgotten cat is still in the roster")
	}
	if _, ok := milo.Roster().GetByKey(one.Key); !ok {
		t.Fatal("the other same-named cat was removed too")
	}
	if !milo.isBlockedKey(two.Key) || milo.isBlockedKey(one.Key) {
		t.Fatal("blocklist does not match the prefix-forgotten cat")
	}
}

// Two cats claiming one name: forgetting by key removes exactly one,
// the key being the only unambiguous handle.
func TestForgetKeyDisambiguatesDuplicateName(t *testing.T) {
	milo := startDaemon(t, "milo")
	one, _ := offlineCat(t)
	two, _ := offlineCat(t)
	two.Name = one.Name
	addCat(t, milo, one)
	addCat(t, milo, two)
	if got := milo.Roster().Duplicates(); len(got) != 1 || got[0] != one.Name {
		t.Fatalf("Duplicates() = %v, want [%s]", got, one.Name)
	}

	if _, ok := milo.Forget(two.Key); !ok {
		t.Fatal("Forget(by key) failed")
	}
	if _, ok := milo.Roster().GetByKey(two.Key); ok {
		t.Fatal("forgotten-by-key cat is still in the roster")
	}
	if _, ok := milo.Roster().GetByKey(one.Key); !ok {
		t.Fatal("the other same-named cat was removed too")
	}
	if !milo.isBlockedKey(two.Key) || milo.isBlockedKey(one.Key) {
		t.Fatal("blocklist does not match the forgotten cat")
	}
}
