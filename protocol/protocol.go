// Package protocol defines the clowder wire protocol: CBOR messages with a
// 4-byte big-endian length prefix, exchanged over a tailcat TCP stream.
//
// Each connection starts with both sides sending a Hello (self
// introduction), then both sides sending a Roster (full-roster sync).
// After the handshake, a connection carries one request/response exchange
// at a time: an Offer/Answer/stream/Ack file transfer. The sealed stream
// itself is not framed as a message:
// after an accepted Offer, exactly Size raw bytes follow on the stream.
package protocol

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"clowder/roster"
	"github.com/fxamacker/cbor/v2"
)

// MaxMessageSize bounds a single framed message. Roster syncs are the
// largest messages; a 1000-cat roster is a few hundred kilobytes.
const MaxMessageSize = 8 << 20

// Ack kinds.
const (
	AckStored    = "stored"    // storer spooled the blob; sender's job is done
	AckDelivered = "delivered" // recipient saved and decrypted the blob
)

// Hello is the first message on each connection, sent by both sides.
// Key is the sender's identity (server) key; ClientKey is the separate
// keypair all its outbound dials use, which peers allowlist.
// HelloVersion is the wire protocol version carried in Hello. There
// is no negotiation: a peer announcing a higher version is logged and
// still served, so an upgrade never partitions the clowder. Version 0
// (absent) means a pre-version peer.
const HelloVersion = 1

type Hello struct {
	Name      string `cbor:"n"`
	Key       string `cbor:"k"`
	ClientKey string `cbor:"c,omitempty"`
	Addr      string `cbor:"a"`
	Storer    bool   `cbor:"s,omitempty"`
	Dropbox   bool   `cbor:"d,omitempty"`
	Version   uint16 `cbor:"v,omitempty"`
}

// RosterSync carries the sender's full roster (including its own entry)
// for union merge on the receiving side.
type RosterSync struct {
	Cats []roster.Cat `cbor:"c"`
}

// Offer announces a sealed file stream. TargetKey/TargetName identify
// the cat the file is for: the peer itself for a direct delivery, a
// third cat when the peer is acting as a storer. Size is the sealed
// stream's exact length in bytes (see envelope.SealedSize): after the
// peer's Answer, exactly Size raw bytes follow on the stream. SHA256 is
// of the plaintext, so the recipient can verify what it decrypts.
type Offer struct {
	ID         string `cbor:"i"`
	FileName   string `cbor:"f"`
	Size       int64  `cbor:"z"`
	From       string `cbor:"o"` // sender's declared name
	SHA256     string `cbor:"h"` // hex SHA-256 of the plaintext
	TargetKey  string `cbor:"t"`
	TargetName string `cbor:"m"`
	// Receipt marks the stream as a delivery receipt for the
	// transfer named by FileName (the original file name): a tiny
	// sealed envelope, not an inbox delivery. Storers relay it like
	// any other transfer and see only the receipt flag and size.
	Receipt bool `cbor:"rc,omitempty"`
	// Resumable tells the receiver this offer can resume where a
	// previous attempt for the same ID stopped: the sender seals
	// with a per-transfer secret, so re-sent frames are byte-identical
	// to the originals. A receiver holding a partial file answers
	// with Resume set to the sealed-stream offset it wants the rest
	// from; a receiver with nothing (or one that does not understand
	// resume) answers Resume 0 and gets a full stream. Receipts are
	// tiny and never resumable.
	Resumable bool `cbor:"rs,omitempty"`
}

// Answer accepts or rejects an Offer. When accepting a Resumable
// offer, Resume is the sealed-stream offset the receiver already
// holds (0: start from the beginning). The sender then emits the
// header followed by the frames from that offset onward.
type Answer struct {
	ID     string `cbor:"i"`
	OK     bool   `cbor:"k"`
	Reason string `cbor:"r,omitempty"`
	Resume int64  `cbor:"o,omitempty"`
}

// Ack confirms a transfer: stored (by a storer) or delivered (by the
// recipient).
type Ack struct {
	ID   string `cbor:"i"`
	Kind string `cbor:"k"`
}

// PairIntro is exchanged over a pairing channel (clow invite / clow
// join) so two cats can learn each other's real identities without
// copying full tailcat addresses. Addr is the sender's real address,
// not the ephemeral pairing one.
type PairIntro struct {
	Name      string `cbor:"n"`
	Addr      string `cbor:"a"`
	ClientKey string `cbor:"c,omitempty"`
	Storer    bool   `cbor:"s,omitempty"`
	Dropbox   bool   `cbor:"d,omitempty"`
}

