package daemon

import (
	"crypto/ed25519"
	"path/filepath"
	"testing"
)

// TestSignKeyDomainSeparated pins the #12 derivation: the Ed25519 sign
// key is HKDF(node seed, sign/v1) — not the raw node seed reused as an
// Ed25519 seed. One seed across WireGuard and Ed25519 is cross-domain
// key reuse; the derivation must also be stable across opens.
func TestSignKeyDomainSeparated(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, "milo"); err != nil {
		t.Fatal(err)
	}
	env, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw := env.Identity.Private.Raw32()
	if env.SignPriv.Equal(ed25519.NewKeyFromSeed(raw[:])) {
		t.Fatal("sign key is the raw node seed reused as an Ed25519 seed")
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !env.SignPriv.Equal(again.SignPriv) {
		t.Fatal("sign key is not stable across opens")
	}
}

// TestInboxDirPerClowder pins the nested inbox default (#19): without
// an explicit --inbox, each clowder receives into its own folder under
// Downloads/clowder/<clowder-name>/ — the config dir's last segment.
func TestInboxDirPerClowder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := t.TempDir()
	if got := InboxDir(filepath.Join(base, "work")); got != filepath.Join(DefaultInbox(), "work") {
		t.Fatalf("work inbox = %q, want %q", got, filepath.Join(DefaultInbox(), "work"))
	}
	if got := InboxDir(filepath.Join(base, "default")); got != filepath.Join(DefaultInbox(), "default") {
		t.Fatalf("default inbox = %q, want %q", got, filepath.Join(DefaultInbox(), "default"))
	}
}
