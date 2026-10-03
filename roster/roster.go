// Package roster tracks the cats this cat knows, identified by node public
// key, merged last-write-wins by timestamp, and never deleted.
package roster

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"

	"github.com/cterence/clowder/persist"
)

// Cat is one clowder member. Key is the node public key in string form,
// Addr the tailcat address, Updated the unix time of the last change (LWW).
type Cat struct {
	Name    string `json:"name" cbor:"n"`
	Addr    string `json:"addr" cbor:"a"`
	Key     string `json:"key" cbor:"k"`                            // node identity (the address's key)
	DialKey string `json:"client_key,omitempty" cbor:"c,omitempty"` // outbound-dial identity peers allowlist
	Storer  bool   `json:"storer,omitempty" cbor:"s,omitempty"`
	// A storer that only serves third parties: no deliveries to itself, no
	// originating sends. Implies Storer.
	Dropbox bool `json:"dropbox,omitempty" cbor:"d,omitempty"`
	// Capacity is the storer's spool budget in bytes (storer only).
	Capacity int64 `json:"capacity,omitempty" cbor:"p,omitempty"`
	// SignKey is the hex Ed25519 public key derived from the cat's
	// node key seed; it signs the entry (Sig) and the cat's leave.
	SignKey string `json:"sign_key,omitempty" cbor:"g,omitempty"`
	// Sig is the cat's Ed25519 signature over the entry itself
	// (Sig excluded), made with SignKey.
	Sig     []byte `json:"sig,omitempty" cbor:"e,omitempty"`
	Updated int64  `json:"updated" cbor:"u"`
}

// NewCat builds a Cat from a tailcat address, deriving the identity key
// from the address itself.
func NewCat(name, addr string, updated int64) (Cat, error) {
	ci, err := tailcat.ParseAddr(tailcat.Addr(addr))
	if err != nil {
		return Cat{}, fmt.Errorf("roster: parsing tailcat address: %w", err)
	}
	if name == "" {
		return Cat{}, errors.New("roster: cat name is empty")
	}
	return Cat{
		Name:    name,
		Addr:    addr,
		Key:     ci.ServerPublic.String(),
		Updated: updated,
	}, nil
}

// Roster is the set of known cats, safe for concurrent use. The zero value
// is not usable; call New.
type Roster struct {
	mu         sync.Mutex
	cats       map[string]Cat       // keyed by Cat.Key
	tombstones map[string]Tombstone // keyed by Tombstone.Key
	path       string               // persistence path, empty for in-memory rosters
}

// New returns an empty, in-memory roster. Call SetPath to enable
// persistence.
func New() *Roster {
	return &Roster{cats: map[string]Cat{}, tombstones: map[string]Tombstone{}}
}

// SetPath names the JSON file the roster persists to and loads any
// existing entries (and tombstones, kept next to it) from it.
func (r *Roster) SetPath(path string) error {
	r.mu.Lock()
	r.path = path
	r.tombstones = loadTombstones(tombstonePath(path))
	r.mu.Unlock()
	if err := r.Load(); err != nil {
		return err
	}
	return nil
}

// Load re-reads the roster from its path, replacing in-memory state.
func (r *Roster) Load() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.path == "" {
		return errors.New("roster: no path set")
	}
	var cats []Cat
	if _, err := persist.LoadJSON(r.path, &cats); err != nil {
		return fmt.Errorf("roster: loading: %w", err)
	}
	m := map[string]Cat{}
	for _, c := range cats {
		if c.Key == "" {
			continue
		}
		m[c.Key] = c
	}
	r.cats = m
	return nil
}

func (r *Roster) saveLocked() error {
	if r.path == "" {
		return nil
	}
	cats := make([]Cat, 0, len(r.cats))
	for _, c := range r.cats {
		cats = append(cats, c)
	}
	sortCats(cats)
	if err := persist.SaveJSON(r.path, cats); err != nil {
		return fmt.Errorf("roster: saving: %w", err)
	}
	ts := make([]Tombstone, 0, len(r.tombstones))
	for _, t := range r.tombstones {
		ts = append(ts, t)
	}
	sortTombstones(ts)
	if err := persist.SaveJSON(tombstonePath(r.path), ts); err != nil {
		return fmt.Errorf("roster: saving tombstones: %w", err)
	}
	return nil
}

// Get returns the cat with the given name. Names are not unique (identity
// is the key); the newest entry wins, so lookups are deterministic.
func (r *Roster) Get(name string) (Cat, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best Cat
	found := false
	for _, c := range r.cats {
		if c.Name != name {
			continue
		}
		if !found || c.Updated > best.Updated {
			best = c
			found = true
		}
	}
	return best, found
}

