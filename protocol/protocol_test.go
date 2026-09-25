package protocol

import (
	"bytes"
	"io"
	"net"
	"testing"

	"github.com/google/go-cmp/cmp"

	"clowder/roster"
)

func TestMessageRoundTrip(t *testing.T) {
	msgs := []*Message{
		{Hello: &Hello{Name: "fluff", Key: "nodekey:abc", Addr: "tcXXX", Storer: true}},
		{Roster: &RosterSync{Cats: []roster.Cat{
			{Name: "fluff", Addr: "tcXXX", Key: "nodekey:abc", Storer: true, Updated: 42},
			{Name: "milo", Addr: "tcYYY", Key: "nodekey:def", Updated: 43},
		}}},
		{Offer: &Offer{ID: "id1", FileName: "nap.txt", Size: 77, From: "fluff",
			SHA256: "aa11", TargetKey: "nodekey:def", TargetName: "milo"}},
		{Answer: &Answer{ID: "id1", OK: true}},
		{Answer: &Answer{ID: "id1", OK: false, Reason: "not a storer"}},
		{Ack: &Ack{ID: "id1", Kind: AckStored}},
		{Pending: &Pending{Query: true}},
		{Pending: &Pending{Files: []PendingFile{{ID: "id1", FileName: "nap.txt",
			Size: 77, From: "fluff", SHA256: "aa11", StoredAt: 100}}}},
		{Fetch: &Fetch{ID: "id1"}},
	}

	for _, m := range msgs {
		var buf bytes.Buffer
		if err := WriteMsg(&buf, m); err != nil {
			t.Fatalf("WriteMsg(%s): %v", m.Kind(), err)
		}
		got, err := ReadMsg(&buf)
		if err != nil {
			t.Fatalf("ReadMsg(%s): %v", m.Kind(), err)
		}
		if got.Kind() != m.Kind() {
			t.Fatalf("round trip kind = %s, want %s", got.Kind(), m.Kind())
		}
		if diff := cmp.Diff(m, got); diff != "" {
			t.Fatalf("round trip mismatch (-want +got):\n%s", diff)
		}
	}
}

func TestEmptyMessageRejected(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMsg(&buf, &Message{}); err != nil {
		t.Fatalf("WriteMsg: %v", err)
	}
	if _, err := ReadMsg(&buf); err == nil {
		t.Fatal("ReadMsg of empty message succeeded, want error")
	}
}

func TestOversizedFrameRejected(t *testing.T) {
	// 4-byte header claiming a size over the limit.
	buf := bytes.NewReader([]byte{0x00, 0x90, 0x00, 0x00})
	if _, err := ReadMsg(buf); err == nil {
		t.Fatal("ReadMsg of oversized frame succeeded, want error")
	}
}

func TestConnStreamRoundTrip(t *testing.T) {
	c1, c2 := newPipe(t)
	defer c1.Close()
	defer c2.Close()

	stream := bytes.Repeat([]byte("sealed chunk "), 10000) // ~140 KB
	go func() {
		if _, err := c1.Writer().Write(stream); err != nil {
			t.Errorf("stream write: %v", err)
		}
	}()
	got, err := io.ReadAll(io.LimitReader(c2.Reader(), int64(len(stream))))
	if err != nil {
		t.Fatalf("stream read: %v", err)
	}
	if !bytes.Equal(got, stream) {
		t.Fatalf("stream round trip: got %d bytes, want %d", len(got), len(stream))
	}
}

// newPipe returns two connected protocol Conns over an in-memory stream.
func newPipe(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	p1, p2 := net.Pipe()
	return NewConn(p1), NewConn(p2)
}
