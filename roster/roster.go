// Package roster tracks the cats this cat knows: their declared names,
// tailcat addresses, and whether they volunteer as storers. Entries are
// identified by the cat's node public key (the unguessable part of its
// tailcat address), merged last-write-wins by timestamp, and never deleted.
package roster

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
)

// Cat is one member of the clowder. Key is the cat's node public key in
// string form (as produced by key.NodePublic.String), Addr its tailcat
// address, and Updated the unix time of the last change, used for
// last-write-wins merge.
type Cat struct {
	Name      string `json:"name" cbor:"n"`
	Addr      string `json:"addr" cbor:"a"`
	Key       string `json:"key" cbor:"k"`                            // node identity (the address's key)
	ClientKey string `json:"client_key,omitempty" cbor:"c,omitempty"` // outbound-dial identity peers allowlist
	Storer    bool   `json:"storer,omitempty" cbor:"s,omitempty"`
	// Dropbox marks a storer that only serves third parties: it holds
	// and relays files for others but takes no deliveries for itself
	// and cannot originate sends. Implies Storer.
	Dropbox bool `json:"dropbox,omitempty" cbor:"d,omitempty"`
	// Capacity is the storer's spool budget in bytes (storer only).
	Capacity int64 `json:"capacity,omitempty" cbor:"p,omitempty"`
	Updated  int64 `json:"updated" cbor:"u"`
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
	mu   sync.Mutex
	cats map[string]Cat // keyed by Cat.Key
	path string         // persistence path, empty for in-memory rosters
}

// New returns an empty, in-memory roster. Call SetPath to enable
// persistence.
func New() *Roster {
	return &Roster{cats: map[string]Cat{}}
}

// SetPath names the JSON file the roster persists to and loads any
// existing entries from it.
func (r *Roster) SetPath(path string) error {
	r.mu.Lock()
	r.path = path
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
	b, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("roster: reading %s: %w", r.path, err)
	}
	var cats []Cat
	if err := json.Unmarshal(b, &cats); err != nil {
		return fmt.Errorf("roster: parsing %s: %w", r.path, err)
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

// Save persists the roster to its path, if one is set.
func (r *Roster) Save() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveLocked()
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
	b, err := json.MarshalIndent(cats, "", "  ")
	if err != nil {
		return fmt.Errorf("roster: encoding: %w", err)
	}
	if err := os.WriteFile(r.path, b, 0o600); err != nil {
		return fmt.Errorf("roster: writing %s: %w", r.path, err)
	}
	return nil
}

// Get returns the cat with the given declared name.
func (r *Roster) Get(name string) (Cat, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.cats {
		if c.Name == name {
			return c, true
		}
	}
	return Cat{}, false
}

// GetByKey returns the cat with the given node key.
func (r *Roster) GetByKey(k string) (Cat, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.cats[k]
	return c, ok
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

// Merge applies incoming cats with last-write-wins semantics: an incoming
// entry replaces the local one if its Updated is newer, or if Updated is
// equal and its Addr sorts greater (a deterministic tie-break so all cats
// converge). Entries with an empty Key are skipped. It returns the entries
// that changed local state, so callers can react (e.g. allowing the new
// keys). The roster is persisted if anything changed.
func (r *Roster) Merge(cats []Cat) (changed []Cat, err error) {
	r.mu.Lock()
	for _, c := range cats {
		if c.Key == "" {
			continue
		}
		old, ok := r.cats[c.Key]
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

// RemoveName drops a cat by name (local, manual operation; entries are
// never removed by propagation in v1).
func (r *Roster) RemoveName(name string) (Cat, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, c := range r.cats {
		if c.Name == name {
			delete(r.cats, k)
			if err := r.saveLocked(); err != nil {
				return Cat{}, false
			}
			return c, true
		}
	}
	return Cat{}, false
}

func sortCats(cats []Cat) {
	slices.SortFunc(cats, func(a, b Cat) int {
		if a.Name != b.Name {
			if a.Name < b.Name {
				return -1
			}
			return 1
		}
		return 0
	})
}

// now returns the current unix time.
func now() int64 { return time.Now().Unix() }
