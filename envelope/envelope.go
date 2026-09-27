// Package envelope seals files to a recipient's tailcat node key, so a
// storer cat can hold them for an offline recipient without reading them.
//
// Sealed stream: a 115-byte header — sender node public key (32 bytes,
// raw), then a sealed box (24-byte nonce + 32-byte file key + 11-byte
// nonce prefix + 16-byte tag) — then chunks: 4-byte big-endian length L,
// then L bytes of XChaCha20-Poly1305 ciphertext (age's STREAM: nonce =
// 11 random bytes || big-endian chunk counter || last-chunk flag, so
// truncation fails to decrypt). L == 0 is the terminator. All paths are
// per-chunk; a file is never held in memory.
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

// ChunkSize is the plaintext size of each sealed chunk (age's STREAM).
const ChunkSize = 64 << 10

const (
	senderKeyLen = key.NodePublicRawLen // 32
	fileKeyLen   = chacha20poly1305.KeySize
	prefixLen    = 11
	nonceLen     = chacha20poly1305.NonceSizeX // 24
	boxOverhead  = 24 + 16                     // sealed-box nonce + Poly1305 tag
	flagLen      = nonceLen - prefixLen - 8    // 5: counter padding + flag bytes

	// Set in the final chunk's nonce, making silent truncation detectable.
	lastChunkFlag = 1

	HeaderLen = senderKeyLen + boxOverhead + fileKeyLen + prefixLen
)

// SecretLen is the per-stream secret (file key + nonce prefix): sealing
// with the same secret re-emits byte-identical streams — the basis of resume.
const SecretLen = fileKeyLen + prefixLen

// ErrBadHeader is returned when a sealed stream's header cannot be opened.
var ErrBadHeader = errors.New("envelope: cannot open sealed header (wrong key or corrupted header)")

// ErrCorrupt is returned when a chunk fails authentication or the stream
// ends without a terminator.
var ErrCorrupt = errors.New("envelope: corrupt or truncated sealed stream")

// SealStream seals src to dst, returning the plaintext size and hex
// SHA-256 of the whole file. The secret is random; pass a fixed one to
// SealStreamAt when retries must re-emit identical bytes (resume).
func SealStream(sender key.NodePrivate, recipient key.NodePublic, dst io.Writer, src io.Reader) (plainSize int64, shaHex string, err error) {
	var secret [SecretLen]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return 0, "", fmt.Errorf("envelope: generating file key: %w", err)
	}
	return SealStreamAt(sender, recipient, secret[:], 0, dst, src)
}

