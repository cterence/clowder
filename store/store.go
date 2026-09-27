// Package store implements a storer cat's spool: sealed streams held on
// disk for offline recipients, deleted on delivery or TTL expiry. Writes
// are atomic (temp file + rename), so a crash leaves no half-written stream.
package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"clowder/envelope"
	"clowder/persist"
)

// DefaultTTL is how long a spooled file survives undelivered.
const DefaultTTL = 7 * 24 * time.Hour

// ErrNotFound is returned when no spooled file matches the request.
var ErrNotFound = errors.New("store: no such spooled file")

// Meta describes one spooled sealed stream.
type Meta struct {
	ID         string `json:"id"`
	FileName   string `json:"file_name"`
	Size       int64  `json:"size"`        // sealed stream size in bytes
	SHA256     string `json:"sha256"`      // hex SHA-256 of the plaintext
	From       string `json:"from"`        // sender's declared name
	TargetKey  string `json:"target_key"`  // recipient node public key, string form
	TargetName string `json:"target_name"` // recipient's declared name
	StoredAt   int64  `json:"stored_at"`   // unix seconds
	// Marks a delivery receipt: relays must preserve the flag so the target
	// routes it to the receipts ledger, not the inbox.
	Receipt bool `json:"receipt,omitempty"`
	// Sealed with a per-transfer secret by the original sender: the target
	// may answer a delivery attempt with a resume offset, and relays must
	// preserve the flag across hops.
	Resumable bool `json:"resumable,omitempty"`
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

// depositState is the sidecar of a partial deposit. SHA256 pins the
// announced digest so an offer reusing an ID with different content cannot
// ride a stale part.
type depositState struct {
	ID        string `json:"id"`
	Size      int64  `json:"size"` // announced sealed-stream size
	SHA256    string `json:"sha256"`
	Offset    int64  `json:"offset"` // sealed-stream bytes already received
	UpdatedAt int64  `json:"updated"`
}

func (s *Spool) partBlobPath(id string) string  { return filepath.Join(s.dir, id+".blob.part") }
func (s *Spool) partStatePath(id string) string { return filepath.Join(s.dir, id+".partstate") }

// DepositResume reports the offset a retry should continue from, 0 for a
// fresh deposit. A partial deposit only counts when sidecar and part blob
// match the announced values.
func (s *Spool) DepositResume(meta Meta) int64 {
	var st depositState
	if _, err := persist.LoadJSON(s.partStatePath(meta.ID), &st); err != nil || st.ID != meta.ID {
		return 0
	}
	if st.Size != meta.Size || st.SHA256 != meta.SHA256 || st.Offset <= 0 || st.Offset >= meta.Size {
		return 0
	}
	if fi, err := os.Stat(s.partBlobPath(meta.ID)); err != nil || fi.Size() < st.Offset {
		return 0
	}
	return st.Offset
}

// DropDepositPart removes a partial deposit, if any.
func (s *Spool) DropDepositPart(id string) {
	_ = os.Remove(s.partBlobPath(id))
	_ = os.Remove(s.partStatePath(id))
}

// PutResume resumes or starts a resumable deposit at sealed-stream
// offset resume. r must hold the re-sent header plus exactly
// meta.Size - resume bytes of frames; the storer parses only frame
// lengths (the stream stays opaque) and checkpoints at each boundary. A
// cut mid-frame truncates back to the last boundary.
func (s *Spool) PutResume(meta Meta, resume int64, r io.Reader) error {
	if meta.ID == "" {
		return errors.New("store: meta has no ID")
	}
	if meta.Size < 0 {
		return fmt.Errorf("store: negative stream size %d", meta.Size)
	}
	if resume < 0 || resume >= meta.Size {
		return fmt.Errorf("store: resume offset %d out of range for %d bytes", resume, meta.Size)
	}
	if meta.StoredAt == 0 {
		meta.StoredAt = time.Now().Unix()
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("store: creating spool dir: %w", err)
	}
	// Every attempt re-sends the header; verify it against the blob's own:
	// a mismatch means a different stream, and the deposit restarts from zero.
	var header [envelope.HeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return fmt.Errorf("store: reading re-sent header: %w", err)
	}
	if resume == 0 {
		s.DropDepositPart(meta.ID)
	}
	part := s.partBlobPath(meta.ID)
	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("store: opening part blob: %w", err)
	}
	if resume == 0 {
		if _, err := f.WriteAt(header[:], 0); err != nil {
			_ = f.Close()
			return fmt.Errorf("store: writing part header: %w", err)
		}
		if _, err := f.Seek(envelope.HeaderLen, io.SeekStart); err != nil {
			_ = f.Close()
			return fmt.Errorf("store: seeking part blob: %w", err)
		}
	} else {
		var existing [envelope.HeaderLen]byte
		if _, err := f.ReadAt(existing[:], 0); err != nil {
			_ = f.Close()
			return fmt.Errorf("store: reading part header: %w", err)
		}
		if !bytes.Equal(existing[:], header[:]) {
			_ = f.Close()
			s.DropDepositPart(meta.ID)
			return errors.New("store: retry header differs from the original attempt")
		}
		// Cut any torn tail, then append from the recorded boundary.
		if err := f.Truncate(resume); err != nil {
			_ = f.Close()
			return fmt.Errorf("store: trimming part blob: %w", err)
		}
		if _, err := f.Seek(resume, io.SeekStart); err != nil {
			_ = f.Close()
			return fmt.Errorf("store: seeking part blob: %w", err)
		}
	}
	fail := func(err error) error {
		_ = f.Close()
		// A cut mid-frame must not leave a torn tail: shrink to the last
		// complete frame boundary.
		s.truncatePartToBoundary(meta)
		return err
	}
	checkpoint := func(offset int64) {
		_ = persist.SaveJSON(s.partStatePath(meta.ID), depositState{
			ID:        meta.ID,
			Size:      meta.Size,
			SHA256:    meta.SHA256,
			Offset:    offset,
			UpdatedAt: time.Now().Unix(),
		})
	}
	offset := resume
	if offset < envelope.HeaderLen {
		offset = envelope.HeaderLen
	}
	remaining := meta.Size - offset
	var frame [4]byte
	for remaining > 0 {
		if _, err := io.ReadFull(r, frame[:]); err != nil {
			return fail(fmt.Errorf("store: reading frame length: %w", err))
		}
		n := binary.BigEndian.Uint32(frame[:])
		if int64(4+n) > remaining {
			return fail(fmt.Errorf("store: frame overruns announced size (%d > %d)", 4+n, remaining))
		}
		if _, err := f.Write(frame[:]); err != nil {
			return fail(fmt.Errorf("store: writing frame length: %w", err))
		}
		if _, err := io.CopyN(f, r, int64(n)); err != nil {
			return fail(fmt.Errorf("store: writing frame: %w", err))
		}
		offset += 4 + int64(n)
		remaining -= 4 + int64(n)
		checkpoint(offset)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: closing part blob: %w", err)
	}
	// Finish atomically.
	if err := os.Rename(part, s.blobPath(meta.ID)); err != nil {
		return fmt.Errorf("store: finishing blob: %w", err)
	}
	_ = os.Remove(s.partStatePath(meta.ID))
	if err := persist.SaveJSON(s.metaPath(meta.ID), meta); err != nil {
		return fmt.Errorf("store: writing meta: %w", err)
	}
	return nil
}

