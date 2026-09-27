package roster

import (
	"crypto/ed25519"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"
)

// signPair makes a cat entry and the Ed25519 keypair its owner signs with.
func signCat(t *testing.T, c Cat) (Cat, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SignKey = hex.EncodeToString(pub)
	return SignCat(priv, c), priv
}

func TestMergeRequiresValidSignatureForKnownKeys(t *testing.T) {
	r := New()
	signed, priv := signCat(t, cat(t, "milo"))
	if err := r.Add(signed); err != nil {
		t.Fatal(err)
	}

	// A newer but unsigned update for a key with a pinned sign key is
	// rejected: any trusted cat can no longer inject or override entries.
	unsigned := signed
	unsigned.Name = "hijacked"
	unsigned.Updated = signed.Updated + 10
	if changed, _ := r.Merge([]Cat{unsigned}); len(changed) != 0 {
		t.Fatalf("unsigned update for known key merged: %v", changed)
	}

	// The same update under a different sign key is rejected too.
	forged, _ := signCat(t, unsigned)
	if changed, _ := r.Merge([]Cat{forged}); len(changed) != 0 {
		t.Fatalf("entry signed with a foreign sign key merged: %v", changed)
	}

	// A newer update properly signed by the owner lands.
	owned := SignCat(priv, unsigned)
	if _, err := r.Merge([]Cat{owned}); err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Get("hijacked"); !ok || got.Key != signed.Key {
		t.Fatalf("signed update not applied: %v, %v", got, ok)
	}

	// A tampered signature is rejected: mutate the name after signing.
	tampered := owned
	tampered.Name = "tampered"
	tampered.Updated = owned.Updated + 10
	if changed, _ := r.Merge([]Cat{tampered}); len(changed) != 0 {
		t.Fatalf("tampered entry merged: %v", changed)
	}
}

func TestMergePinsSignKeyOfUnknownCats(t *testing.T) {
	r := New()
	signed, _ := signCat(t, cat(t, "milo"))
	if _, err := r.Merge([]Cat{signed}); err != nil {
		t.Fatal(err)
	}
	// Now known and pinned: a later foreign-signed update is rejected.
	foreign, _ := signCat(t, signed)
	foreign.Updated = signed.Updated + 10
	if changed, _ := r.Merge([]Cat{foreign}); len(changed) != 0 {
		t.Fatalf("foreign-signed update merged for pinned key: %v", changed)
	}
}

