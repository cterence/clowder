// Package store implements a storer cat's spool: sealed streams held on
// disk for offline recipients, with a time-to-live and explicit deletion
// once the target acknowledges delivery. Streams are written atomically
// (temporary file then rename) so a crash never leaves a half-written
// stream visible.
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
	"strings"
	"time"

	"clowder/persist"
)

// DefaultTTL is how long a spooled file survives without being fetched.
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
	// Receipt marks the blob as a delivery receipt rather than a
	// file: relays must preserve the flag so the target routes it to
	// the receipts ledger, not the inbox.
	Receipt bool `json:"receipt,omitempty"`
	// Resumable marks the sealed stream as sealed with a per-transfer
	// secret by the original sender (see envelope.SealStreamAt): the
	// target may answer a delivery attempt with the offset it wants
	// the rest from, and relays must preserve the flag so the offer
	// stays resumable across storer hops.
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

// Put writes a sealed stream and its metadata atomically. meta.Size
// must be set: exactly that many bytes are consumed from r. The SHA256
// is taken as given (it is the plaintext digest the sender announced;
// the stream is opaque to the storer).
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

// depositState is the sidecar of a partially received deposit, the
// storer-side mirror of the receiving cat's parts sidecar: the sender
// answers with the offset to continue from on a retry. SHA256 pins
// the announced digest so an offer reusing an ID with different
// content cannot ride a stale part.
type depositState struct {
	ID        string `json:"id"`
	Size      int64  `json:"size"` // announced sealed-stream size
	SHA256    string `json:"sha256"`
	Offset    int64  `json:"offset"` // sealed-stream bytes already received
	UpdatedAt int64  `json:"updated"`
}

func (s *Spool) partBlobPath(id string) string  { return filepath.Join(s.dir, id+".blob.part") }
func (s *Spool) partStatePath(id string) string { return filepath.Join(s.dir, id+".partstate") }

// DepositResume reports the sealed-stream offset a retry of deposit
// meta.ID should continue from, 0 for a fresh deposit. A partial
// deposit only counts when its sidecar matches the announced size
// and digest and its part blob really holds the recorded prefix.
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

// PutResume resumes or starts a resumable deposit continuing at
// sealed-stream offset resume. r must hold the attempt's shape: the
// re-sent header (envelope.HeaderLen bytes) followed by exactly
// meta.Size - resume bytes of frames. The frames append to the part
// blob, checkpointed at each frame boundary (the storer parses only
// the 4-byte frame lengths — the stream stays opaque); a stream cut
// mid-frame truncates back to the last boundary so the next retry
// resumes from a valid one. On completion the blob takes its final
// name and the meta appears, atomically. resume must come from a
// previous DepositResume for the same meta.
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
	// Every attempt re-sends the sealed-stream header first; the
	// blob already holds it (a fresh deposit starts with it), so it
	// is consumed and verified against the blob's own header: a
	// mismatch means the sender is not re-sending the same stream,
	// and the deposit restarts from zero rather than mixing bytes.
	var header [envelopeHeaderLen]byte
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
		if _, err := f.Seek(envelopeHeaderLen, io.SeekStart); err != nil {
			_ = f.Close()
			return fmt.Errorf("store: seeking part blob: %w", err)
		}
	} else {
		var existing [envelopeHeaderLen]byte
		if _, err := f.ReadAt(existing[:], 0); err != nil {
			_ = f.Close()
			return fmt.Errorf("store: reading part header: %w", err)
		}
		if !bytes.Equal(existing[:], header[:]) {
			_ = f.Close()
			s.DropDepositPart(meta.ID)
			return errors.New("store: retry header differs from the original attempt")
		}
		// Cut any torn tail from the last failure, then append from
		// the recorded boundary.
		if err := f.Truncate(resume); err != nil {
			_ = f.Close()
			return fmt.Errorf("store: trimming part blob: %w", err)
		}
		if _, err := f.Seek(resume, io.SeekStart); err != nil {
			_ = f.Close()
			return fmt.Errorf("store: seeking part blob: %w", err)
		}
	}
	if err != nil {
		return fmt.Errorf("store: opening part blob: %w", err)
	}
	fail := func(err error) error {
		_ = f.Close()
		// A stream cut mid-frame must not leave a torn tail: the
		// part shrinks to the last complete frame boundary, which is
		// where the next attempt resumes.
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
	// offset is the sealed-stream position of the next frame to append:
	// the header precedes the frames on both a fresh and a resumed
	// attempt, and was consumed above.
	offset := resume
	if offset < envelopeHeaderLen {
		offset = envelopeHeaderLen
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
	// The completed blob takes its final name atomically; the meta
	// appears with it.
	if err := os.Rename(part, s.blobPath(meta.ID)); err != nil {
		return fmt.Errorf("store: finishing blob: %w", err)
	}
	_ = os.Remove(s.partStatePath(meta.ID))
	if err := persist.SaveJSON(s.metaPath(meta.ID), meta); err != nil {
		return fmt.Errorf("store: writing meta: %w", err)
	}
	return nil
}

// envelopeHeaderLen mirrors envelope.HeaderLen: the sealed stream
// header the sender re-emits on every attempt. The store package
// does not depend on envelope (it never opens streams), so the
// constant is local and pinned by TestPutResumeRoundTrip.
const envelopeHeaderLen = 115

// truncatePartToBoundary shrinks a partial deposit's part blob back
// to the last complete frame boundary: the offset a retry can safely
// resume from. The walk starts after the header and stops at a zero
// frame (the terminator) or a frame that runs past the blob's end
// (a torn tail).
func (s *Spool) truncatePartToBoundary(meta Meta) {
	id := meta.ID
	path := s.partBlobPath(id)
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	var offset int64 = envelopeHeaderLen
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

// PartsUsage returns the total bytes held in partial deposits, which
// count toward the spool's capacity like completed blobs.
func (s *Spool) PartsUsage() int64 {
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, de := range des {
		if strings.HasSuffix(de.Name(), ".blob.part") {
			if fi, err := de.Info(); err == nil {
				total += fi.Size()
			}
		}
	}
	return total
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
			// An unremovable expired entry resurfaces on the next
			// Sweep; keep listing the rest.
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

// All returns the metadata of every held entry (expired entries
// dropped), sorted by stored time. It is the storer-side view used to
// find delivery targets.
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

// Open returns the metadata and a reader over the sealed stream for an ID
// the target is fetching. Close the reader when done. The reader is
// seekable: a resumable replay starts mid-stream.
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
