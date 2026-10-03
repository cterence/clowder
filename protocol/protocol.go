// Package protocol defines the clowder wire protocol: CBOR messages with
// a 4-byte big-endian length prefix over a tailcat TCP stream. Connections
// open with mutual Hello and Roster exchange, then carry one
// Offer/Answer/stream/Ack exchange at a time; the sealed stream itself is
// not framed — after an accepted Offer, exactly Size raw bytes follow.
package protocol

import (
	"bufio"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/cterence/clowder/roster"
	"github.com/fxamacker/cbor/v2"
)

// Bounds a single framed message; a 1000-cat roster is a few hundred KB.
const MaxMessageSize = 8 << 20

const (
	AckStored    = "stored"    // storer spooled the blob; sender's job is done
	AckDelivered = "delivered" // recipient saved and decrypted the blob
	// AckHolding is a query, not a confirmation: a sender asks a storer
	// to prove it still holds the spooled transfer; the reply is an Answer.
	AckHolding = "holding"
)

// Hello is the first message on each connection, sent by both sides.
// Key is the identity (server) key; DialKey is the separate keypair used
// for all outbound dials, which peers allowlist.
// HelloVersion is the wire protocol version. No negotiation: newer peers
// are logged and still served, so an upgrade never partitions the clowder.
const HelloVersion = 1

type Hello struct {
	Name    string `cbor:"n"`
	Key     string `cbor:"k"`
	DialKey string `cbor:"c,omitempty"`
	// Ed25519 public key (hex) derived from the node key seed; signs the
	// sender's roster entries and its leave.
	SignKey string `cbor:"g,omitempty"`
	Addr    string `cbor:"a"`
	Storer  bool   `cbor:"s,omitempty"`
	Dropbox bool   `cbor:"d,omitempty"`
	Version uint16 `cbor:"v,omitempty"`
	// RosterHash is the sender's roster digest (#16): when it matches
	// the receiver's, the roster exchange is skipped — an empty
	// RosterSync instead of the full payload. Pre-#16 peers send none
	// and always get the full roster.
	RosterHash string `cbor:"x,omitempty"`
}

type RosterSync struct {
	Cats       []roster.Cat       `cbor:"c"`
	Tombstones []roster.Tombstone `cbor:"b,omitempty"`
	// Liveness is the sender's last-seen map (unix seconds per key);
	// the pairing push carries it so a fresh joiner's status is alive
	// before its first sync. Display hint only.
	Liveness map[string]int64 `cbor:"l,omitempty"`
}

// Offer announces a sealed file stream. TargetKey/TargetName name the cat
// the file is for: the peer itself, or a third cat when the peer acts as a
// storer. Size is the exact sealed-stream length (envelope.SealedSize);
// SHA256 is of the plaintext. FileName is the wire name: a directory send
// carries a slash-separated relative path (#31) — old daemons flatten it
// to the basename, new ones recreate the structure under the inbox.
type Offer struct {
	ID       string `cbor:"i"`
	FileName string `cbor:"f"`
	Size     int64  `cbor:"z"`
	From     string `cbor:"o"` // sender's declared name
	// FromKey is the sender's node key: recipients block by key, not by
	// the spoofable name. Empty on offers from pre-key senders.
	FromKey    string `cbor:"k,omitempty"`
	SHA256     string `cbor:"h"` // hex SHA-256 of the plaintext
	TargetKey  string `cbor:"t"`
	TargetName string `cbor:"m"`
	// Sig is the sender's Ed25519 signature over every other field
	// (SignOffer): end-to-end sender authentication the final
	// recipient verifies — the relaying storer cannot forge it.
	Sig []byte `cbor:"e,omitempty"`
}

// offerBytes is the canonical form an offer's signature covers: offer
// JSON with Sig cleared (encoding/json field order is stable).
func offerBytes(o *Offer) []byte {
	c := *o
	c.Sig = nil
	b, err := json.Marshal(&c)
	if err != nil {
		return nil // Offer holds only strings, ints and bytes
	}
	return b
}

// SignOffer stamps o with the sender's Ed25519 key over every field
// except Sig.
func SignOffer(priv ed25519.PrivateKey, o *Offer) {
	o.Sig = ed25519.Sign(priv, offerBytes(o))
}

// VerifyOffer checks o's signature against a hex Ed25519 public key.
func VerifyOffer(o *Offer, signKeyHex string) bool {
	k, err := hex.DecodeString(signKeyHex)
	if err != nil || len(k) != ed25519.PublicKeySize || len(o.Sig) == 0 {
		return false
	}
	return ed25519.Verify(k, offerBytes(o), o.Sig)
}

// Answer accepts or rejects an Offer.
type Answer struct {
	ID     string `cbor:"i"`
	OK     bool   `cbor:"k"`
	Reason string `cbor:"r,omitempty"`
}

type Ack struct {
	ID   string `cbor:"i"`
	Kind string `cbor:"k"`
}

