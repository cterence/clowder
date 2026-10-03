package daemon

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"clowder/persist"
)

// Entry is one pending outbound send: a source file retried until the
// target or a storer accepts it. The file is re-sealed per attempt.
type Entry struct {
	ID         string `json:"id"`
	TargetName string `json:"target_name"`
	TargetKey  string `json:"target_key"`
	SourcePath string `json:"source_path"`
	FileName   string `json:"file_name"`
	AddedAt    int64  `json:"added_at"`
	// SourceSHA256/SourceSize/SourceModNs cache the source file's
	// digest and stat, so delivery retries skip the re-hash while the
	// file is unchanged.
	SourceSHA256 string `json:"source_sha256,omitempty"`
	SourceSize   int64  `json:"source_size,omitempty"`
	SourceModNs  int64  `json:"source_mod_ns,omitempty"`
}

// outbox persists entries as <dir>/<id>.json.
type outbox struct {
	dir string

	mu        sync.Mutex
	cancelled map[string]bool // deleted IDs: a later Put must not resurrect
}

func newOutbox(dir string) *outbox {
	return &outbox{dir: dir, cancelled: map[string]bool{}}
}

// Put records a pending send, atomically. An ID deleted in the
// meantime stays deleted — a delivery attempt caching its digest
// concurrently with a cancel or forget must not resurrect the entry.
// The whole write runs under the lock: a check-then-write Put could
// otherwise straddle a concurrent Delete and re-create the file.
func (o *outbox) Put(e Entry) error {
	if e.ID == "" {
		return fmt.Errorf("outbox: entry has no ID")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cancelled[e.ID] {
		return nil
	}
	if err := os.MkdirAll(o.dir, 0o700); err != nil {
		return fmt.Errorf("outbox: creating dir: %w", err)
	}
	return persist.SaveJSON(filepath.Join(o.dir, e.ID+".json"), e)
}

// All returns pending entries, oldest first.
func (o *outbox) All() []Entry {
	des, err := os.ReadDir(o.dir)
	if err != nil {
		return nil
	}
	var entries []Entry
	for _, de := range des {
		if filepath.Ext(de.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(o.dir, de.Name()))
		if err != nil {
			continue
		}
		var e Entry
		if json.Unmarshal(b, &e) == nil && e.ID != "" {
			entries = append(entries, e)
		}
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		return cmp.Or(cmp.Compare(a.AddedAt, b.AddedAt), cmp.Compare(a.ID, b.ID))
	})
	return entries
}

// Delete removes a pending entry; a missing entry is not an error.
func (o *outbox) Delete(id string) error {
	o.mu.Lock()
	o.cancelled[id] = true
	o.mu.Unlock()
	err := os.Remove(filepath.Join(o.dir, id+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("outbox: deleting entry %s: %w", id, err)
	}
	return nil
}

// Clear removes all pending entries and returns how many.
func (o *outbox) Clear() (int, error) {
	entries := o.All()
	for _, e := range entries {
		if err := o.Delete(e.ID); err != nil {
			return 0, err
		}
	}
	return len(entries), nil
}
