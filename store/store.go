// Package store implements a storer cat's spool: sealed files held on
// disk for offline recipients, with a time-to-live and explicit deletion
// once the target acknowledges delivery. Files are written atomically
// (temporary file then rename) so a crash never leaves a half-written
// blob visible.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// DefaultTTL is how long a spooled file survives without being fetched.
const DefaultTTL = 7 * 24 * time.Hour

// ErrNotFound is returned when no spooled file matches the request.
var ErrNotFound = errors.New("store: no such spooled file")

// Meta describes one spooled sealed file.
type Meta struct {
	ID         string `json:"id"`
	FileName   string `json:"file_name"`
	Size       int64  `json:"size"` // sealed blob size in bytes
	SHA256     string `json:"sha256"`
	From       string `json:"from"`        // sender's declared name
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

// Put writes a sealed blob and its metadata atomically.
func (s *Spool) Put(meta Meta, blob []byte) error {
	if meta.ID == "" {
		return errors.New("store: meta has no ID")
	}
	if meta.StoredAt == 0 {
		meta.StoredAt = time.Now().Unix()
	}
	sum := sha256.Sum256(blob)
	meta.Size = int64(len(blob))
	meta.SHA256 = hex.EncodeToString(sum[:])

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("store: creating spool dir: %w", err)
	}
	if err := writeFileAtomic(s.blobPath(meta.ID), blob); err != nil {
		return err
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("store: encoding meta: %w", err)
	}
	if err := writeFileAtomic(s.metaPath(meta.ID), metaJSON); err != nil {
		return err
	}
	return nil
}

// List returns the metadata of files held for the given target, sorted by
// stored time, dropping expired entries.
func (s *Spool) List(targetKey string) []Meta {
	now := time.Now()
	var metas []Meta
	for _, m := range s.all() {
		if m.TargetKey != targetKey {
			continue
		}
		if m.ExpiresAt(s.ttl).Before(now) {
			s.Delete(m.ID)
			continue
		}
		metas = append(metas, m)
	}
	slices.SortFunc(metas, func(a, b Meta) int {
		return int(a.StoredAt - b.StoredAt)
	})
	return metas
}

// Open returns the metadata and sealed blob for an ID the target is
// fetching.
func (s *Spool) Open(id string) (Meta, []byte, error) {
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
	blob, err := os.ReadFile(s.blobPath(id))
	if os.IsNotExist(err) {
		return Meta{}, nil, ErrNotFound
	}
	if err != nil {
		return Meta{}, nil, fmt.Errorf("store: reading blob %s: %w", id, err)
	}
	return meta, blob, nil
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

// Sweep deletes expired entries and returns how many were removed. Call
// periodically so files for targets that never return do not accumulate.
func (s *Spool) Sweep() int {
	now := time.Now()
	removed := 0
	for _, m := range s.all() {
		if m.ExpiresAt(s.ttl).Before(now) {
			if err := s.Delete(m.ID); err == nil {
				removed++
			}
		}
	}
	return removed
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

// writeFileAtomic writes data to path via a temp file and rename.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("store: creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("store: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: closing temp file: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("store: chmod temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("store: renaming into place: %w", err)
	}
	return nil
}
