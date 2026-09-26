package persist

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteStream(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob")
	if err := WriteStream(path, strings.NewReader("hello"), 5); err != nil {
		t.Fatalf("WriteStream: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(b) != "hello" {
		t.Fatalf("content = %q, want %q", b, "hello")
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}

	// A short source is an error, and no file appears.
	if err := WriteStream(filepath.Join(dir, "short"), strings.NewReader("hi"), 5); err == nil {
		t.Fatal("short source succeeded, want error")
	}
	if _, err := os.Stat(filepath.Join(dir, "short")); !os.IsNotExist(err) {
		t.Fatal("short write left a file behind")
	}
	assertNoTempFiles(t, dir)
}

func TestWriteFunc(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := WriteFunc(path, func(w io.Writer) error {
		_, err := w.Write([]byte("payload"))
		return err
	}); err != nil {
		t.Fatalf("WriteFunc: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "payload" {
		t.Fatalf("content = %q, err = %v", b, err)
	}

	// An error from the builder leaves no file behind.
	if err := WriteFunc(filepath.Join(dir, "gone"), func(w io.Writer) error {
		return os.ErrInvalid
	}); err == nil {
		t.Fatal("builder error not reported")
	}
	if _, err := os.Stat(filepath.Join(dir, "gone")); !os.IsNotExist(err) {
		t.Fatal("failed write left a file behind")
	}
	assertNoTempFiles(t, dir)
}

func TestJSONRoundTrip(t *testing.T) {
	type cat struct {
		Name string `json:"name"`
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cat.json")

	if err := SaveJSON(path, cat{Name: "milo"}); err != nil {
		t.Fatalf("SaveJSON: %v", err)
	}
	var got cat
	ok, err := LoadJSON(path, &got)
	if err != nil || !ok {
		t.Fatalf("LoadJSON: ok=%v err=%v", ok, err)
	}
	if got.Name != "milo" {
		t.Fatalf("loaded = %+v, want milo", got)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "\n  ") {
		t.Fatalf("SaveJSON did not indent: %s", b)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}

	// A missing file is not an error.
	ok, err = LoadJSON(filepath.Join(dir, "nope.json"), &got)
	if ok || err != nil {
		t.Fatalf("missing file: ok=%v err=%v", ok, err)
	}

	// A corrupt file is an error.
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJSON(filepath.Join(dir, "bad.json"), &got); err == nil {
		t.Fatal("corrupt file loaded without error")
	}
}

// assertNoTempFiles fails when a temp file leaked in dir.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range des {
		if strings.HasPrefix(de.Name(), ".tmp-") {
			t.Fatalf("temp file leaked: %s", de.Name())
		}
	}
}
