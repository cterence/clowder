// Package envelope seals files to a recipient's tailcat node key, so a
// storer cat can hold them for an offline recipient without being able to
// read their contents. It reuses the node keypair that already identifies
// each cat: no separate encryption keyset is needed.
//
// A sealed stream is a self-contained byte sequence that only the
// recipient can decrypt, so a storer can spool and replay it verbatim:
//
//	header (115 bytes):
//	  sender node public key   32 bytes, raw
//	  sealed box               83 bytes = 24-byte nonce +
//	                           (file key 32 + nonce prefix 11 + 16-byte tag)
//	  The box is tailscale's sealed box (key.NodePrivate.SealTo/OpenFrom),
//	  carrying a random 32-byte file key and an 11-byte nonce prefix.
//
//	chunks, each:
//	  4-byte big-endian length L, then L bytes of XChaCha20-Poly1305
//	  ciphertext (L = plaintext chunk + 16-byte tag). L == 0 is the
//	  terminator. Chunk nonces are 11 random bytes || 11-byte big-endian
//	  chunk counter || 2-byte last-chunk flag (age's STREAM construction),
//	  so a stream truncated by dropping trailing chunks fails to decrypt.
//
// There is no size limit beyond the underlying medium: sender and
// receiver work in ChunkSize pieces, never holding the file in memory.
package envelope

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/tailscale/tailcat"
	"golang.org/x/crypto/chacha20poly1305"
	"tailscale.com/types/key"
)

// ChunkSize is the plaintext size of each sealed chunk (64 KiB, matching
// age's STREAM).
const ChunkSize = 64 << 10

const (
	senderKeyLen = key.NodePublicRawLen // 32
	fileKeyLen   = chacha20poly1305.KeySize
	prefixLen    = 11
	nonceLen     = chacha20poly1305.NonceSizeX // 24
	boxOverhead  = 24 + 16                     // sealed-box nonce + Poly1305 tag
	flagLen      = nonceLen - prefixLen - 8    // 5: counter padding + flag bytes

	// lastChunkFlag is set in the final chunk's nonce to make silent
	// truncation detectable.
	lastChunkFlag = 1

	// HeaderLen is the sealed stream header size: sender key + sealed box
	// of (file key + nonce prefix).
	HeaderLen = senderKeyLen + boxOverhead + fileKeyLen + prefixLen
)

// ErrBadHeader is returned when a sealed stream's header cannot be opened.
var ErrBadHeader = errors.New("envelope: cannot open sealed header (wrong key or corrupted header)")

// ErrCorrupt is returned when a chunk fails authentication or the stream
// ends without a terminator.
var ErrCorrupt = errors.New("envelope: corrupt or truncated sealed stream")

// SealStream reads plaintext from src and writes a sealed stream to dst.
// It returns the plaintext size and hex SHA-256 of the plaintext. Memory
// use is bounded by one chunk regardless of file size.
func SealStream(sender key.NodePrivate, recipient key.NodePublic, dst io.Writer, src io.Reader) (plainSize int64, shaHex string, err error) {
	if sender.IsZero() || recipient.IsZero() {
		return 0, "", errors.New("envelope: sealing with zero keys")
	}
	// File key and per-stream nonce prefix, generated together so the
	// sealed header carries both.
	var secret [fileKeyLen + prefixLen]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return 0, "", fmt.Errorf("envelope: generating file key: %w", err)
	}
	aead, err := chacha20poly1305.NewX(secret[:fileKeyLen])
	if err != nil {
		return 0, "", err
	}

	// Header: sender key || sealed box of (file key || nonce prefix).
	pub := tailcat.NodePublic{NodePublic: sender.Public()}
	pubRaw, err := pub.MarshalBinary()
	if err != nil {
		return 0, "", fmt.Errorf("envelope: encoding sender key: %w", err)
	}
	header := make([]byte, 0, HeaderLen)
	header = append(header, pubRaw...)
	header = append(header, sender.SealTo(recipient, secret[:])...)
	if len(header) != HeaderLen {
		return 0, "", fmt.Errorf("envelope: internal error: header is %d bytes, want %d", len(header), HeaderLen)
	}
	if _, err := dst.Write(header); err != nil {
		return 0, "", fmt.Errorf("envelope: writing header: %w", err)
	}

	var prefix [prefixLen]byte
	copy(prefix[:], secret[fileKeyLen:])

	h := sha256.New()
	buf := make([]byte, ChunkSize)
	ct := make([]byte, 0, ChunkSize+aead.Overhead())
	var nonce [nonceLen]byte
	var frame [4]byte
	var seq uint64
	for {
		n, rerr := io.ReadFull(src, buf)
		if n > 0 {
			plainSize += int64(n)
			h.Write(buf[:n])

			last := uint16(0)
			if rerr != nil {
				last = lastChunkFlag
			}
			ct = aead.Seal(ct[:0], chunkNonce(&nonce, prefix, seq, last), buf[:n], nil)
			binary.BigEndian.PutUint32(frame[:], uint32(len(ct)))
			if _, err := dst.Write(frame[:]); err != nil {
				return 0, "", fmt.Errorf("envelope: writing chunk frame: %w", err)
			}
			if _, err := dst.Write(ct); err != nil {
				return 0, "", fmt.Errorf("envelope: writing chunk: %w", err)
			}
			seq++
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				break
			}
			return 0, "", fmt.Errorf("envelope: reading plaintext: %w", rerr)
		}
	}
	// Terminator: a zero-length frame.
	binary.BigEndian.PutUint32(frame[:], 0)
	if _, err := dst.Write(frame[:]); err != nil {
		return 0, "", fmt.Errorf("envelope: writing terminator: %w", err)
	}
	return plainSize, hex.EncodeToString(h.Sum(nil)), nil
}