// Duplicates lists the names claimed by more than one key, sorted.
func (r *Roster) Duplicates() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := map[string]int{}
	for _, c := range r.cats {
		counts[c.Name]++
	}
	var out []string
	for n, k := range counts {
		if k > 1 {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// NameTaken reports whether a name is claimed by a key other than the
// given one — the pairing-time uniqueness check.
func (r *Roster) NameTaken(name, key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.cats {
		if c.Name == name && c.Key != key {
			return true
		}
	}
	return false
}

// GetByKey returns the cat with the given node key.
func (r *Roster) GetByKey(k string) (Cat, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.cats[k]
	return c, ok
}

// GetByPrefix returns the unique cat whose key hex (after "nodekey:")
// starts with p — the short form `clow status` displays. An ambiguous
// prefix matches nothing.
func (r *Roster) GetByPrefix(p string) (Cat, bool) {
	p = strings.TrimPrefix(p, "nodekey:")
	if p == "" {
		return Cat{}, false // would match everything
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var found Cat
	for _, c := range r.cats {
		if strings.HasPrefix(strings.TrimPrefix(c.Key, "nodekey:"), p) {
			if found.Key != "" {
				return Cat{}, false
			}
			found = c
		}
	}
	return found, found.Key != ""
}

// SyncHash is the digest of everything a roster push would carry —
// the sender's own entry, every roster entry and every tombstone —
// in a canonical (key-sorted) form. Equal hashes let sync peers skip
// the roster payload (#16); any change rewrites it. Sig is excluded:
// it is derived from the other fields.
func (r *Roster) SyncHash(me Cat) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := sha256.New()
	hashCat(h, me)
	for _, k := range slices.Sorted(maps.Keys(r.cats)) {
		hashCat(h, r.cats[k])
	}
	for _, k := range slices.Sorted(maps.Keys(r.tombstones)) {
		t := r.tombstones[k]
		_, _ = fmt.Fprintf(h, "T%s\x00%s\x00%d\n", t.Key, t.SignKey, t.Time)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashCat(h io.Writer, c Cat) {
	// The sha256 writer never fails.
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%t\x00%t\x00%d\n",
		c.Key, c.Name, c.Addr, c.DialKey, c.SignKey, c.Updated, c.Storer, c.Dropbox, c.Capacity)
}

// All returns all cats sorted by name.
func (r *Roster) All() []Cat {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.allLocked()
}

func (r *Roster) allLocked() []Cat {
	cats := make([]Cat, 0, len(r.cats))
	for _, c := range r.cats {
		cats = append(cats, c)
	}
	sortCats(cats)
	return cats
}

// Storers returns the cats that declared themselves storers.
func (r *Roster) Storers() []Cat {
	var out []Cat
	for _, c := range r.All() {
		if c.Storer {
			out = append(out, c)
		}
	}
	return out
}

// Add records a cat, stamping Updated if unset, and persists.
func (r *Roster) Add(c Cat) error {
	if c.Key == "" {
		return errors.New("roster: cat has no key")
	}
	r.mu.Lock()
	if c.Updated == 0 {
		c.Updated = now()
	}
	r.cats[c.Key] = c
	err := r.saveLocked()
	r.mu.Unlock()
	return err
}

// Merge applies incoming cats LWW: newer Updated wins, ties broken by Addr
// then Name so all cats converge. A tombstoned key is refused unless the
// entry is a newer, validly signed rejoin by the leaver. Known keys must
// be signed by the pinned sign key, so no cat can inject or override
// another's entry; a pre-signing local entry pins the first sign key it
// sees. Returns the entries that changed state. MaxClockSkew bounds future
// timestamps, so a broken clock cannot win every merge.
const MaxClockSkew = 5 * time.Minute

func (r *Roster) Merge(cats []Cat) (changed []Cat, err error) {
	r.mu.Lock()
	limit := now() + int64(MaxClockSkew/time.Second)
	for _, c := range cats {
		if c.Key == "" {
			continue
		}
		if c.Updated > limit {
			continue // impossible timestamp: never let it win
		}
		if t, ok := r.tombstones[c.Key]; ok {
			// The leaver rejoining is the only resurrection: newer than the leave,
			// signed by the sign key pinned in the tombstone.
			if c.SignKey != t.SignKey || c.Updated <= t.Time || !verifyEntry(c) {
				continue
			}
			delete(r.tombstones, c.Key)
		}
		old, ok := r.cats[c.Key]
		if ok && old.SignKey != "" {
			if c.SignKey != old.SignKey || !verifyEntry(c) {
				continue // unsigned or foreign-signed: reject
			}
		}
		if ok && !newerWins(c, old) {
			continue
		}
		r.cats[c.Key] = c
		changed = append(changed, c)
	}
	if len(changed) > 0 {
		err = r.saveLocked()
	}
	r.mu.Unlock()
	return changed, err
}

// newerWins reports whether incoming entry c should replace local entry old.
func newerWins(c, old Cat) bool {
	if c.Updated != old.Updated {
		return c.Updated > old.Updated
	}
	if c.Addr != old.Addr {
		return c.Addr > old.Addr
	}
	return c.Name > old.Name
}

// RemoveKey removes exactly the cat with that node key, so duplicate
// names can be disambiguated by key.
func (r *Roster) RemoveKey(key string) (Cat, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.cats[key]
	if !ok {
		return Cat{}, false
	}
	delete(r.cats, key)
	if err := r.saveLocked(); err != nil {
		return Cat{}, false
	}
	return c, true
}

// sortCats orders by name, keys breaking ties: duplicate names must
// land in one deterministic order, or listings shuffle between runs.
func sortCats(cats []Cat) {
	slices.SortFunc(cats, func(a, b Cat) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Key, b.Key))
	})
}

// now returns the current unix time.
func now() int64 { return time.Now().Unix() }
