package envelope

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"testing"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

// resumeFile writes plain to a temp file and returns an open handle,
// because resumable sealing requires a seekable source.
func resumeFile(t *testing.T, plain []byte) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "src-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, err := f.Write(plain); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSealStreamAtIdenticalBytes(t *testing.T) {
	sender := key.NewNode()
	recipient := key.NewNode()
	var secret [SecretLen]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}

	plain := make([]byte, 5*ChunkSize+12345)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}

	// A fresh seal and a seal of the same secret from any resume point
	// must agree byte-for-byte from that point on (identical header,
	// identical chunk ciphertexts), or a receiver keeping a partial
	// file could never trust a retry.
	fresh := resumeFile(t, plain)
	var full bytes.Buffer
	fullSize, fullSha, err := SealStreamAt(sender, recipient.Public(), secret[:], 0, &full, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if fullSize != int64(len(plain)) {
		t.Fatalf("SealStreamAt size = %d, want %d", fullSize, len(plain))
	}

	off := int64(HeaderLen + 2*(4+ChunkSize+aeadTagLen))
	resumed := resumeFile(t, plain)
	var part bytes.Buffer
	partSize, partSha, err := SealStreamAt(sender, recipient.Public(), secret[:], off, &part, resumed)
	if err != nil {
		t.Fatal(err)
	}
	// The resumed attempt must announce the WHOLE file's values: the
	// recipient verifies the assembled plaintext, not the suffix.
	if partSize != int64(len(plain)) || partSha != fullSha {
		t.Fatalf("resumed attempt announced (%d, %s), want whole-file (%d, %s)", partSize, partSha, len(plain), fullSha)
	}
	fullBytes := full.Bytes()
	partBytes := part.Bytes()
	// The output is header + the frames from off onward; the prefix
	// frames are NOT re-sent. The sealed box in the header is
	// re-randomized per call (a fresh nonce inside SealTo), so headers
	// are not byte-identical; what must hold is that both open to the
	// same secret and that the re-sent frames are byte-identical —
	// that is what a receiver keeping a partial file depends on.
	s0, ok0 := openTestBox(t, recipient, fullBytes[:HeaderLen])
	s1, ok1 := openTestBox(t, recipient, partBytes[:HeaderLen])
	if !ok0 || !ok1 || !bytes.Equal(s0, s1) {
		t.Fatal("headers do not carry the same per-stream secret")
	}
	if !bytes.Equal(fullBytes[off:], partBytes[HeaderLen:]) {
		t.Fatal("resumed frames differ from the original attempt's")
	}
}

// openTestBox extracts the per-stream secret from a sealed header.
func openTestBox(t *testing.T, recipient key.NodePrivate, header []byte) ([]byte, bool) {
	t.Helper()
	var pub tailcat.NodePublic
	if err := pub.UnmarshalBinary(header[:32]); err != nil {
		t.Fatal(err)
	}
	secret, ok := recipient.OpenFrom(pub.NodePublic, header[32:])
	return secret, ok
}

func TestSealedToPlain(t *testing.T) {
	const fullFrame = 4 + ChunkSize + aeadTagLen
	tests := []struct {
		off      int64
		plain    int64
		chunks   int64
		wantFail bool
	}{
		{0, 0, 0, false},
		{HeaderLen, 0, 0, false},
		{HeaderLen + fullFrame, ChunkSize, 1, false},
		{HeaderLen + 3*fullFrame, 3 * ChunkSize, 3, false},
		{1, 0, 0, true},
		{HeaderLen - 1, 0, 0, true},
		{HeaderLen + 1, 0, 0, true},
		{HeaderLen + fullFrame + 1, 0, 0, true},
		{-1, 0, 0, true},
	}
	for _, tt := range tests {
		plain, chunks, err := SealedToPlain(tt.off)
		if tt.wantFail {
			if err == nil {
				t.Errorf("SealedToPlain(%d) succeeded, want error", tt.off)
			}
			continue
		}
		if err != nil {
			t.Errorf("SealedToPlain(%d): %v", tt.off, err)
			continue
		}
		if plain != tt.plain || chunks != tt.chunks {
			t.Errorf("SealedToPlain(%d) = (%d, %d), want (%d, %d)", tt.off, plain, chunks, tt.plain, tt.chunks)
		}
	}
}