// OpenStream decrypts a sealed stream from src into dst. src must contain
// exactly one sealed stream (bound it with io.LimitReader if it is
// followed by more data). It returns the plaintext size and hex SHA-256 of
// the plaintext, for comparison with what the sender announced.
func OpenStream(recipient key.NodePrivate, src io.Reader, dst io.Writer) (plainSize int64, shaHex string, err error) {
	header := make([]byte, HeaderLen)
	if _, err := io.ReadFull(src, header); err != nil {
		return 0, "", fmt.Errorf("envelope: reading header: %w", err)
	}
	var pub tailcat.NodePublic
	if err := pub.UnmarshalBinary(header[:senderKeyLen]); err != nil {
		return 0, "", fmt.Errorf("envelope: parsing sender key: %w", err)
	}
	secret, ok := recipient.OpenFrom(pub.NodePublic, header[senderKeyLen:])
	if !ok || len(secret) != fileKeyLen+prefixLen {
		return 0, "", ErrBadHeader
	}
	aead, err := chacha20poly1305.NewX(secret[:fileKeyLen])
	if err != nil {
		return 0, "", err
	}
	var prefix [prefixLen]byte
	copy(prefix[:], secret[fileKeyLen:])

	h := sha256.New()
	// The chunk loop reuses its buffers: opening is allocation-free
	// per chunk (see TestOpenStreamAllocationBound).
	ctBuf := make([]byte, ChunkSize+aead.Overhead())
	plainBuf := make([]byte, 0, ChunkSize)
	var nonce [nonceLen]byte
	var frame [4]byte
	var seq uint64
	for {
		if _, err := io.ReadFull(src, frame[:]); err != nil {
			return 0, "", fmt.Errorf("%w: missing terminator", ErrCorrupt)
		}
		n := binary.BigEndian.Uint32(frame[:])
		if n == 0 {
			break // terminator
		}
		if n < uint32(aead.Overhead()) || n > ChunkSize+uint32(aead.Overhead()) {
			return 0, "", fmt.Errorf("%w: chunk length %d out of range", ErrCorrupt, n)
		}
		ct := ctBuf[:n]
		if _, err := io.ReadFull(src, ct); err != nil {
			return 0, "", fmt.Errorf("%w: chunk cut short: %v", ErrCorrupt, err)
		}
		// Only the final chunk is sealed with the last-chunk flag; try
		// the plain nonce first, then the flagged one. A chunk that
		// opens under neither is corrupt.
		plain, err := aead.Open(plainBuf[:0], chunkNonce(&nonce, prefix, seq, 0), ct, nil)
		if err != nil {
			plain, err = aead.Open(plainBuf[:0], chunkNonce(&nonce, prefix, seq, lastChunkFlag), ct, nil)
			if err != nil {
				return 0, "", ErrCorrupt
			}
		}
		if _, err := dst.Write(plain); err != nil {
			return 0, "", fmt.Errorf("envelope: writing plaintext: %w", err)
		}
		plainSize += int64(len(plain))
		h.Write(plain)
		seq++
	}
	return plainSize, hex.EncodeToString(h.Sum(nil)), nil
}

// chunkNonce builds a per-chunk XChaCha20-Poly1305 nonce into nonce:
// the stream's random prefix, a big-endian chunk counter, and the
// last-chunk flag. Callers hoist one array and refill it per chunk —
// the seal/open loops are allocation-free.
func chunkNonce(nonce *[nonceLen]byte, prefix [prefixLen]byte, seq uint64, last uint16) []byte {
	copy(nonce[:prefixLen], prefix[:])
	binary.BigEndian.PutUint64(nonce[nonceLen-flagLen-8:nonceLen-flagLen], seq)
	binary.BigEndian.PutUint16(nonce[nonceLen-2:], last)
	return nonce[:]
}

// SealedSize returns the exact sealed stream size for a plaintext of
// plainSize bytes, so senders can announce it before streaming.
func SealedSize(plainSize int64) int64 {
	chunks := (plainSize + ChunkSize - 1) / ChunkSize
	return HeaderLen + 4 + plainSize + chunks*(4+aeadTagLen)
}

const aeadTagLen = 16