// truncatePartToBoundary shrinks a partial deposit back to the last
// complete frame boundary, stopping at a zero frame (terminator) or a
// frame running past the blob's end (torn tail).
func (s *Spool) truncatePartToBoundary(meta Meta) {
	id := meta.ID
	path := s.partBlobPath(id)
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	var offset int64 = envelope.HeaderLen
	var frame [4]byte
	best := int64(0)
	for {
		if _, err := f.ReadAt(frame[:], offset); err != nil {
			break
		}
		n := binary.BigEndian.Uint32(frame[:])
		if n == 0 {
			best = offset + 4 // terminator: stream complete on disk
			break
		}
		next := offset + 4 + int64(n)
		if _, err := f.ReadAt(make([]byte, 1), next-1); err != nil {
			break // frame runs past the blob's end: torn tail
		}
		best = next
		offset = next
	}
	if best > 0 {
		_ = os.Truncate(path, best)
		_ = persist.SaveJSON(s.partStatePath(id), depositState{
			ID:        id,
			Size:      meta.Size,
			SHA256:    meta.SHA256,
			Offset:    best,
			UpdatedAt: time.Now().Unix(),
		})
	} else {
		_ = os.Remove(path)
		_ = os.Remove(s.partStatePath(id))
	}
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

// Sweep deletes expired entries. Call periodically.
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
