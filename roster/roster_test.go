package roster

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tailscale/tailcat"
)

func cat(t *testing.T, name string) Cat {
	t.Helper()
	k := tailcat.NewPrivateKey()
	c, err := NewCat(name, string(k.Public.Addr()), 100)
	if err != nil {
		t.Fatalf("NewCat: %v", err)
	}
	return c
}

func TestNewCatDerivesKeyFromAddr(t *testing.T) {
	k := tailcat.NewPrivateKey()
	c, err := NewCat("fluff", string(k.Public.Addr()), 1)
	if err != nil {
		t.Fatalf("NewCat: %v", err)
	}
	if want := k.Public.ServerPublic.String(); c.Key != want {
		t.Fatalf("Key = %q, want %q", c.Key, want)
	}
}

func TestNewCatRejectsEmptyName(t *testing.T) {
	k := tailcat.NewPrivateKey()
	if _, err := NewCat("", string(k.Public.Addr()), 1); err == nil {
		t.Fatal("NewCat with empty name succeeded, want error")
	}
}

func TestMergeLastWriteWins(t *testing.T) {
	r := New()
	a := cat(t, "a")
	if _, err := r.Merge([]Cat{a}); err != nil {
		t.Fatal(err)
	}

	// Older update loses.
	older := a
	older.Name = "renamed-too-soon"
	older.Updated = 99
	changed, err := r.Merge([]Cat{older})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("older entry changed roster: %v", changed)
	}
	if got, _ := r.Get("renamed-too-soon"); got.Key != "" {
		t.Fatal("older rename was applied")
	}

	// Newer update wins.
	newer := a
	newer.Name = "renamed"
	newer.Updated = 101
	if _, err := r.Merge([]Cat{newer}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("a"); ok {
		t.Fatal("old name still present after rename")
	}
	got, ok := r.Get("renamed")
	if !ok || got.Key != a.Key {
		t.Fatalf("Get(renamed) = %v, %v", got, ok)
	}
}

func TestMergeTieBreakDeterministic(t *testing.T) {
	r := New()
	a := cat(t, "a")
	a.Name = "left"
	a2 := a
	a2.Name = "right"
	// Same Updated: the greater Addr string wins. Both entries here have
	// the same Addr, so the greater Name wins; entries that differ only
	// by name converge either way.
	_, err := r.Merge([]Cat{a, a2})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetByKey(a.Key)
	if got.Name != "right" {
		t.Fatalf("tie-break gave %q, want %q", got.Name, "right")
	}
}

func TestMergeSkipsEmptyKey(t *testing.T) {
	r := New()
	changed, err := r.Merge([]Cat{{Name: "x", Addr: "whatever", Updated: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("empty-key entry merged: %v", changed)
	}
	if len(r.All()) != 0 {
		t.Fatal("roster not empty")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster.json")
	r := New()
	if err := r.SetPath(path); err != nil {
		t.Fatal(err)
	}
	a := cat(t, "a")
	a.Storer = true
	if err := r.Add(a); err != nil {
		t.Fatal(err)
	}

	r2 := New()
	if err := r2.SetPath(path); err != nil {
		t.Fatal(err)
	}
	got, ok := r2.Get("a")
	if !ok || got.Key != a.Key || !got.Storer {
		t.Fatalf("round trip = %v, %v", got, ok)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("roster file not written: %v", err)
	}
}

func TestStorers(t *testing.T) {
	r := New()
	a := cat(t, "a")
	a.Storer = true
	b := cat(t, "b")
	if _, err := r.Merge([]Cat{a, b}); err != nil {
		t.Fatal(err)
	}
	storers := r.Storers()
	if len(storers) != 1 || storers[0].Name != "a" {
		t.Fatalf("Storers() = %v", storers)
	}
}

func TestGetPrefersNewestDuplicate(t *testing.T) {
	r := New()
	old := Cat{Name: "milo", Key: "nodekey:old", Addr: "tcX", Updated: 100}
	if err := r.Add(old); err != nil {
		t.Fatal(err)
	}

	// A sole claimant does not have its own name taken.
	if r.NameTaken("milo", old.Key) {
		t.Fatal("NameTaken with the sole claimant's own key must be false")
	}
	if !r.NameTaken("milo", "nodekey:other") {
		t.Fatal("NameTaken with a different key must be true")
	}

	new := Cat{Name: "milo", Key: "nodekey:new", Addr: "tcY", Updated: 200}
	if err := r.Add(new); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get("milo")
	if !ok {
		t.Fatal("duplicate name not found")
	}
	if got.Key != new.Key {
		t.Fatalf("Get(duplicate) = %s, want the newest entry (%s)", got.Key, new.Key)
	}

	dups := r.Duplicates()
	if len(dups) != 1 || dups[0] != "milo" {
		t.Fatalf("Duplicates() = %v, want [milo]", dups)
	}
	// With two claimants, each sees the name taken by the other.
	if !r.NameTaken("milo", old.Key) || !r.NameTaken("milo", new.Key) {
		t.Fatal("duplicate claimants must see the name taken")
	}

	// A single cat is not a duplicate.
	if _, ok := r.Get("old"); ok {
		t.Fatal("unrelated lookup changed")
	}
}

// GetByPrefix resolves the short key `clow status` displays: the hex
// after "nodekey:", any length; an ambiguous prefix matches nothing.
func TestGetByPrefix(t *testing.T) {
	r := New()
	for _, c := range []Cat{
		{Name: "one", Key: "nodekey:aabbccdd11", Updated: 1},
		{Name: "two", Key: "nodekey:aabbeeff22", Updated: 2},
	} {
		if err := r.Add(c); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		prefix string
		want   string
		ok     bool
	}{
		{"aabbcc", "one", true},             // unique
		{"nodekey:aabbccdd11", "one", true}, // full key
		{"aabb", "", false},                 // ambiguous
		{"", "", false},                     // matches everything
		{"zz", "", false},                   // nothing
	} {
		got, ok := r.GetByPrefix(tc.prefix)
		if ok != tc.ok || (ok && got.Name != tc.want) {
			t.Errorf("GetByPrefix(%q) = (%s, %v), want (%s, %v)", tc.prefix, got.Name, ok, tc.want, tc.ok)
		}
	}
}
