package roster

// Entry signing and leave tombstones. Each cat derives an Ed25519 keypair
// from its node key seed; the public half rides Hello and roster entries,
// and signs both (so no trusted cat can inject or override entries via LWW)
// and the cat's own leave (so only the leaver can remove the leaver).
// Tombstones ride roster sync and outrank unsigned re-adds; only a re-pair
// (newer entry signed by the leaver) resurrects.

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"

	"clowder/persist"
)

// SignCat stamps c with the sign key derived from priv and signs it. The
// signature covers every field except Sig.
func SignCat(priv ed25519.PrivateKey, c Cat) Cat {
	c.SignKey = hex.EncodeToString(priv.Public().(ed25519.PublicKey))
	c.Sig = ed25519.Sign(priv, entryBytes(c))
	return c
}

// entryBytes is the canonical form a signature covers: entry JSON with
// Sig cleared (encoding/json field order is stable).
func entryBytes(c Cat) []byte {
	c.Sig = nil
	b, err := json.Marshal(c)
	if err != nil {
		// Cat contains only strings, ints and bytes; never fails.
		return nil
	}
	return b
}

// verifyEntry reports whether c carries a valid signature from its
// own announced sign key.
// VerifyEntry reports whether the entry's signature is valid, so the
// inviter can store a joiner's signed intro verbatim.
func VerifyEntry(c Cat) bool { return verifyEntry(c) }

func verifyEntry(c Cat) bool {
	if c.SignKey == "" || len(c.Sig) == 0 {
		return false
	}
	pub, err := hex.DecodeString(c.SignKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), entryBytes(c), c.Sig)
}

// Tombstone is a signed forget-me (Key, SignKey, Time, Sig). It rides roster
// sync and outlives unsigned re-adds; only the leaver's re-pair resurrects
// the key.
type Tombstone struct {
	Key     string `json:"key" cbor:"k"`
	SignKey string `json:"sign_key" cbor:"g"`
	Time    int64  `json:"time" cbor:"t"`
	Sig     []byte `json:"sig" cbor:"s"`
}

// SignTombstone signs a leave for key at unix time at with priv.
func SignTombstone(priv ed25519.PrivateKey, key string, at int64) Tombstone {
	t := Tombstone{
		Key:     key,
		SignKey: hex.EncodeToString(priv.Public().(ed25519.PublicKey)),
		Time:    at,
	}
	t.Sig = ed25519.Sign(priv, t.payload())
	return t
}

// payload is the canonical bytes a tombstone signature covers.
func (t Tombstone) payload() []byte {
	return []byte("clowder-leave:" + t.Key + ":" + strconv.FormatInt(t.Time, 10))
}

// verified reports whether the tombstone carries a valid signature
// from its own announced sign key.
func (t Tombstone) verified() bool {
	if t.SignKey == "" || len(t.Sig) == 0 {
		return false
	}
	pub, err := hex.DecodeString(t.SignKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), t.payload(), t.Sig)
}

// ApplyTombstones verifies and applies incoming tombstones, removing the
// leavers' entries. The sign key must be pinned — by the entry it removes or
// a tombstone already recorded — since one asserted by the tombstone alone
// would let anyone forge anyone's leave. Stale tombstones (leaver rejoined
// newer) are skipped.
func (r *Roster) ApplyTombstones(ts []Tombstone) []Tombstone {
	r.mu.Lock()
	var applied []Tombstone
	for _, t := range ts {
		if !t.verified() {
			continue
		}
		old, hasEntry := r.cats[t.Key]
		if hasEntry {
			// ponytail: a pre-signing entry (no pinned sign key)
			// accepts any validly signed tombstone — an upgrade
			// seam; drop it once no pre-signing roster survives.
			if old.SignKey != "" && old.SignKey != t.SignKey {
				continue // foreign sign key: not the leaver's
			}
			if old.SignKey == t.SignKey && old.Updated > t.Time && verifyEntry(old) {
				continue // leaver already rejoined: stale tombstone
			}
		} else if cur, hasTomb := r.tombstones[t.Key]; !hasTomb || cur.SignKey != t.SignKey {
			// No entry and no prior tombstone: nothing pins the
			// sign key, so the tombstone cannot be verified.
			continue
		}
		if cur, ok := r.tombstones[t.Key]; ok && (cur.SignKey != t.SignKey || cur.Time >= t.Time) {
			continue
		}
		r.tombstones[t.Key] = t
		delete(r.cats, t.Key)
		applied = append(applied, t)
	}
	if len(applied) > 0 {
		_ = r.saveLocked()
	}
	r.mu.Unlock()
	return applied
}

// Tombstones returns the recorded tombstones sorted by leaver key.
func (r *Roster) Tombstones() []Tombstone {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Tombstone, 0, len(r.tombstones))
	for _, t := range r.tombstones {
		out = append(out, t)
	}
	sortTombstones(out)
	return out
}

func sortTombstones(ts []Tombstone) {
	slices.SortFunc(ts, func(a, b Tombstone) int {
		if a.Key != b.Key {
			if a.Key < b.Key {
				return -1
			}
			return 1
		}
		return 0
	})
}

// ClearTombstone forgets a leaver's tombstone: the local half of a
// re-pair, which is the authoritative way a leaver comes back.
func (r *Roster) ClearTombstone(key string) {
	r.mu.Lock()
	if _, ok := r.tombstones[key]; ok {
		delete(r.tombstones, key)
		_ = r.saveLocked()
	}
	r.mu.Unlock()
}

// Reset wipes the roster and every tombstone: leaving the clowder
// discards the old world so a later pairing starts clean.
func (r *Roster) Reset() error {
	r.mu.Lock()
	r.cats = map[string]Cat{}
	r.tombstones = map[string]Tombstone{}
	err := r.saveLocked()
	r.mu.Unlock()
	if err != nil {
		return fmt.Errorf("roster: resetting: %w", err)
	}
	return nil
}

// tombstonePath is where tombstones persist, next to roster.json.
func tombstonePath(path string) string {
	return filepath.Join(filepath.Dir(path), "tombstones.json")
}

// loadTombstones reads the persisted tombstones.
func loadTombstones(path string) map[string]Tombstone {
	var ts []Tombstone
	if _, err := persist.LoadJSON(path, &ts); err != nil {
		return map[string]Tombstone{} // unreadable: start empty
	}
	m := make(map[string]Tombstone, len(ts))
	for _, t := range ts {
		m[t.Key] = t
	}
	return m
}
