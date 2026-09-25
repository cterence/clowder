package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPutListOpenDelete(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "spool"), 0)

	blob := []byte("sealed blob of nap data")
	if err := s.Put(Meta{ID: "id1", FileName: "nap.txt", From: "fluff",
		TargetKey: "nodekey:me", TargetName: "me"}, blob); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A blob for someone else.
	if err := s.Put(Meta{ID: "id2", FileName: "other.txt", From: "fluff",
		TargetKey: "nodekey:other", TargetName: "other"}, []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got := s.List("nodekey:me")
	if len(got) != 1 || got[0].ID != "id1" {
		t.Fatalf("List = %+v, want one id1 entry", got)
	}
	wantSum := sha256.Sum256(blob)
	if got[0].SHA256 != hex.EncodeToString(wantSum[:]) {
		t.Fatal("Put did not record the blob's SHA-256")
	}
	if got[0].Size != int64(len(blob)) {
		t.Fatalf("Size = %d, want %d", got[0].Size, len(blob))
	}

	meta, data, err := s.Open("id1")
	if err != nil {
		t.Fatalf("Open: %v", err)
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

func TestPutRejectsEmptyID(t *testing.T) {
	s := New(t.TempDir(), 0)
	if err := s.Put(Meta{}, nil); err == nil {
		t.Fatal("Put with no ID succeeded, want error")
	}
}

func TestTTLExpiry(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, time.Hour)

	// An entry stored 2 hours ago with a 1 hour TTL: expired.
	old := time.Now().Add(-2 * time.Hour).Unix()
	if err := s.Put(Meta{ID: "old", TargetKey: "nodekey:me", StoredAt: old},
		[]byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A fresh entry.
	if err := s.Put(Meta{ID: "new", TargetKey: "nodekey:me"}, []byte("y")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if got := s.List("nodekey:me"); len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("List = %+v, want only the fresh entry", got)
	}
	if n := s.Sweep(); n != 0 {
		t.Fatalf("Sweep removed %d, want 0 (List already dropped it)", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.blob")); !os.IsNotExist(err) {
		t.Fatal("expired blob still on disk")
	}
}

func TestSweepRemovesExpired(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, time.Hour)

	old := time.Now().Add(-2 * time.Hour).Unix()
	if err := s.Put(Meta{ID: "old", TargetKey: "nodekey:me", StoredAt: old},
		[]byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n := s.Sweep(); n != 1 {
		t.Fatalf("Sweep removed %d, want 1", n)
	}
	if s.Count() != 0 {
		t.Fatalf("Count = %d, want 0", s.Count())
	}
}

func TestSpoolSurvivesRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	s1 := New(dir, 0)
	if err := s1.Put(Meta{ID: "id1", FileName: "nap.txt", TargetKey: "k"},
		[]byte("blob")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s2 := New(dir, 0) // same dir, fresh instance: simulates a restart
	got := s2.List("k")
	if len(got) != 1 || got[0].ID != "id1" {
		t.Fatalf("List after restart = %+v", got)
	}
}
