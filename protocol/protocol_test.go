package protocol

import (
	"bytes"
	"io"
	"net"
	"reflect"
	"testing"

	"github.com/cterence/clowder/roster"
)

func TestMessageRoundTrip(t *testing.T) {
	msgs := []*Message{
		{Hello: &Hello{Name: "fluff", Key: "nodekey:abc", Addr: "tcXXX", Storer: true}},
		{Hello: &Hello{Name: "fluff", Key: "nodekey:abc", SignKey: "ed25519:abc", Addr: "tcXXX", Version: HelloVersion}},
		{Roster: &RosterSync{Cats: []roster.Cat{
			{Name: "fluff", Addr: "tcXXX", Key: "nodekey:abc", Storer: true, Updated: 42},
			{Name: "milo", Addr: "tcYYY", Key: "nodekey:def", Updated: 43},
		}}},
		{Roster: &RosterSync{Cats: []roster.Cat{
			{Name: "milo", Addr: "tcYYY", Key: "nodekey:def", Updated: 43},
		}, Tombstones: []roster.Tombstone{
			{Key: "nodekey:gone", SignKey: "ed25519:def", Time: 99, Sig: []byte{1, 2, 3}},
		}}},
		{Offer: &Offer{ID: "id1", FileName: "nap.txt", Size: 77, From: "fluff",
			SHA256: "aa11", TargetKey: "nodekey:def", TargetName: "milo"}},
		{Answer: &Answer{ID: "id1", OK: true}},
		{Answer: &Answer{ID: "id1", OK: false, Reason: "not a storer"}},
		{Ack: &Ack{ID: "id1", Kind: AckStored}},
		{PairAck: &PairAck{}},
		{Leave: &LeaveMsg{Key: "nodekey:gone", SignKey: "ed25519:def", Time: 99, Sig: []byte{4, 5, 6}}},
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
		if !reflect.DeepEqual(m, got) {
			t.Fatalf("round trip mismatch:\nwant %+v\ngot  %+v", m, got)
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

// TestMultiFieldMessageRejected: exactly one union field may be set —
// a message carrying two would be processed as whichever Kind()
// matches first, so it is refused at the read boundary.
func TestMultiFieldMessageRejected(t *testing.T) {
	m := &Message{Hello: &Hello{Name: "fluff"}, Ack: &Ack{ID: "id1", Kind: AckStored}}
	var buf bytes.Buffer
	if err := WriteMsg(&buf, m); err != nil {
		t.Fatalf("WriteMsg: %v", err)
	}
	if _, err := ReadMsg(&buf); err == nil {
		t.Fatal("ReadMsg of a two-field message succeeded, want error")
	}
}

func TestConnStreamRoundTrip(t *testing.T) {
	c1, c2 := newPipe(t)
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()

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

func TestConnAnswerAck(t *testing.T) {
	c1, c2 := newPipe(t)
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()

	go func() {
		if err := c1.Answer("t1", false, "storer full"); err != nil {
			t.Errorf("Answer: %v", err)
		}
		if err := c1.Ack("t2", AckStored); err != nil {
			t.Errorf("Ack: %v", err)
		}
	}()
	m, err := c2.ReadMsg()
	if err != nil {
		t.Fatalf("ReadMsg answer: %v", err)
	}
	if m.Answer == nil || m.Answer.ID != "t1" || m.Answer.OK || m.Answer.Reason != "storer full" {
		t.Fatalf("Answer round trip: %+v", m.Answer)
	}
	m, err = c2.ReadMsg()
	if err != nil {
		t.Fatalf("ReadMsg ack: %v", err)
	}
	if m.Ack == nil || m.Ack.ID != "t2" || m.Ack.Kind != AckStored {
		t.Fatalf("Ack round trip: %+v", m.Ack)
	}
}

func TestHelloVersionRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	m := &Message{Hello: &Hello{Name: "fluff", Key: "nodekey:abc", Addr: "tcX", Version: HelloVersion}}
	if err := WriteMsg(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMsg(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hello.Version != HelloVersion {
		t.Fatalf("Hello version round trip = %d, want %d", got.Hello.Version, HelloVersion)
	}
}

func FuzzReadMsg(f *testing.F) {
	// A valid frame and its truncations, plus the rejection cases the
	// parser documents: arbitrary bytes must error or return a
	// message, never panic.
	var buf bytes.Buffer
	m := &Message{Hello: &Hello{Name: "fluff", Key: "nodekey:abc", Addr: "tcX", Version: HelloVersion}}
	if err := WriteMsg(&buf, m); err != nil {
		f.Fatal(err)
	}
	frame := buf.Bytes()
	f.Add(frame)
	f.Add(frame[:len(frame)-1])
	f.Add([]byte{0x00, 0x90, 0x00, 0x00}) // header claiming an oversized frame
	f.Add([]byte{0x00, 0x00, 0x00, 0x00}) // empty frame -> empty message
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := ReadMsg(bytes.NewReader(data))
		if err == nil && msg == nil {
			t.Fatal("ReadMsg returned neither message nor error")
		}
	})
}
