package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// Entry is one pending outbound send: a source file on this cat's disk
// destined for a target cat, retried until the target or a storer accepts
// it. The file is re-sealed per attempt, so the outbox never holds a
// second copy of the file's bytes.
type Entry struct {
	ID         string `json:"id"`
	TargetName string `json:"target_name"`
	TargetKey  string `json:"target_key"`
	SourcePath string `json:"source_path"`
	FileName   string `json:"file_name"`
	AddedAt    int64  `json:"added_at"`
}

// outbox persists entries as <dir>/<id>.json.
type outbox struct{ dir string }

func newOutbox(dir string) *outbox { return &outbox{dir: dir} }

// Put records a pending send, atomically.
func (o *outbox) Put(e Entry) error {
	if e.ID == "" {
		return fmt.Errorf("outbox: entry has no ID")
	}
	if err := os.MkdirAll(o.dir, 0o700); err != nil {
		return fmt.Errorf("outbox: creating dir: %w", err)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("outbox: encoding entry: %w", err)
	}
	if err := atomicWrite(filepath.Join(o.dir, e.ID+".json"), b); err != nil {
		return err
	}
	return nil
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
		if a.AddedAt != b.AddedAt {
			return int(a.AddedAt - b.AddedAt)
		}
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	return entries
}

// Delete removes a pending entry. Deleting a missing entry is not an
// error.
func (o *outbox) Delete(id string) error {
	err := os.Remove(filepath.Join(o.dir, id+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("outbox: deleting entry %s: %w", id, err)
	}
	return nil
}

// atomicWrite writes data to path via a temp file and rename.
func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("outbox: creating temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("outbox: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("outbox: closing temp file: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("outbox: chmod temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("outbox: renaming into place: %w", err)
	}
	return nil
}