// TestOpenResumeRoundTrip seals a file, receives a prefix, then
// simulates a retry: the sender re-seals from the checkpoint and the
// receiver opens the suffix, and the assembled plaintext must match
// the original byte-for-byte.
func TestOpenResumeRoundTrip(t *testing.T) {
	sender := key.NewNode()
	recipient := key.NewNode()
	var secret [SecretLen]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}

	plain := make([]byte, 4*ChunkSize+999)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	src := resumeFile(t, plain)
	var sealed bytes.Buffer
	wholeSize, wholeSha, err := SealStreamAt(sender, recipient.Public(), secret[:], 0, &sealed, src)
	if err != nil {
		t.Fatal(err)
	}
	if wholeSize != int64(len(plain)) {
		t.Fatalf("sealed size %d, want %d", wholeSize, len(plain))
	}

	// First attempt: keep the first two chunks, then "crash" — the
	// stream is cut at the boundary, so opening fails at EOF with a
	// truncated-stream error, which is the crash we simulate. The
	// boundary after the second chunk is the resume point.
	var got bytes.Buffer
	var checkpoint int64
	chunksKept := 0
	_, _, err = OpenStreamAt(recipient, bytes.NewReader(sealed.Bytes()[:HeaderLen+2*(4+ChunkSize+aeadTagLen)]), &got, 0, func(nextOff, plainLen int64) {
		chunksKept++
		if chunksKept <= 2 {
			checkpoint = nextOff
		}
	})
	if err == nil {
		t.Fatal("cut stream opened without error, want truncated-stream error")
	}
	if chunksKept != 2 {
		t.Fatalf("kept %d chunks, want 2", chunksKept)
	}
	plainKept, _, err := SealedToPlain(checkpoint)
	if err != nil {
		t.Fatalf("checkpoint %d is not a boundary: %v", checkpoint, err)
	}
	if plainKept != int64(got.Len()) {
		t.Fatalf("kept %d plaintext bytes, boundary maps to %d", got.Len(), plainKept)
	}

	// Retry: the sender re-seals from the checkpoint. The receiver
	// already holds [0, plainKept) on disk; it skips the re-sent
	// prefix and opens from the boundary.
	resumed := resumeFile(t, plain)
	var retry bytes.Buffer
	retrySize, retrySha, err := SealStreamAt(sender, recipient.Public(), secret[:], checkpoint, &retry, resumed)
	if err != nil {
		t.Fatal(err)
	}
	if retrySize != wholeSize || retrySha != wholeSha {
		t.Fatal("retry announced different whole-file values")
	}
	retryBytes := retry.Bytes()
	if !bytes.Equal(sealed.Bytes()[checkpoint:], retryBytes[HeaderLen:]) {
		t.Fatal("retry frames differ from the original attempt's")
	}

	// The receiver's suffix read opens the retry stream's header to
	// recover the per-stream secret (same secret, fresh box nonce),
	// then decrypts frames from the chunk counter the checkpoint
	// implies. The stream it reads is the full retry output: header
	// + frames from checkpoint onward.
	suffix := bytes.NewReader(retryBytes)
	var rest bytes.Buffer
	restSize, _, err := OpenStreamAt(recipient, suffix, &rest, checkpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if restSize != int64(len(plain))-plainKept {
		t.Fatalf("resumed open size = %d, want %d", restSize, int64(len(plain))-plainKept)
	}
	assembled := append(got.Bytes(), rest.Bytes()...)
	if !bytes.Equal(assembled, plain) {
		t.Fatal("assembled plaintext differs from the original")
	}
	_ = wholeSha
}

// TestLastResumeBoundary pins the clamping rule a receiver must apply
// to its checkpointed offset before answering: only full-chunk frame
// ends are boundaries SealedToPlain accepts, so the answer for
// plainLen held bytes is the end of the last FULL chunk frame.
func TestLastResumeBoundary(t *testing.T) {
	const fullFrame = 4 + ChunkSize + aeadTagLen
	tests := []struct {
		plainLen int64
		want     int64
	}{
		{0, HeaderLen},
		{1, HeaderLen},
		{ChunkSize - 1, HeaderLen},
		{ChunkSize, HeaderLen + fullFrame},
		{ChunkSize + 1, HeaderLen + fullFrame},
		{2 * ChunkSize, HeaderLen + 2*fullFrame},
		{5*ChunkSize + 12345, HeaderLen + 5*fullFrame},
	}
	for _, tt := range tests {
		got := LastResumeBoundary(tt.plainLen)
		if got != tt.want {
			t.Errorf("LastResumeBoundary(%d) = %d, want %d", tt.plainLen, got, tt.want)
			continue
		}
		if _, _, err := SealedToPlain(got); err != nil {
			t.Errorf("LastResumeBoundary(%d) = %d is not a boundary: %v", tt.plainLen, got, err)
		}
	}
}

