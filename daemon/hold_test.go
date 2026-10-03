package daemon

import (
	"context"
	"crypto/ed25519"
	"net"
	"testing"
	"time"

	"github.com/cterence/clowder/protocol"
)

// TestAcceptAndDropStorerCaughtByProbe pins the storer hold-probe fix:
// an AckStored no longer ends the sender's responsibility. The outbox
// entry survives until a probe confirms the storer still holds the
// file — a storer that accepted and dropped it gets the file again.
func TestAcceptAndDropStorerCaughtByProbe(t *testing.T) {
	// Poll drives the automatic probe pass; this test drives probes by
	// hand so the drop is caught deterministically.
	milo := startDaemon(t, "milo", func(c *Config) { c.PollEvery = time.Hour })
	storer := startDaemon(t, "storer", func(c *Config) { c.SpoolSweepEvery = time.Hour })
	if err := storer.SetStorer(true, 1<<30); err != nil {
		t.Fatal(err)
	}
	trust(t, milo, storer)
	trust(t, storer, milo)
	niko, _ := offlineCat(t)
	addCat(t, milo, niko)
	addCat(t, storer, niko)

	src := writeSource(t, "nap for a sleeping cat")
	if _, err := milo.Send("niko", src); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, func() bool { return storer.Spool().Count() == 1 }, "storer to hold the file")
	waitFor(t, func() bool {
		es := milo.ob.All()
		return len(es) == 1 && es[0].HeldBy != ""
	}, "outbox entry to record the holding storer")

	// The storer loses the file; the probe notices and re-queues it.
	if err := storer.Spool().Delete(milo.ob.All()[0].ID); err != nil {
		t.Fatalf("dropping spooled file: %v", err)
	}
	milo.probeHeld(context.Background())
	waitFor(t, func() bool {
		es := milo.ob.All()
		return len(es) == 1 && es[0].HeldBy == ""
	}, "entry to be re-queued after the storer dropped it")
	waitFor(t, func() bool { return storer.Spool().Count() == 1 }, "retry to re-deliver the file")

	// The storer holds it this time: the probe confirms — but a
	// confirmed hold no longer deletes the entry (#8): only the
	// recipient's signed receipt does.
	waitFor(t, func() bool {
		es := milo.ob.All()
		return len(es) == 1 && es[0].HeldBy != ""
	}, "re-delivery to record the holding storer")
	milo.probeHeld(context.Background())
	waitFor(t, func() bool {
		es := milo.ob.All()
		return len(es) == 1 && es[0].HeldBy != ""
	}, "confirmed hold to keep the entry (a receipt must clear it)")
}

// TestSignedReceiptClearsOutbox pins #8: an AckStored (even a probed
// one) is not delivery — the entry leaves the outbox only on the
// recipient's signed receipt, and a forged receipt clears nothing.
func TestSignedReceiptClearsOutbox(t *testing.T) {
	milo := startDaemon(t, "milo", func(c *Config) { c.PollEvery = time.Hour })
	storer := startDaemon(t, "storer", func(c *Config) { c.SpoolSweepEvery = time.Hour })
	if err := storer.SetStorer(true, 1<<30); err != nil {
		t.Fatal(err)
	}
	trust(t, milo, storer)
	trust(t, storer, milo)
	niko, nikoDir := offlineCat(t)
	addCat(t, milo, niko)
	addCat(t, storer, niko)

	if _, err := milo.Send("niko", writeSource(t, "nap for a sleeping cat")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, func() bool {
		es := milo.ob.All()
		return len(es) == 1 && es[0].HeldBy != ""
	}, "storer to hold the file")
	milo.probeHeld(context.Background())
	waitFor(t, func() bool {
		es := milo.ob.All()
		return len(es) == 1 && es[0].HeldBy != ""
	}, "probe to keep the entry (a receipt must clear it)")

	// A forged receipt must not clear the entry.
	id := milo.ob.All()[0].ID
	_, wrong, _ := ed25519.GenerateKey(nil)
	forged := &protocol.Receipt{ID: id, TargetKey: niko.Key, SignKey: "00"}
	protocol.SignReceipt(wrong, forged)
	client, server := net.Pipe()
	go func() {
		_ = milo.handleReceipt(protocol.NewConn(server), forged)
		_ = server.Close()
	}()
	cpc := protocol.NewConn(client)
	if m, err := cpc.ReadMsg(); err != nil || m.Answer == nil || m.Answer.OK {
		t.Fatalf("forged receipt accepted: %v %+v", err, m)
	}
	_ = client.Close()
	if es := milo.ob.All(); len(es) != 1 {
		t.Fatalf("forged receipt cleared the outbox (%d entries)", len(es))
	}

	// niko wakes and the storer delivers: niko's receipt — pushed on
	// its sync round, signed by niko's sign key — clears the entry.
	nikoD := startDaemonAt(t, nikoDir)
	addCat(t, nikoD, milo.Me())
	trust(t, nikoD, storer)
	trust(t, storer, nikoD)
	storer.sweepSpoolFor(context.Background(), niko.Key)
	waitFor(t, func() bool { return len(milo.ob.All()) == 0 }, "the signed receipt to clear the outbox")
}
