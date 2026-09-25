// Package envelope seals files to a recipient's tailcat node key, so a
// storer cat can hold them for an offline recipient without being able to
// read their contents.
//
// It reuses the node keypair that already identifies each cat: no separate
// encryption keyset is needed. The wire format of a sealed blob is
//
//	sender node public key (32 raw bytes) || sealed-box ciphertext
//
// where the ciphertext is produced by tailscale's sealed box
// (key.NodePrivate.SealTo / OpenFrom), which provides confidentiality and
// authenticity between the two node keys.
package envelope

import (
	"errors"
	"fmt"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

// SenderLen is the size in bytes of the sender public key prefix of a
// sealed blob. It equals key.NodePublicRawLen.
const SenderLen = key.NodePublicRawLen

// ErrTooShort is returned by Open for input that cannot hold the sender
// key prefix and a sealed box.
var ErrTooShort = errors.New("envelope: sealed blob too short")

// Seal encrypts plaintext to recipientPub under the sender's node key.
// Only the holder of recipientPub's private key can open the result.
func Seal(sender key.NodePrivate, recipient key.NodePublic, plaintext []byte) []byte {
	ciphertext := sender.SealTo(recipient, plaintext)
	out := make([]byte, 0, SenderLen+len(ciphertext))
	pub := tailcat.NodePublic{sender.Public()}
	raw, err := pub.MarshalBinary()
	if err != nil {
		// MarshalBinary of a NodePublic cannot fail: it appends the
		// fixed-size raw key.
		panic(err)
	}
	out = append(out, raw...)
	return append(out, ciphertext...)
}

// Open decrypts a blob produced by Seal. recipient is the receiver's node
// private key; the sender's public key is taken from the blob prefix.
func Open(recipient key.NodePrivate, blob []byte) ([]byte, error) {
	if len(blob) <= SenderLen {
		return nil, ErrTooShort
	}
	var pub tailcat.NodePublic
	if err := pub.UnmarshalBinary(blob[:SenderLen]); err != nil {
		return nil, fmt.Errorf("envelope: parsing sender key: %w", err)
	}
	plaintext, ok := recipient.OpenFrom(pub.NodePublic, blob[SenderLen:])
	if !ok {
		return nil, errors.New("envelope: open failed (wrong key or corrupted blob)")
	}
	return plaintext, nil
}
