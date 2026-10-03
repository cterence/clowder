package daemon

import (
	"context"
	"testing"
	"time"
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

	// The storer holds it this time: the probe confirms and the entry
	// settles for good.
	waitFor(t, func() bool {
		es := milo.ob.All()
		return len(es) == 1 && es[0].HeldBy != ""
	}, "re-delivery to record the holding storer")
	milo.probeHeld(context.Background())
	waitFor(t, func() bool { return len(milo.ob.All()) == 0 }, "confirmed entry to leave the outbox")
}
