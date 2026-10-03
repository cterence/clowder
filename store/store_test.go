package store

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPutListOpenDelete(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "spool"), 0)

	blob := []byte("sealed stream of nap data")
	put := func(meta Meta, data []byte) {
		t.Helper()
		meta.Size = int64(len(data))
		if err := s.Put(meta, io.NopCloser(bytes.NewReader(data))); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	put(Meta{ID: "id1", FileName: "nap.txt", From: "fluff",
		TargetKey: "nodekey:me", TargetName: "me", SHA256: "aa11"}, blob)
	// A stream for someone else.
	put(Meta{ID: "id2", FileName: "other.txt", From: "fluff",
		TargetKey: "nodekey:other", TargetName: "other"}, []byte("x"))

	got := s.List("nodekey:me")
	if len(got) != 1 || got[0].ID != "id1" {
		t.Fatalf("List = %+v, want one id1 entry", got)
	}
	if got[0].Size != int64(len(blob)) {
		t.Fatalf("Size = %d, want %d", got[0].Size, len(blob))
	}
	if got[0].SHA256 != "aa11" {
		t.Fatalf("SHA256 = %q, want passthrough of the announced digest", got[0].SHA256)
	}

	meta, r, err := s.Open("id1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if meta.FileName != "nap.txt" || !bytes.Equal(data, blob) {
		t.Fatalf("Open = %+v, %q", meta, data)
	}

	if err := s.Delete("id1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := s.Open("id1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after Delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete("id1"); err != nil {
		t.Fatalf("Delete of missing file = %v, want nil", err)
	}
}

func TestPutRejectsBadMeta(t *testing.T) {
	s := New(t.TempDir(), 0)
	if err := s.Put(Meta{}, bytes.NewReader(nil)); err == nil {
		t.Fatal("Put with no ID succeeded, want error")
	}
	if err := s.Put(Meta{ID: "x", Size: -1}, bytes.NewReader(nil)); err == nil {
		t.Fatal("Put with negative size succeeded, want error")
	}
}

func TestTTLExpiry(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, time.Hour)

	// An entry stored 2 hours ago with a 1 hour TTL: expired.
	old := time.Now().Add(-2 * time.Hour).Unix()
	if err := s.Put(Meta{ID: "old", TargetKey: "nodekey:me", StoredAt: old, Size: 1},
		bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A fresh entry.
	if err := s.Put(Meta{ID: "new", TargetKey: "nodekey:me", Size: 1},
		bytes.NewReader([]byte("y"))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if got := s.List("nodekey:me"); len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("List = %+v, want only the fresh entry", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.blob")); !os.IsNotExist(err) {
		t.Fatal("expired blob still on disk")
	}
}

func TestSpoolSurvivesRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	s1 := New(dir, 0)
	if err := s1.Put(Meta{ID: "id1", FileName: "nap.txt", TargetKey: "k", Size: 4},
		bytes.NewReader([]byte("blob"))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s2 := New(dir, 0) // same dir, fresh instance: simulates a restart
	got := s2.List("k")
	if len(got) != 1 || got[0].ID != "id1" {
		t.Fatalf("List after restart = %+v", got)
	}
}

func TestPutShortStreamRejected(t *testing.T) {
	s := New(t.TempDir(), 0)
	// Size promises more bytes than the reader holds.
	if err := s.Put(Meta{ID: "id1", TargetKey: "k", Size: 100},
		bytes.NewReader([]byte("short"))); err == nil {
		t.Fatal("Put with truncated stream succeeded, want error")
	}
	if _, _, err := s.Open("id1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after failed Put = %v, want ErrNotFound", err)
	}
}
