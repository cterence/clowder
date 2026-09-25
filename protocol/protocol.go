// Package protocol defines the clowder wire protocol: CBOR messages with a
// 4-byte big-endian length prefix, exchanged over a tailcat TCP stream.
//
// Each connection starts with both sides sending a Hello (self
// introduction), then both sides sending a Roster (full-roster sync).
// After the handshake, a connection carries one request/response exchange
// at a time: an Offer/Answer/stream/Ack file transfer, a Pending query, or
// a Fetch request. The sealed stream itself is not framed as a message:
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
type Hello struct {
	Name      string `cbor:"n"`
	Key       string `cbor:"k"`
	ClientKey string `cbor:"c,omitempty"`
	Addr      string `cbor:"a"`
	Storer    bool   `cbor:"s,omitempty"`
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
}

// Answer accepts or rejects an Offer.
type Answer struct {
	ID     string `cbor:"i"`
	OK     bool   `cbor:"k"`
	Reason string `cbor:"r,omitempty"`
}

// Ack confirms a transfer: stored (by a storer) or delivered (by the
// recipient).
type Ack struct {
	ID   string `cbor:"i"`
	Kind string `cbor:"k"`
}

// Pending is both a query ("what are you holding for me?") and the
// storer's response listing held files.
type Pending struct {
	Query bool          `cbor:"q,omitempty"`
	Files []PendingFile `cbor:"f,omitempty"`
}

// PendingFile describes one file a storer holds for the requester.
type PendingFile struct {
	ID       string `cbor:"i"`
	FileName string `cbor:"f"`
	Size     int64  `cbor:"z"`
	From     string `cbor:"o"`
	SHA256   string `cbor:"h"`
	StoredAt int64  `cbor:"s"`
}

// Fetch asks a storer to send a held file now.
type Fetch struct {
	ID string `cbor:"i"`
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
}

// Message is the union of all protocol messages. Exactly one field is
// non-nil on the wire.
type Message struct {
	Hello   *Hello      `cbor:"h,omitempty"`
	Roster  *RosterSync `cbor:"r,omitempty"`
	Offer   *Offer      `cbor:"o,omitempty"`
	Answer  *Answer     `cbor:"a,omitempty"`
	Ack     *Ack        `cbor:"k,omitempty"`
	Pending *Pending    `cbor:"p,omitempty"`
	Fetch   *Fetch      `cbor:"t,omitempty"`
	Pair    *PairIntro  `cbor:"j,omitempty"`
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
	case m.Pending != nil:
		return "pending"
	case m.Fetch != nil:
		return "fetch"
	case m.Pair != nil:
		return "pair"
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