func TestTombstoneDropsEntryAndOutranksReadd(t *testing.T) {
	r := New()
	signed, priv := signCat(t, cat(t, "milo"))
	if err := r.Add(signed); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Unix()
	tomb := SignTombstone(priv, signed.Key, at)

	// A forged tombstone (wrong key) is ignored.
	_, otherPriv := signCat(t, signed)
	applied := r.ApplyTombstones([]Tombstone{SignTombstone(otherPriv, signed.Key, at)})
	if len(applied) != 0 {
		t.Fatalf("foreign tombstone applied: %v", applied)
	}
	if _, ok := r.GetByKey(signed.Key); !ok {
		t.Fatal("entry dropped by a forged tombstone")
	}

	// The real tombstone drops the entry.
	applied = r.ApplyTombstones([]Tombstone{tomb})
	if len(applied) != 1 || applied[0].Key != signed.Key {
		t.Fatalf("ApplyTombstones = %v", applied)
	}
	if _, ok := r.GetByKey(signed.Key); ok {
		t.Fatal("leaver still in roster after tombstone")
	}
	if len(r.Tombstones()) != 1 {
		t.Fatalf("tombstone not recorded: %v", r.Tombstones())
	}

	// A later re-add of the leaver — even signed by the leaver, but not
	// newer than the leave — never resurrects the entry.
	oldSigned := signed
	oldSigned.Updated = at // equal to the tombstone: not newer
	oldSigned = SignCat(priv, oldSigned)
	if changed, _ := r.Merge([]Cat{oldSigned}); len(changed) != 0 {
		t.Fatalf("re-add at tombstone time merged: %v", changed)
	}

	// A signed entry newer than the leave is the leaver rejoining:
	// only the leaver's own sign key can say that, so it wins.
	rejoined := signed
	rejoined.Updated = at + 1
	rejoined = SignCat(priv, rejoined)
	if _, err := r.Merge([]Cat{rejoined}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.GetByKey(signed.Key); !ok {
		t.Fatal("signed rejoin newer than the leave was refused")
	}
	if len(r.Tombstones()) != 0 {
		t.Fatalf("tombstone outlived the rejoin: %v", r.Tombstones())
	}
}

func TestApplyTombstonesSkipsStaleTombstoneAfterRejoin(t *testing.T) {
	r := New()
	signed, priv := signCat(t, cat(t, "milo"))
	if err := r.Add(signed); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Unix()
	tomb := SignTombstone(priv, signed.Key, at)
	if got := r.ApplyTombstones([]Tombstone{tomb}); len(got) != 1 {
		t.Fatalf("tombstone not applied: %v", got)
	}

	// The leaver rejoined via pairing: a fresh signed entry, a cleared
	// tombstone. A straggler peer re-sends the old tombstone: it must
	// not undo the rejoin.
	rejoined := signed
	rejoined.Updated = at + 5
	rejoined = SignCat(priv, rejoined)
	if err := r.Add(rejoined); err != nil {
		t.Fatal(err)
	}
	r.ClearTombstone(signed.Key)
	if got := r.ApplyTombstones([]Tombstone{tomb}); len(got) != 0 {
		t.Fatalf("stale tombstone re-applied: %v", got)
	}
	if _, ok := r.GetByKey(signed.Key); !ok {
		t.Fatal("stale tombstone dropped a rejoined cat")
	}
}

func TestApplyTombstonesNeedsAPinForUnknownKeys(t *testing.T) {
	r := New()
	_, priv := signCat(t, cat(t, "milo"))
	tomb := SignTombstone(priv, "nodekey:nobody", time.Now().Unix())
	if got := r.ApplyTombstones([]Tombstone{tomb}); len(got) != 0 {
		t.Fatalf("tombstone for an unknown, unpinned key applied: %v", got)
	}
}

func TestTombstonePersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster.json")
	r := New()
	if err := r.SetPath(path); err != nil {
		t.Fatal(err)
	}
	signed, priv := signCat(t, cat(t, "milo"))
	if err := r.Add(signed); err != nil {
		t.Fatal(err)
	}
	tomb := SignTombstone(priv, signed.Key, time.Now().Unix())
	if got := r.ApplyTombstones([]Tombstone{tomb}); len(got) != 1 {
		t.Fatalf("tombstone not applied: %v", got)
	}

	r2 := New()
	if err := r2.SetPath(path); err != nil {
		t.Fatal(err)
	}
	if got := r2.Tombstones(); len(got) != 1 || got[0].Key != signed.Key {
		t.Fatalf("tombstone did not survive a reload: %v", got)
	}
	// The re-add guard survives the reload too.
	stale := signed
	stale.Updated = tomb.Time + 1
	if changed, _ := r2.Merge([]Cat{stale}); len(changed) != 0 {
		t.Fatalf("unsigned re-add merged after reload: %v", changed)
	}
}

func TestResetClearsRosterAndTombstones(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster.json")
	r := New()
	if err := r.SetPath(path); err != nil {
		t.Fatal(err)
	}
	signed, priv := signCat(t, cat(t, "milo"))
	if err := r.Add(signed); err != nil {
		t.Fatal(err)
	}
	r.ApplyTombstones([]Tombstone{SignTombstone(priv, "nodekey:gone", time.Now().Unix())})
	if err := r.Reset(); err != nil {
		t.Fatal(err)
	}
	if len(r.All()) != 0 || len(r.Tombstones()) != 0 {
		t.Fatal("Reset left entries or tombstones behind")
	}
	r2 := New()
	if err := r2.SetPath(path); err != nil {
		t.Fatal(err)
	}
	if len(r2.All()) != 0 || len(r2.Tombstones()) != 0 {
		t.Fatal("Reset did not persist the wipe")
	}
}
