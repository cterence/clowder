package envelope

import (
	"bytes"
	"testing"

	"tailscale.com/types/key"
)

func TestSealOpen(t *testing.T) {
	sender := key.NewNode()
	recipient := key.NewNode()
	plain := []byte("the clowder naps in the sun")

	blob := Seal(sender, recipient.Public(), plain)
	got, err := Open(recipient, blob)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("Open(Seal(x)) = %q, want %q", got, plain)
	}
}

func TestOpenWrongKey(t *testing.T) {
	sender := key.NewNode()
	recipient := key.NewNode()
	intruder := key.NewNode()

	blob := Seal(sender, recipient.Public(), []byte("secret"))

	if _, err := Open(intruder, blob); err == nil {
		t.Fatal("Open with the wrong private key succeeded, want error")
	}
}

func TestOpenTampered(t *testing.T) {
	sender := key.NewNode()
	recipient := key.NewNode()

	blob := Seal(sender, recipient.Public(), []byte("secret"))
	blob[len(blob)-1] ^= 0xff

	if _, err := Open(recipient, blob); err == nil {
		t.Fatal("Open of a tampered blob succeeded, want error")
	}
}

func TestOpenTooShort(t *testing.T) {
	recipient := key.NewNode()

	for _, n := range []int{0, 1, SenderLen - 1, SenderLen} {
		if _, err := Open(recipient, make([]byte, n)); err == nil {
			t.Fatalf("Open of %d-byte blob succeeded, want error", n)
		}
	}
}
