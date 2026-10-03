package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInboxPath(t *testing.T) {
	dir := t.TempDir()
	touch := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Free name passes through unchanged.
	if got := inboxPath(dir, "nap.txt"); got != filepath.Join(dir, "nap.txt") {
		t.Fatalf("free name = %q", got)
	}

	// Collision with an extension numbers before it.
	touch("nap.txt")
	if got := inboxPath(dir, "nap.txt"); got != filepath.Join(dir, "nap-1.txt") {
		t.Fatalf("first collision = %q, want nap-1.txt", got)
	}
	touch("nap-1.txt")
	if got := inboxPath(dir, "nap.txt"); got != filepath.Join(dir, "nap-2.txt") {
		t.Fatalf("second collision = %q, want nap-2.txt", got)
	}

	// Extensionless files get a dash, not a fake extension.
	touch("notes")
	if got := inboxPath(dir, "notes"); got != filepath.Join(dir, "notes-1") {
		t.Fatalf("extensionless collision = %q, want notes-1", got)
	}

	// Dotfiles keep their name as the stem.
	touch(".bashrc")
	if got := inboxPath(dir, ".bashrc"); got != filepath.Join(dir, ".bashrc-1") {
		t.Fatalf("dotfile collision = %q, want .bashrc-1", got)
	}

	// Traversal components are dropped; honest components survive (#31).
	if got := inboxPath(dir, "../../etc/passwd"); got != filepath.Join(dir, "etc", "passwd") {
		t.Fatalf("traversal = %q, want etc/passwd under the inbox", got)
	}
	if got := inboxPath(dir, ".."); got != filepath.Join(dir, "file") {
		t.Fatalf("'..' = %q, want file", got)
	}
}

// A daemon killed mid-receive leaves ".tmp-*" atomic-write leftovers
// in the inbox; the next start must sweep them — the app's publisher
// would otherwise promote the partial file into Downloads.
func TestInboxSweepsStaleTemps(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, "milo"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	inbox := t.TempDir()
	if err := SetInboxAt(dir, inbox); err != nil {
		t.Fatalf("SetInboxAt: %v", err)
	}
	stale := filepath.Join(inbox, ".tmp-1234567890")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	startDaemonAt(t, dir)
	waitFor(t, func() bool {
		_, err := os.Stat(stale)
		return os.IsNotExist(err)
	}, "the stale inbox temp to be swept")
}

func FuzzInboxPath(f *testing.F) {
	f.Add("nap.txt")
	f.Add("../../../etc/passwd")
	f.Add("..")
	f.Add(".bashrc")
	f.Add("weird\x00name")
	f.Add("")
	f.Fuzz(func(t *testing.T, name string) {
		dir := t.TempDir()
		got := inboxPath(dir, name)
		// Whatever the input, the result must stay inside dir.
		if !strings.HasPrefix(got, dir+string(filepath.Separator)) {
			t.Fatalf("inboxPath(%q) escaped the inbox: %q", name, got)
		}
	})
}

// Nested names (#31) keep their directory structure: each component is
// sanitized on its own, traversal components are dropped (the rest of
// the path survives), backslashes are separators, and Windows-reserved
// component names are munged. Collision numbering still applies to the
// leaf only.
func TestInboxPathNested(t *testing.T) {
	dir := t.TempDir()
	touchNested := func(rel string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if got := inboxPath(dir, "photos/cats/nap.txt"); got != filepath.Join(dir, "photos", "cats", "nap.txt") {
		t.Fatalf("nested name = %q", got)
	}
	if got := inboxPath(dir, `photos\cats\nap.txt`); got != filepath.Join(dir, "photos", "cats", "nap.txt") {
		t.Fatalf("backslash-separated name = %q", got)
	}
	// Traversal components are dropped; the honest components survive.
	if got := inboxPath(dir, "../../etc/nap.txt"); got != filepath.Join(dir, "etc", "nap.txt") {
		t.Fatalf("traversal = %q, want etc/nap.txt under the inbox", got)
	}
	if got := inboxPath(dir, "a/./nap.txt//b"); got != filepath.Join(dir, "a", "nap.txt", "b") {
		t.Fatalf("dot and empty components = %q", got)
	}
	// Windows-reserved components are munged, not delivered as-is.
	if got := inboxPath(dir, "photos/CON.txt"); got != filepath.Join(dir, "photos", "_CON.txt") {
		t.Fatalf("reserved component = %q, want _CON.txt", got)
	}
	// The leaf numbers on collision; the directories do not.
	touchNested("photos/nap.txt")
	if got := inboxPath(dir, "photos/nap.txt"); got != filepath.Join(dir, "photos", "nap-1.txt") {
		t.Fatalf("nested collision = %q, want nap-1.txt", got)
	}
}