// TestResumeAfterFinalChunkCheckpoint reproduces a cut in the
// terminator window: every chunk — including the short final one —
// arrived, but not the 4-byte terminator. The receiver's last
// checkpoint names the end of a SHORT frame, which SealedToPlain
// rejects on both sides; answering with it verbatim wedges the
// transfer. The receiver must clamp through LastResumeBoundary and
// re-take the final chunk, and the retry must assemble the whole
// file.
func TestResumeAfterFinalChunkCheckpoint(t *testing.T) {
	sender := key.NewNode()
	recipient := key.NewNode()
	var secret [SecretLen]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}

	// One full chunk plus a short final chunk: the common file shape.
	plain := make([]byte, ChunkSize+12345)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	src := resumeFile(t, plain)
	var sealed bytes.Buffer
	wholeSize, wholeSha, err := SealStreamAt(sender, recipient.Public(), secret[:], 0, &sealed, src)
	if err != nil {
		t.Fatal(err)
	}
	full := sealed.Bytes()

	// Attempt 1 cut in the terminator window: all but the last 4
	// bytes arrived, so the receiver holds the whole plaintext and
	// its last checkpoint follows the final chunk.
	var got bytes.Buffer
	var lastCheckpoint int64
	_, _, err = OpenStreamAt(recipient, bytes.NewReader(full[:len(full)-4]), &got, 0, func(nextOff, plainLen int64) {
		lastCheckpoint = nextOff
	})
	if err == nil {
		t.Fatal("truncated stream opened without error")
	}
	if got.Len() != len(plain) {
		t.Fatalf("receiver holds %d bytes, want the whole %d", got.Len(), len(plain))
	}
	// The raw checkpoint is not a boundary, which is the wedge.
	if _, _, err := SealedToPlain(lastCheckpoint); err == nil {
		t.Fatal("checkpoint after a short final chunk parsed as a boundary")
	}

	// What the receiver must answer instead: the last full-chunk
	// boundary for the plaintext it holds.
	resume := LastResumeBoundary(int64(got.Len()))
	plainKept, _, err := SealedToPlain(resume)
	if err != nil {
		t.Fatalf("LastResumeBoundary(%d) = %d is not a boundary: %v", got.Len(), resume, err)
	}
	if plainKept >= int64(len(plain)) {
		t.Fatalf("boundary maps to %d plaintext, already the whole file", plainKept)
	}

	// Retry: the sender re-seals from the clamped boundary; the
	// receiver discards the plaintext past it and opens the suffix.
	resumed := resumeFile(t, plain)
	var retry bytes.Buffer
	retrySize, retrySha, err := SealStreamAt(sender, recipient.Public(), secret[:], resume, &retry, resumed)
	if err != nil {
		t.Fatal(err)
	}
	if retrySize != wholeSize || retrySha != wholeSha {
		t.Fatal("retry announced different whole-file values")
	}
	if !bytes.Equal(full[resume:], retry.Bytes()[HeaderLen:]) {
		t.Fatal("retry frames differ from the original attempt's")
	}
	var rest bytes.Buffer
	restSize, _, err := OpenStreamAt(recipient, bytes.NewReader(retry.Bytes()), &rest, resume, nil)
	if err != nil {
		t.Fatal(err)
	}
	if restSize != int64(len(plain))-plainKept {
		t.Fatalf("suffix size = %d, want %d", restSize, int64(len(plain))-plainKept)
	}
	assembled := append(append([]byte{}, got.Bytes()[:plainKept]...), rest.Bytes()...)
	if !bytes.Equal(assembled, plain) {
		t.Fatal("assembled plaintext differs from the original")
	}
}