// PairIntro is exchanged over the pairing channel; Addr is the sender's
// real address, not the ephemeral pairing one.
type PairIntro struct {
	Name    string `cbor:"n"`
	Addr    string `cbor:"a"`
	DialKey string `cbor:"c,omitempty"`
	SignKey string `cbor:"g,omitempty"`
	Storer  bool   `cbor:"s,omitempty"`
	Dropbox bool   `cbor:"d,omitempty"`
	// The joiner's signed roster entry rides the intro, so the inviter
	// stores it verbatim and a stale tombstone from an old leave cannot
	// un-pair the fresh re-join (the rejoin refusal needs a signed
	// newer entry). Older joiners send none and stay unsigned.
	Capacity int64  `cbor:"p,omitempty"`
	Updated  int64  `cbor:"u"`
	Sig      []byte `cbor:"e,omitempty"`
}

// PairAck confirms the joiner got the inviter's intro; the inviter commits
// and retires the invite only once it arrives, so a lost reply keeps the
// invite alive.
type PairAck struct{}

// LeaveMsg is a signed forget-me; recipients drop the leaver (roster,
// allowlist, spool) and re-broadcast once.
type LeaveMsg struct {
	Key     string `cbor:"k"`
	SignKey string `cbor:"g"`
	Time    int64  `cbor:"t"`
	Sig     []byte `cbor:"s"`
}

// Receipt is the recipient's signed proof that a transfer landed (#8):
// the sender forgets a storer-held send only once this verifies against
// the recipient's pinned sign key. Rides a sync connection.
type Receipt struct {
	ID        string `cbor:"i"` // the transfer, as the sender named it
	TargetKey string `cbor:"k"` // the recipient's node key (the signer)
	SignKey   string `cbor:"g"` // recipient's Ed25519 public key (hex)
	SHA256    string `cbor:"h"` // the offered plaintext digest
	Time      int64  `cbor:"t"`
	Sig       []byte `cbor:"s"`
}

// receiptBytes is the canonical form a receipt's signature covers:
// receipt JSON with Sig cleared (encoding/json field order is stable).
func receiptBytes(r *Receipt) []byte {
	c := *r
	c.Sig = nil
	b, err := json.Marshal(&c)
	if err != nil {
		return nil // Receipt holds only strings, ints and bytes
	}
	return b
}

// SignReceipt stamps r with the recipient's Ed25519 key.
func SignReceipt(priv ed25519.PrivateKey, r *Receipt) {
	r.Sig = ed25519.Sign(priv, receiptBytes(r))
}

// VerifyReceipt checks r's signature against its own SignKey field;
// the daemon must still pin that key to the recipient's roster entry.
func VerifyReceipt(r *Receipt) bool {
	k, err := hex.DecodeString(r.SignKey)
	if err != nil || len(k) != ed25519.PublicKeySize || len(r.Sig) == 0 {
		return false
	}
	return ed25519.Verify(k, receiptBytes(r), r.Sig)
}

// Message is the union of all protocol messages; exactly one field is
// non-nil on the wire.
type Message struct {
	Hello   *Hello      `cbor:"h,omitempty"`
	Roster  *RosterSync `cbor:"r,omitempty"`
	Offer   *Offer      `cbor:"o,omitempty"`
	Answer  *Answer     `cbor:"a,omitempty"`
	Ack     *Ack        `cbor:"k,omitempty"`
	Pair    *PairIntro  `cbor:"j,omitempty"`
	PairAck *PairAck    `cbor:"g,omitempty"`
	Leave   *LeaveMsg   `cbor:"l,omitempty"`
	Receipt *Receipt    `cbor:"e,omitempty"`
}

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
	case m.Leave != nil:
		return "leave"
	case m.Receipt != nil:
		return "receipt"
	}
	return "empty"
}

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
	if n := m.fieldsSet(); n != 1 {
		return nil, fmt.Errorf("protocol: %s message sets %d fields, want 1", m.Kind(), n)
	}
	return &m, nil
}

// fieldsSet counts the union fields the message carries; exactly one
// is valid on the wire.
func (m *Message) fieldsSet() int {
	n := 0
	for _, set := range []bool{
		m.Hello != nil, m.Roster != nil, m.Offer != nil, m.Answer != nil,
		m.Ack != nil, m.Pair != nil, m.PairAck != nil, m.Leave != nil,
		m.Receipt != nil,
	} {
		if set {
			n++
		}
	}
	return n
}

type Conn struct {
	r *bufio.Reader
	c io.ReadWriteCloser
}

func NewConn(c io.ReadWriteCloser) *Conn {
	return &Conn{r: bufio.NewReader(c), c: c}
}

func (c *Conn) WriteMsg(m *Message) error { return WriteMsg(c.c, m) }

func (c *Conn) Answer(id string, ok bool, reason string) error {
	return c.WriteMsg(&Message{Answer: &Answer{ID: id, OK: ok, Reason: reason}})
}

func (c *Conn) Ack(id, kind string) error {
	return c.WriteMsg(&Message{Ack: &Ack{ID: id, Kind: kind}})
}

func (c *Conn) ReadMsg() (*Message, error) { return ReadMsg(c.r) }

// Reader is positioned after the last message read; raw transfer bytes
// (the sealed stream after an accepted Offer) must be read through it, not
// the underlying connection — it may hold buffered bytes.
func (c *Conn) Reader() io.Reader { return c.r }

func (c *Conn) Writer() io.Writer { return c.c }

func (c *Conn) Close() error { return c.c.Close() }

// SetDeadline sets both deadlines when the stream supports them, else no-op.
func (c *Conn) SetDeadline(t time.Time) error {
	if nc, ok := c.c.(net.Conn); ok {
		return nc.SetDeadline(t)
	}
	return nil
}
