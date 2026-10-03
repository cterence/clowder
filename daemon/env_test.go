package daemon

import (
	"crypto/ed25519"
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
