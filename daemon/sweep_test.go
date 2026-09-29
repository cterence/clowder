package daemon

import (
	"testing"
	"time"
)

// The spool sweep owns its cadence, decoupled from the roster poll:
// a storer on a slow poll box must still push a held file as soon as
// the target wakes — a cat with the app open should not wait a
// minute for a file the storer is holding.
func TestSpoolSweepDecoupledFromPoll(t *testing.T) {
	milo := startDaemon(t, "milo")
	storer := startDaemon(t, "storer", func(c *Config) {
		c.PollEvery = time.Minute // slow: no poll tick during the test
	})
	if err := storer.SetStorer(true, 1<<30); err != nil {
		t.Fatal(err)
	}
	trust(t, milo, storer)
	trust(t, storer, milo)

	// Offline target, known to both; milo's send parks it on the storer.
	niko, nikoDir := offlineCat(t)
	addCat(t, milo, niko)
	addCat(t, storer, niko)
	src := writeSource(t, "nap for a sleeping cat")
	if _, err := milo.Send("niko", src); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, func() bool { return storer.Spool().Count() == 1 }, "storer to hold the file")

	// The cat wakes; the push must arrive on the sweep, not the poll.
	nikoD := startDaemonAt(t, nikoDir, func(c *Config) {
		c.PollEvery = time.Minute
	})
	trust(t, nikoD, storer)
	trust(t, storer, nikoD)
	waitFor(t, func() bool {
		got, ok := inboxFile(t, nikoD, "nap.txt")
		return ok && got == "nap for a sleeping cat"
	}, "the held file to push on the sweep alone")
}
