package daemon

import (
	"testing"
)

func TestDropboxMode(t *testing.T) {
	milo := startDaemon(t, "milo")
	box := startDaemon(t, "box")
	niko, nikoDir := offlineCat(t)

	if err := box.SetDropbox(true, 1<<30); err != nil {
		t.Fatal(err)
	}
	trust(t, milo, box)
	trust(t, box, milo)
	addCat(t, milo, niko)
	addCat(t, box, niko)

	// A dropbox cannot originate sends.
	src := writeSource(t, "box cannot send this")
	if _, err := box.Send("milo", src); err == nil {
		t.Fatal("dropbox Send succeeded, want refusal")
	}

	// A dropbox refuses files addressed to itself: the send stays
	// queued on the sender.
	if _, err := milo.Send("box", src); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, e := range milo.ob.All() {
			if e.TargetKey == box.Me().Key {
				return true
			}
		}
		return false
	}, "the direct-to-dropbox send to stay queued")

	// A dropbox still takes third-party deposits and pushes them when
	// the target wakes.
	offline := writeSource(t, "nap via dropbox")
	if _, err := milo.Send("niko", offline); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return box.Spool().Count() == 1 }, "dropbox to hold the deposit")

	nikoD := startDaemonAt(t, nikoDir)
	trust(t, nikoD, box)
	trust(t, box, nikoD)
	waitFor(t, func() bool {
		got, ok := inboxFile(t, nikoD, "nap.txt")
		return ok && got == "nap via dropbox"
	}, "niko to receive via the dropbox's push sweep")
	waitFor(t, func() bool { return box.Spool().Count() == 0 }, "dropbox to drop the delivered file")

	// The dropbox flag propagates like the storer flag: milo learns
	// box's mode through the roster exchange above.
	waitFor(t, func() bool {
		c, ok := milo.Roster().GetByKey(box.Me().Key)
		return ok && c.Dropbox && c.Storer
	}, "milo to see box as a dropbox")

	// Turning storer mode off clears dropbox as well.
	if err := box.SetStorer(false, 0); err != nil {
		t.Fatal(err)
	}
	if box.Me().Dropbox || box.Me().Storer {
		t.Fatal("storer off did not clear dropbox mode")
	}

	// And modes survive a restart (persisted in me.json).
	box2 := startDaemonAt(t, box.cfg.Dir)
	if box2.Me().Storer || box2.Me().Dropbox {
		t.Fatal("mode reappeared after restart")
	}
}
