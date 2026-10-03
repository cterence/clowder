package daemon

// Signed delivery receipts (#8): an AckStored — even a probed one — is
// not proof a storer-held send reached its target, so the outbox entry
// leaves only on the recipient's signed receipt. The recipient records
// one per successful receive and pushes it to the sender on its sync
// round; the sender verifies it against the recipient's pinned sign
// key. In-memory only: a restart re-receipts on the next re-delivery.

import (
	"time"

	"github.com/cterence/clowder/protocol"
)

// receiptOut is one receipt awaiting delivery to its sender.
type receiptOut struct {
	senderKey string // the offer's FromKey
	r         protocol.Receipt
}

// maxPendingReceipts bounds what peers can make us hold.
const maxPendingReceipts = 1024

func (d *Daemon) rememberReceipt(o *protocol.Offer) {
	if o.FromKey == "" {
		return
	}
	me := d.Me()
	r := protocol.Receipt{
		ID:        o.ID,
		TargetKey: me.Key,
		SignKey:   me.SignKey,
		SHA256:    o.SHA256,
		Time:      time.Now().Unix(),
	}
	protocol.SignReceipt(d.env.SignPriv, &r)
	d.receiptsMu.Lock()
	defer d.receiptsMu.Unlock()
	if d.receipts == nil {
		d.receipts = map[string]receiptOut{}
	}
	if _, exists := d.receipts[o.ID]; !exists && len(d.receipts) >= maxPendingReceipts {
		return // bounded; the entry re-records on the next re-delivery
	}
	d.receipts[o.ID] = receiptOut{senderKey: o.FromKey, r: r}
}

// pushReceipts sends the receipts owed to one peer over a fresh
// handshake and forgets the confirmed ones. Unconfirmed ones retry on
// the next sync round.
func (d *Daemon) pushReceipts(pc *protocol.Conn, peerKey string) {
	d.receiptsMu.Lock()
	pending := make([]receiptOut, 0, len(d.receipts))
	for _, ro := range d.receipts {
		if ro.senderKey == peerKey {
			pending = append(pending, ro)
		}
	}
	d.receiptsMu.Unlock()
	for _, ro := range pending {
		_ = pc.SetDeadline(time.Now().Add(msgTimeout))
		if err := pc.WriteMsg(&protocol.Message{Receipt: &ro.r}); err != nil {
			return
		}
		m, err := pc.ReadMsg()
		if err != nil {
			return
		}
		if m.Answer != nil && m.Answer.OK {
			d.receiptsMu.Lock()
			delete(d.receipts, ro.r.ID)
			d.receiptsMu.Unlock()
		}
	}
}

// handleReceipt verifies a delivery receipt and clears the matching
// outbox entry; the return value says whether the connection may
// continue.
func (d *Daemon) handleReceipt(pc *protocol.Conn, r *protocol.Receipt) bool {
	// The signer must be the roster entry the receipt names, pinned to
	// that sign key, and the signature must verify.
	target, ok := d.ros.GetByKey(r.TargetKey)
	if !ok || target.SignKey != r.SignKey || !protocol.VerifyReceipt(r) {
		return pc.Answer(r.ID, false, "receipt does not verify") == nil
	}
	for _, e := range d.ob.All() {
		if e.ID == r.ID && e.TargetKey == r.TargetKey {
			if err := d.deleteEntry(r.ID); err != nil {
				d.cfg.logf("clowder: deleting receipted entry %s: %v", r.ID, err)
			}
			d.settle(r.ID, "delivered")
			d.cfg.logf("clowder: %s confirmed receiving %s", target.Name, e.FileName)
			break
		}
	}
	// OK even with no matching entry: the send is long settled, and the
	// recipient must stop re-sending the receipt.
	return pc.Answer(r.ID, true, "") == nil
}