// SealStreamAt seals with a caller-fixed secret from sealed-stream offset
// off — a boundary the receiver reported, mapped to a plaintext position
// by SealedToPlain — emitting the header then only the frames from off
// onward. src must be an io.Seeker; the returned size and digest cover
// the whole file.
func SealStreamAt(sender key.NodePrivate, recipient key.NodePublic, secret []byte, off int64, dst io.Writer, src io.Reader) (plainSize int64, shaHex string, err error) {
	if sender.IsZero() || recipient.IsZero() {
		return 0, "", errors.New("envelope: sealing with zero keys")
	}
	if len(secret) != SecretLen {
		return 0, "", fmt.Errorf("envelope: secret is %d bytes, want %d", len(secret), SecretLen)
	}
	plainOff, chunksDone, err := SealedToPlain(off)
	if err != nil {
		return 0, "", err
	}
	s, ok := src.(io.Seeker)
	if !ok {
		return 0, "", errors.New("envelope: resumable seal requires a seekable source")
	}
	h := sha256.New()
	if plainOff > 0 {
		// The digest covers the WHOLE file, so hash the skipped prefix too;
		// a fresh seal (off == 0) stays single-pass.
		if _, err := s.Seek(0, io.SeekStart); err != nil {
			return 0, "", fmt.Errorf("envelope: seeking plaintext to start: %w", err)
		}
		if _, err := io.CopyN(h, src, plainOff); err != nil {
			return 0, "", fmt.Errorf("envelope: hashing skipped prefix: %w", err)
		}
	}
	if _, err := s.Seek(plainOff, io.SeekStart); err != nil {
		return 0, "", fmt.Errorf("envelope: seeking plaintext to %d: %w", plainOff, err)
	}
	plainSize = plainOff
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

	buf := make([]byte, ChunkSize)
	ct := make([]byte, 0, ChunkSize+aead.Overhead())
	var nonce [nonceLen]byte
	var frame [4]byte
	var seq = uint64(chunksDone)
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

// OpenStream decrypts exactly one sealed stream (bound it with
// io.LimitReader if more data follows), returning the plaintext size and
// hex SHA-256 for comparison with what the sender announced.
func OpenStream(recipient key.NodePrivate, src io.Reader, dst io.Writer) (plainSize int64, shaHex string, err error) {
	return openStream(recipient, src, dst, 0, nil)
}

// OpenStreamAt decrypts a resumed suffix beginning at sealed-stream
// offset resumeOff. onChunk, when set, receives the NEXT frame's offset
// (a safe resume point) and the chunk's plaintext length. The returned
// size and digest cover only the newly opened suffix.
func OpenStreamAt(recipient key.NodePrivate, src io.Reader, dst io.Writer, resumeOff int64, onChunk func(nextOff, plainLen int64)) (plainSize int64, shaHex string, err error) {
	return openStream(recipient, src, dst, resumeOff, onChunk)
}

// openStream is the shared decrypt loop; onChunk receives the offset of
// the next frame — the resume point if the transfer dies here.
func openStream(recipient key.NodePrivate, src io.Reader, dst io.Writer, resumeOff int64, onChunk func(nextOff, plainLen int64)) (plainSize int64, shaHex string, err error) {
	_, chunksDone, err := SealedToPlain(resumeOff)
	if err != nil {
		return 0, "", err
	}
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
	// Buffer reuse: opening is allocation-free per chunk.
	ctBuf := make([]byte, ChunkSize+aead.Overhead())
	plainBuf := make([]byte, 0, ChunkSize)
	var nonce [nonceLen]byte
	var frame [4]byte
	var seq = uint64(chunksDone)
	sealedOff := resumeOff
	if sealedOff < HeaderLen {
		sealedOff = HeaderLen
	}
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
		// Try the plain nonce, then the flagged one (only the final chunk
		// is flagged).
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
		if onChunk != nil {
			onChunk(sealedOff+4+int64(n), int64(len(plain)))
		}
		sealedOff += 4 + int64(n)
		seq++
	}
	return plainSize, hex.EncodeToString(h.Sum(nil)), nil
}

// chunkNonce fills nonce with the random prefix, a big-endian chunk
// counter, and the last-chunk flag.
func chunkNonce(nonce *[nonceLen]byte, prefix [prefixLen]byte, seq uint64, last uint16) []byte {
	copy(nonce[:prefixLen], prefix[:])
	binary.BigEndian.PutUint64(nonce[nonceLen-flagLen-8:nonceLen-flagLen], seq)
	binary.BigEndian.PutUint16(nonce[nonceLen-2:], last)
	return nonce[:]
}

func SealedSize(plainSize int64) int64 {
	chunks := (plainSize + ChunkSize - 1) / ChunkSize
	return HeaderLen + 4 + plainSize + chunks*(4+aeadTagLen)
}

const aeadTagLen = 16

// SealedToPlain maps a resume offset (a frame boundary the receiver
// reported) to the plaintext offset and chunk count. Any other value —
// garbage, mid-frame — is an error, never a silent guess.
func SealedToPlain(off int64) (plainOff, chunksDone int64, err error) {
	if off < 0 {
		return 0, 0, fmt.Errorf("envelope: negative resume offset %d", off)
	}
	if off == 0 {
		return 0, 0, nil // fresh start: seal the whole file
	}
	body := off - HeaderLen
	if body < 0 {
		return 0, 0, fmt.Errorf("envelope: resume offset %d is inside the header", off)
	}
	if body == 0 {
		return 0, 0, nil // resume at the header end: nothing received yet
	}
	// Every mid-stream resume point sits at the end of a FULL chunk frame
	// (only the final chunk may be short, and no resume offset names it).
	const fullFrame = 4 + ChunkSize + aeadTagLen
	if body%fullFrame != 0 {
		return 0, 0, fmt.Errorf("envelope: resume offset %d is not a chunk-frame boundary", off)
	}
	chunks := body / fullFrame
	return chunks * ChunkSize, chunks, nil
}

// LastResumeBoundary is the largest valid resume offset for plainLen
// plaintext bytes: the end of the last full chunk frame. A checkpoint
// past the final short chunk would wedge every retry — clamp through this.
func LastResumeBoundary(plainLen int64) int64 {
	full := plainLen / ChunkSize
	if full < 0 {
		full = 0
	}
	return HeaderLen + full*(4+ChunkSize+aeadTagLen)
}
