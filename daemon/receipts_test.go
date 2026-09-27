package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// findReceipt looks a ledger entry up through the recent() view, so
// the keeper needs no production accessor that only tests use.
func findReceipt(k *receiptKeeper, id string) (Receipt, bool) {
	for _, r := range k.recent(1000) {
		if r.ID == id {
			return r, true
		}
	}
	return Receipt{}, false
}

// TestDeliveryReceipts covers the receipt loop: a direct delivery
// confirms in the sender's ledger, and a receipt for a storer-relayed
// delivery rides the storer while the sender is offline and lands
// when the sender returns.
func TestDeliveryReceipts(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	// Direct: fluff's receipt for milo's send reaches milo at once.
	src := writeSource(t, "nap first")
	id, err := milo.Send("fluff", src)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, ok := inboxFile(t, fluff, "nap.txt")
		return ok
	}, "the direct send to arrive")
	waitFor(t, func() bool {
		r, ok := findReceipt(milo.receipts, id)
		return ok && r.FileName == "nap.txt" && r.From == "fluff" && r.DeliveredAt > 0
	}, "the direct receipt to land in milo's ledger")

	// Storer-relayed: milo sends to an offline niko, then goes offline
	// himself; niko wakes, pulls the file, and his receipt cannot
	// reach milo directly — it must ride the storer until milo
	// returns.
	box := startDaemon(t, "box")
	if err := box.SetStorer(true, 10<<20); err != nil {
		t.Fatal(err)
	}
	niko, nikoDir := offlineCat(t)
	trust(t, milo, box)
	addCat(t, milo, niko)
	addCat(t, box, niko)

	src2 := writeSource(t, "nap second")
	id2, err := milo.Send("niko", src2)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return box.Spool().Count() == 1 },
		"the storer to hold the offline cat's file")
	stopDaemon(milo)

	nikoD := startDaemonAt(t, nikoDir)
	trust(t, nikoD, box)
	addCat(t, nikoD, milo.Me()) // niko knows milo, so the receipt can target him
	trust(t, box, nikoD)
	// The storer's sweep pushes the held file to the waking target.
	box.sweepSpoolFor(context.Background(), nikoD.Me().Key)
	waitFor(t, func() bool {
		_, ok := inboxFile(t, nikoD, "nap.txt")
		return ok
	}, "the storer to push the held file to niko")
	waitFor(t, func() bool { return box.Spool().Count() >= 1 },
		"niko's receipt to be held at the storer (milo is offline)")

	// Milo returns: the storer's sweep pushes the receipt and the
	// ledger closes the loop.
	milo2 := startDaemonAt(t, milo.cfg.Dir)
	trust(t, milo2, box)
	// box is the dialer now (the push sweep), and the restarted milo
	// serves a fresh LocalTransport port — box must re-learn the
	// address, the way the mesh's roster sync would in production
	// (where the tailcat address survives restarts and this cannot
	// happen; the same pattern as TestDirectSendResumesAcrossRestart).
	trust(t, box, milo2)
	// The storer's sweep pushes the held receipt to the returning sender.
	box.sweepSpoolFor(context.Background(), milo2.Me().Key)
	waitFor(t, func() bool { return box.Spool().Count() == 0 },
		"the storer to hand over the held receipt (spool %d)", box.Spool().Count())
	t.Logf("milo2 inbox: %v", inboxFiles(t, milo2))
	waitFor(t, func() bool {
		r, ok := findReceipt(milo2.receipts, id2)
		return ok && r.From == "niko" && r.FileName == "nap.txt"
	}, "the relayed receipt to land in milo's ledger after his return")
}

// TestReceiptsSurviveRestart pins the ledger's persistence.
func TestReceiptsSurviveRestart(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)
	id, err := milo.Send("fluff", writeSource(t, "persist me"))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		r, ok := findReceipt(milo.receipts, id)
		return ok && r.DeliveredAt > 0
	}, "the receipt to land")
	stopDaemon(milo)
	if _, ok := findReceipt(startDaemonAt(t, milo.cfg.Dir).receipts, id); !ok {
		t.Fatal("receipts ledger did not survive restart")
	}
}

func inboxFiles(t *testing.T, d *Daemon) []string {
	t.Helper()
	des, err := os.ReadDir(d.InboxDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, de := range des {
		out = append(out, filepath.Join(d.InboxDir(), de.Name()))
	}
	return out
}

// TestReceiveRefusedWhenDiskLow pins the free-space check: a sender
// cannot fill the receiver's disk, and the send retries once space
// returns.
func TestReceiveRefusedWhenDiskLow(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	saved := freeSpace
	freeSpace = func(string) (uint64, bool) { return 100, true } // bytes, not mebibytes
	defer func() { freeSpace = saved }()

	src := writeSource(t, "too big for the disk")
	id, err := milo.Send("fluff", src)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, e := range milo.ob.All() {
			if e.ID == id {
				return true
			}
		}
		return false
	}, "the send to stay queued while the receiver refuses")
	if des, _ := os.ReadDir(fluff.InboxDir()); len(des) != 0 {
		t.Fatalf("inbox has %d files, want 0", len(des))
	}

	freeSpace = func(string) (uint64, bool) { return 1 << 40, true }
	waitFor(t, func() bool {
		_, ok := inboxFile(t, fluff, "nap.txt")
		return ok
	}, "the send to deliver once space returns")
}

// TestReceiveBusyRefused pins the global transfer cap: with every slot
// taken the receiver refuses instead of stacking another stream, and
// the send retries once a slot frees.
func TestReceiveBusyRefused(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	// Take every transfer slot: the receiver must refuse instead of
	// stacking another stream.
	for len(fluff.slots) < cap(fluff.slots) {
		fluff.slots <- struct{}{}
	}
	src := writeSource(t, "while busy")
	id, err := milo.Send("fluff", src)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, e := range milo.ob.All() {
			if e.ID == id {
				return true
			}
		}
		return false
	}, "the send to stay queued while the receiver is busy")
	for len(fluff.slots) > 0 {
		<-fluff.slots
	}

	waitFor(t, func() bool {
		_, ok := inboxFile(t, fluff, "nap.txt")
		return ok
	}, "the send to deliver once the slot frees")
}
