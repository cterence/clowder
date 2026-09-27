// Package persist writes local state files atomically: content appears at
// its final path (0600) only once fully written, so a crash never leaves a
// torn roster, ledger or spool behind.
package persist

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteFunc writes path atomically via a temp file: path appears (0600)
// only when write returns nil; any error leaves any existing path untouched.
func WriteFunc(path string, write func(w io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("persist: creating temp file in %s: %w", filepath.Dir(path), err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("persist: writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("persist: closing temp file: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("persist: chmod temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("persist: renaming into %s: %w", path, err)
	}
	return nil
}

// WriteStream copies exactly size bytes from r to path atomically. A
// source shorter than size is an error and leaves no file behind.
func WriteStream(path string, r io.Reader, size int64) error {
	return WriteFunc(path, func(w io.Writer) error {
		if _, err := io.CopyN(w, r, size); err != nil {
			return err
		}
		return nil
	})
}

// LoadJSON decodes the JSON file at path into v. A missing file returns
// (false, nil), leaving v untouched (callers supply the default); a corrupt
// file is an error.
func LoadJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("persist: reading %s: %w", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("persist: parsing %s: %w", path, err)
	}
	return true, nil
}

// SaveJSON encodes v (indented) and writes it to path atomically.
func SaveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("persist: encoding %s: %w", path, err)
	}
	return WriteFunc(path, func(w io.Writer) error {
		_, err := w.Write(b)
		return err
	})
}
