package envelope

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"tailscale.com/types/key"
)

func TestSealOpenStreamRoundTrip(t *testing.T) {
	sender := key.NewNode()
	recipient := key.NewNode()

	sizes := []int64{0, 1, 100, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3*ChunkSize + 12345}
	for _, size := range sizes {
		plain := make([]byte, size)
		if _, err := rand.Read(plain); err != nil {
			t.Fatal(err)
		}
		var sealed, out bytes.Buffer

		gotSize, gotSha, err := SealStream(sender, recipient.Public(), &sealed, bytes.NewReader(plain))
		if err != nil {
			t.Fatalf("SealStream(%d): %v", size, err)
		}
		if gotSize != size {
			t.Fatalf("SealStream(%d) reported size %d", size, gotSize)
		}
		wantSha := sha256.Sum256(plain)
		if gotSha != hex.EncodeToString(wantSha[:]) {
			t.Fatalf("SealStream(%d) sha mismatch", size)
		}

		if sealed.Len() != int(SealedSize(size)) {
			t.Fatalf("sealed size %d, want %d (SealedSize)", sealed.Len(), SealedSize(size))
		}

		outSize, outSha, err := OpenStream(recipient, bytes.NewReader(sealed.Bytes()), &out)
		if err != nil {
			t.Fatalf("OpenStream(%d): %v", size, err)
		}
		if outSize != size || outSha != gotSha {
			t.Fatalf("OpenStream(%d) = %d, %s; want %d, %s", size, outSize, outSha, size, gotSha)
		}
		if !bytes.Equal(out.Bytes(), plain) {
			t.Fatalf("OpenStream(%d) plaintext mismatch", size)
		}
	}
}

func TestOpenStreamWrongKey(t *testing.T) {
	sender := key.NewNode()
	recipient := key.NewNode()
	intruder := key.NewNode()

	var sealed bytes.Buffer
	if _, _, err := SealStream(sender, recipient.Public(), &sealed, bytes.NewReader([]byte("secret"))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenStream(intruder, bytes.NewReader(sealed.Bytes()), io.Discard); !errors.Is(err, ErrBadHeader) {
		t.Fatalf("OpenStream with wrong key = %v, want ErrBadHeader", err)
	}
}

func TestOpenStreamTruncated(t *testing.T) {
	sender := key.NewNode()
	recipient := key.NewNode()
	plain := bytes.Repeat([]byte("z"), 3*ChunkSize) // multiple chunks

	var sealed bytes.Buffer
	if _, _, err := SealStream(sender, recipient.Public(), &sealed, bytes.NewReader(plain)); err != nil {
		t.Fatal(err)
	}
	// Drop the final chunk and the terminator: the now-last chunk carries
	// no last-chunk flag, so decryption must fail.
	stream := sealed.Bytes()
	cut := stream[:len(stream)-4-(ChunkSize+16+4)]
	if _, _, err := OpenStream(recipient, bytes.NewReader(cut), io.Discard); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("OpenStream of truncated stream = %v, want ErrCorrupt", err)
	}
	// Merely dropping the terminator must also fail.
	cut2 := stream[:len(stream)-4]
	if _, _, err := OpenStream(recipient, bytes.NewReader(cut2), io.Discard); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("OpenStream without terminator = %v, want ErrCorrupt", err)
	}
}

func TestOpenStreamTampered(t *testing.T) {
	sender := key.NewNode()
	recipient := key.NewNode()

	var sealed bytes.Buffer
	if _, _, err := SealStream(sender, recipient.Public(), &sealed, bytes.NewReader([]byte("secret"))); err != nil {
		t.Fatal(err)
	}
	stream := sealed.Bytes()
	// Flip a byte inside the first chunk's ciphertext (after the header
	// and the first frame).
	stream[HeaderLen+4+3] ^= 0xff
	if _, _, err := OpenStream(recipient, bytes.NewReader(stream), io.Discard); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("OpenStream of tampered stream = %v, want ErrCorrupt", err)
	}
}

func TestOpenStreamBogusChunkLengthRejected(t *testing.T) {
	sender := key.NewNode()
	recipient := key.NewNode()

	var sealed bytes.Buffer
	if _, _, err := SealStream(sender, recipient.Public(), &sealed, bytes.NewReader([]byte("secret"))); err != nil {
		t.Fatal(err)
	}
	// Corrupt the first chunk's length frame to an out-of-range value.
	stream := sealed.Bytes()
	binary.BigEndian.PutUint32(stream[HeaderLen:HeaderLen+4], ChunkSize+17)
	if _, _, err := OpenStream(recipient, bytes.NewReader(stream), io.Discard); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("OpenStream with bogus chunk length = %v, want ErrCorrupt", err)
	}
}

func TestHeaderIsOpaque(t *testing.T) {
	sender := key.NewNode()
	recipient := key.NewNode()
	secret := "the clowder's darkest whisker secret"
	var sealed bytes.Buffer
	if _, _, err := SealStream(sender, recipient.Public(), &sealed, bytes.NewReader([]byte(secret))); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed.Bytes(), []byte(secret)) {
		t.Fatal("sealed stream contains the plaintext")
	}
}

func TestOpenStreamAllocationBound(t *testing.T) {
	// The chunk loop must reuse its buffers: opening a stream allocates
	// a fixed handful of pieces (header parse, sealed-box open, AEAD,
	// digest) regardless of chunk count. A leaked per-chunk buffer adds
	// allocations linear in the stream length, so open one chunk and
	// sixteen and require the counts to match.
	sender := key.NewNode()
	recipient := key.NewNode()

	allocs := func(size int) float64 {
		plain := make([]byte, size)
		var sealed bytes.Buffer
		if _, _, err := SealStream(sender, recipient.Public(), &sealed, bytes.NewReader(plain)); err != nil {
			t.Fatalf("sealing %d: %v", size, err)
		}
		sealedBytes := sealed.Bytes()
		return testing.AllocsPerRun(5, func() {
			if _, _, err := OpenStream(recipient, bytes.NewReader(sealedBytes), io.Discard); err != nil {
				t.Errorf("OpenStream(%d): %v", size, err)
			}
		})
	}

	one := allocs(ChunkSize)
	many := allocs(16 * ChunkSize)
	if many > one+4 {
		t.Fatalf("OpenStream allocations grow with chunk count: %d chunks = %.0f allocs, 1 chunk = %.0f (per-chunk buffers are back)", 16, many, one)
	}
}
