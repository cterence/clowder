package daemon

// Delivery receipts: when a target receives a file (directly or via a
// storer), it seals a tiny receipt {transferID, fileName, deliveredAt}
// to the ORIGINAL SENDER's node key with its own key and relays it
// direct-or-via-storer exactly like any small transfer — storers see
// only the receipt flag and size; the payload is sealed to the sender
// and opaque to them. The sender keeps a receipts.json ledger (via
// persist) shown in `clow status`, closing the loop for sends that
// left the outbox while the sender was offline.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"

	"clowder/envelope"
	"clowder/persist"
	"clowder/protocol"
)

// receiptMaxLen bounds a sealed receipt stream: anything larger is a
// protocol violation, not a receipt.
const receiptMaxLen = 4096

// transferAttemptTimeout bounds one receipt relay attempt.
const transferAttemptTimeout = 30 * time.Second

// Receipt is one confirmed delivery, as kept in the sender's ledger.
type Receipt struct {
	ID          string `json:"id"`           // the transfer ID from the original send
	FileName    string `json:"file_name"`    // the delivered file's name
	From        string `json:"from"`         // the cat that confirmed delivery
	DeliveredAt int64  `json:"delivered_at"` // unix seconds, set by the receiver
	ReceivedAt  int64  `json:"received_at"`  // unix seconds, when the ledger recorded it
}

// receiptEnvelope is the sealed payload of a receipt transfer.
type receiptEnvelope struct {
	ID          string `cbor:"i"`
	FileName    string `cbor:"f"`
	DeliveredAt int64  `cbor:"d"`
}

// receiptKeeper is the sender-side ledger: transfer ID -> confirmed
// delivery. Persisted in receipts.json.
type receiptKeeper struct {
	mu   sync.Mutex
	m    map[string]Receipt
	path string
}

func newReceiptKeeper(dir string) *receiptKeeper {
	return &receiptKeeper{m: map[string]Receipt{}, path: receiptsPath(dir)}
}

func loadReceipts(dir string) *receiptKeeper {
	k := newReceiptKeeper(dir)
	_, _ = persist.LoadJSON(k.path, &k.m)
	return k
}

func receiptsPath(dir string) string { return dir + "/receipts.json" }

// record remembers a confirmed delivery (idempotent by transfer ID)
// and reports whether the ledger changed.
func (k *receiptKeeper) record(r Receipt) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if old, ok := k.m[r.ID]; ok && old.DeliveredAt == r.DeliveredAt && old.From == r.From {
		return false
	}
	r.ReceivedAt = time.Now().Unix()
	k.m[r.ID] = r
	_ = persist.SaveJSON(k.path, k.m) // persist failure keeps the ledger in memory
	return true
}

// get returns one ledger entry.
func (k *receiptKeeper) get(id string) (Receipt, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	r, ok := k.m[id]
	return r, ok
}

// recent returns up to n entries, newest first.
func (k *receiptKeeper) recent(n int) []Receipt {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]Receipt, 0, len(k.m))
	for _, r := range k.m {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Receipt) int {
		if a.ReceivedAt != b.ReceivedAt {
			return int(b.ReceivedAt - a.ReceivedAt)
		}
		return 0
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// sendReceipt seals and relays a receipt for a delivered transfer to
// the original sender, direct first then via storers, best effort: a
// receipt that cannot reach anyone now rides a storer for later, and
// one lost entirely just leaves the sender's ledger unconfirmed.
func (d *Daemon) sendReceipt(fromName, transferID, fileName string) {
	cat, ok := d.ros.Get(fromName)
	if !ok || cat.Key == d.Me().Key {
		return // best effort: unknown sender, no receipt
	}
	payload, err := cbor.Marshal(receiptEnvelope{ID: transferID, FileName: fileName, DeliveredAt: time.Now().Unix()})
	if err != nil {
		d.cfg.logf("clowder: encoding receipt for %s: %v", transferID, err)
		return
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	o := &protocol.Offer{
		ID:         newID(),
		FileName:   fileName,
		Size:       envelope.SealedSize(int64(len(payload))),
		From:       d.Me().Name,
		SHA256:     digest,
		TargetKey:  cat.Key,
		TargetName: cat.Name,
		Receipt:    true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), transferAttemptTimeout)
	defer cancel()
	if err := d.sendSealed(ctx, cat, o, nil, bytes.NewReader(payload), protocol.AckDelivered, false); err == nil {
		return
	}
	for _, s := range d.ros.Storers() {
		if s.Key == cat.Key || d.isBlockedKey(s.Key) {
			continue
		}
		if err := d.sendSealed(ctx, s, o, nil, bytes.NewReader(payload), protocol.AckStored, false); err == nil {
			return
		}
	}
	d.cfg.logf("clowder: receipt for %s could not reach %s (delivered anyway)", fileName, fromName)
}

// receiveReceipt consumes a receipt transfer addressed to us: unseal
// the tiny payload and record the confirmed delivery. It reports
// whether the connection may continue.
func (d *Daemon) receiveReceipt(pc *protocol.Conn, o *protocol.Offer) bool {
	if o.Size > receiptMaxLen {
		d.cfg.logf("clowder: refusing %d-byte receipt stream, want <= %d", o.Size, receiptMaxLen)
		_ = pc.Answer(o.ID, false, "receipt too large")
		return true
	}
	if err := pc.Answer(o.ID, true, ""); err != nil {
		return false
	}
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	var env receiptEnvelope
	src := io.LimitReader(pc.Reader(), o.Size)
	_, gotSha, err := envelope.OpenStream(d.env.Identity.Private, src, cborDecoder{&env})
	if err != nil {
		d.cfg.logf("clowder: opening receipt stream: %v", err)
		return false
	}
	if gotSha != o.SHA256 {
		d.cfg.logf("clowder: receipt digest mismatch for %s", env.ID)
		return false
	}
	if env.ID == "" || env.FileName != o.FileName {
		d.cfg.logf("clowder: receipt stream mismatch (id %q, file %q vs %q)", env.ID, env.FileName, o.FileName)
		return false
	}
	if d.receipts.record(Receipt{ID: env.ID, FileName: env.FileName, From: o.From, DeliveredAt: env.DeliveredAt}) {
		d.cfg.logf("clowder: delivery of %s to %s confirmed", env.FileName, o.From)
	}
	if err := pc.Ack(o.ID, protocol.AckDelivered); err != nil {
		return false
	}
	return true
}

// cborDecoder adapts a target for decode-after-open: OpenStream needs
// an io.Writer for the plaintext.
type cborDecoder struct{ v any }

func (w cborDecoder) Write(p []byte) (int, error) {
	if err := cbor.Unmarshal(p, w.v); err != nil {
		return 0, err
	}
	return len(p), nil
}