// PairAck confirms the joiner received the inviter's intro. The
// inviter commits a pairing — records the joiner and retires its
// invite — only once this arrives, so a reply the joiner never saw
// leaves the invite alive for its next attempt.
type PairAck struct{}

// Message is the union of all protocol messages. Exactly one field is
// non-nil on the wire.
type Message struct {
	Hello   *Hello      `cbor:"h,omitempty"`
	Roster  *RosterSync `cbor:"r,omitempty"`
	Offer   *Offer      `cbor:"o,omitempty"`
	Answer  *Answer     `cbor:"a,omitempty"`
	Ack     *Ack        `cbor:"k,omitempty"`
	Pair    *PairIntro  `cbor:"j,omitempty"`
	PairAck *PairAck    `cbor:"g,omitempty"`
}

// Kind returns a short label for the set message, for logging and errors.
func (m *Message) Kind() string {
	switch {
	case m.Hello != nil:
		return "hello"
	case m.Roster != nil:
		return "roster"
	case m.Offer != nil:
		return "offer"
	case m.Answer != nil:
		return "answer"
	case m.Ack != nil:
		return "ack"
	case m.Pair != nil:
		return "pair"
	case m.PairAck != nil:
		return "pairack"
	}
	return "empty"
}

// WriteMsg frames and writes msg to w.
func WriteMsg(w io.Writer, m *Message) error {
	payload, err := cbor.Marshal(m)
	if err != nil {
		return fmt.Errorf("protocol: encoding %s: %w", m.Kind(), err)
	}
	if len(payload) > MaxMessageSize {
		return fmt.Errorf("protocol: %s message too large (%d bytes)", m.Kind(), len(payload))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("protocol: writing %s header: %w", m.Kind(), err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("protocol: writing %s body: %w", m.Kind(), err)
	}
	return nil
}

// ReadMsg reads one framed message from r.
func ReadMsg(r io.Reader) (*Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("protocol: reading header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxMessageSize {
		return nil, fmt.Errorf("protocol: message size %d exceeds limit %d", n, MaxMessageSize)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("protocol: reading body: %w", err)
	}
	var m Message
	if err := cbor.Unmarshal(payload, &m); err != nil {
		return nil, fmt.Errorf("protocol: decoding body: %w", err)
	}
	if m.Kind() == "empty" {
		return nil, errors.New("protocol: empty message")
	}
	return &m, nil
}

// Conn is a message-oriented wrapper over a byte stream, holding a
// buffered reader so framed reads are efficient.
type Conn struct {
	r *bufio.Reader
	c io.ReadWriteCloser
}

// NewConn wraps a stream connection.
func NewConn(c io.ReadWriteCloser) *Conn {
	return &Conn{r: bufio.NewReader(c), c: c}
}

// WriteMsg sends m. See the package-level function.
func (c *Conn) WriteMsg(m *Message) error { return WriteMsg(c.c, m) }

// Answer sends an Answer for a transfer ID: ok accepts it, otherwise
// reason explains the refusal to the peer.
func (c *Conn) Answer(id string, ok bool, reason string) error {
	return c.WriteMsg(&Message{Answer: &Answer{ID: id, OK: ok, Reason: reason}})
}

// AnswerResume sends an accepting Answer carrying the sealed-stream
// offset a resumable transfer should continue from (0: full stream).
func (c *Conn) AnswerResume(id string, resume int64) error {
	return c.WriteMsg(&Message{Answer: &Answer{ID: id, OK: true, Resume: resume}})
}

// Ack sends an Ack of the given kind for a transfer ID.
func (c *Conn) Ack(id, kind string) error {
	return c.WriteMsg(&Message{Ack: &Ack{ID: id, Kind: kind}})
}

// ReadMsg receives one message. See the package-level function.
func (c *Conn) ReadMsg() (*Message, error) { return ReadMsg(c.r) }

// Reader returns the stream positioned right after the last message
// read, for consuming raw transfer bytes (the sealed stream after an
// accepted Offer). Reads must go through this, not the underlying
// connection, because it may hold buffered bytes.
func (c *Conn) Reader() io.Reader { return c.r }

// Writer returns the raw stream for writing raw transfer bytes.
func (c *Conn) Writer() io.Writer { return c.c }

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.c.Close() }

// SetDeadline sets the read/write deadline on the underlying stream if it
// supports deadlines, and is a no-op otherwise.
func (c *Conn) SetDeadline(t time.Time) error {
	if nc, ok := c.c.(net.Conn); ok {
		return nc.SetDeadline(t)
	}
	return nil
}
