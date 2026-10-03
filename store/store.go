// Package store implements a storer cat's spool: sealed streams held on
// disk for offline recipients, deleted on delivery or TTL expiry. Writes
// are atomic (temp file + rename), so a crash leaves no half-written stream.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/cterence/clowder/persist"
)

// DefaultTTL is how long a spooled file survives undelivered.
const DefaultTTL = 7 * 24 * time.Hour

// ErrNotFound is returned when no spooled file matches the request.
var ErrNotFound = errors.New("store: no such spooled file")

// Meta describes one spooled sealed stream.
type Meta struct {
	ID       string `json:"id"`
	FileName string `json:"file_name"`
	Size     int64  `json:"size"`   // sealed stream size in bytes
	SHA256   string `json:"sha256"` // hex SHA-256 of the plaintext
	From     string `json:"from"`   // sender's declared name
	// FromKey is the sender's node key, carried onto relayed offers so
	// targets can block by key, not by the spoofable name.
	FromKey    string `json:"from_key,omitempty"`
	TargetKey  string `json:"target_key"`  // recipient node public key, string form
	TargetName string `json:"target_name"` // recipient's declared name
	StoredAt   int64  `json:"stored_at"`   // unix seconds
}

// ExpiresAt returns when the entry expires given the spool's TTL.
func (m Meta) ExpiresAt(ttl time.Duration) time.Time {
	return time.Unix(m.StoredAt, 0).Add(ttl)
}

// Spool is a directory of <id>.meta.json and <id>.blob files. It is safe
// for concurrent use.
type Spool struct {
	dir string
	ttl time.Duration
}

// New returns a spool rooted at dir, using ttl as the retention period
// (zero means DefaultTTL).
func New(dir string, ttl time.Duration) *Spool {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Spool{dir: dir, ttl: ttl}
}

// Put writes meta.Size bytes from r plus the metadata, atomically. The
// SHA256 is taken as given (the stream is opaque to the storer).
func (s *Spool) Put(meta Meta, r io.Reader) error {
	if meta.ID == "" {
		return errors.New("store: meta has no ID")
	}
	if meta.Size < 0 {
		return fmt.Errorf("store: negative stream size %d", meta.Size)
	}
	if meta.StoredAt == 0 {
		meta.StoredAt = time.Now().Unix()
	}

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("store: creating spool dir: %w", err)
	}
	if err := persist.WriteStream(s.blobPath(meta.ID), r, meta.Size); err != nil {
		return fmt.Errorf("store: writing blob: %w", err)
	}
	if err := persist.SaveJSON(s.metaPath(meta.ID), meta); err != nil {
		return fmt.Errorf("store: writing meta: %w", err)
	}
	return nil
}

// List returns the metadata of files held for the given target, sorted by
// stored time, dropping expired entries.
func (s *Spool) List(targetKey string) []Meta {
	return slices.DeleteFunc(s.All(), func(m Meta) bool {
		return m.TargetKey != targetKey
	})
}

// All returns every held entry (expired dropped), sorted by stored time.
func (s *Spool) All() []Meta {
	now := time.Now()
	var metas []Meta
	for _, m := range s.all() {
		if m.ExpiresAt(s.ttl).Before(now) {
			_ = s.Delete(m.ID)
			continue
		}
		metas = append(metas, m)
	}
	slices.SortFunc(metas, func(a, b Meta) int {
		return int(a.StoredAt - b.StoredAt)
	})
	return metas
}

// Usage returns the total sealed bytes currently held.
func (s *Spool) Usage() int64 {
	var total int64
	for _, m := range s.all() {
		total += m.Size
	}
	return total
}

// UsageBy returns the sealed bytes held from one sender (the storer's
// per-sender quota check); an empty fromKey attributes nothing.
func (s *Spool) UsageBy(fromKey string) int64 {
	if fromKey == "" {
		return 0
	}
	var total int64
	for _, m := range s.all() {
		if m.FromKey == fromKey {
			total += m.Size
		}
	}
	return total
}

// Has reports whether the spool holds the transfer (meta and blob).
func (s *Spool) Has(id string) bool {
	_, err1 := os.Stat(s.metaPath(id))
	_, err2 := os.Stat(s.blobPath(id))
	return err1 == nil && err2 == nil
}

// Open returns the metadata and a seekable reader for the sealed stream.
func (s *Spool) Open(id string) (Meta, io.ReadSeekCloser, error) {
	var meta Meta
	mb, err := os.ReadFile(s.metaPath(id))
	if os.IsNotExist(err) {
		return Meta{}, nil, ErrNotFound
	}
	if err != nil {
		return Meta{}, nil, fmt.Errorf("store: reading meta %s: %w", id, err)
	}
	if err := json.Unmarshal(mb, &meta); err != nil {
		return Meta{}, nil, fmt.Errorf("store: parsing meta %s: %w", id, err)
	}
	f, err := os.Open(s.blobPath(id))
	if os.IsNotExist(err) {
		return Meta{}, nil, ErrNotFound
	}
	if err != nil {
		return Meta{}, nil, fmt.Errorf("store: opening blob %s: %w", id, err)
	}
	return meta, f, nil
}

// Delete removes a spooled file. Deleting a missing file is not an error.
func (s *Spool) Delete(id string) error {
	err1 := os.Remove(s.blobPath(id))
	err2 := os.Remove(s.metaPath(id))
	if err1 != nil && !os.IsNotExist(err1) {
		return fmt.Errorf("store: deleting blob %s: %w", id, err1)
	}
	if err2 != nil && !os.IsNotExist(err2) {
		return fmt.Errorf("store: deleting meta %s: %w", id, err2)
	}
	return nil
}

// Count returns the number of spooled entries (expired included).
func (s *Spool) Count() int { return len(s.all()) }

func (s *Spool) all() []Meta {
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var metas []Meta
	for _, de := range des {
		if filepath.Ext(de.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, de.Name()))
		if err != nil {
			continue
		}
		var m Meta
		if json.Unmarshal(b, &m) == nil && m.ID != "" {
			metas = append(metas, m)
		}
	}
	return metas
}

func (s *Spool) blobPath(id string) string { return filepath.Join(s.dir, id+".blob") }
func (s *Spool) metaPath(id string) string { return filepath.Join(s.dir, id+".meta.json") }
