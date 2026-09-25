package daemon

import (
	"context"
	"testing"
	"time"

	"clowder/roster"
)

func TestRotateAnnouncesToRoster(t *testing.T) {
	a := startDaemon(t, "milo")
	b := startDaemon(t, "fluff")
	c := startDaemon(t, "niko")
	trust(t, a, b)
	trust(t, b, a)
	trust(t, a, c)
	trust(t, c, a)

	oldAddr := a.Me().Addr
	envBefore, err := Open(a.cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}

	newAddr, err := a.RotateAddress(context.Background())
	if err != nil {
		t.Fatalf("RotateAddress: %v", err)
	}
	if newAddr == "" || newAddr == oldAddr {
		t.Fatalf("rotated address %q is not new", newAddr)
	}

	// Both peers converge on the rotated address.
	for _, peer := range []*Daemon{b, c} {
		waitFor(t, func() bool {
			e, ok := peer.Roster().GetByKey(a.Me().Key)
			return ok && e.Addr == newAddr
		}, "%s to learn the rotated address", peer.Me().Name)
	}

	// The identity on disk carries a new pre-shared key.
	envAfter, err := Open(a.cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if envAfter.Identity.Public.PresharedKey.Equal(envBefore.Identity.Public.PresharedKey) {
		t.Fatal("rotated identity kept the old pre-shared key")
	}

	// A restarted daemon derives the announced address from the new
	// identity: peers' entries keep matching it.
	a2 := startDaemonAt(t, a.cfg.Dir)
	waitFor(t, func() bool { return a2.Me().Addr != "" }, "rotated milo to restart")
	if a2.Me().Addr == oldAddr {
		t.Fatal("restarted daemon serves the old address")
	}
}

func TestRotateFailsWhenNoPeerReachable(t *testing.T) {
	a := startDaemon(t, "milo")
	niko, _ := offlineCat(t)
	addCat(t, a, niko)

	envBefore, err := Open(a.cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a.RotateAddress(context.Background()); err == nil {
		t.Fatal("rotate succeeded with only an unreachable cat, want failure")
	}

	// Nothing changed: identity untouched, no announcements exist.
	envAfter, err := Open(a.cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !envAfter.Identity.Public.PresharedKey.Equal(envBefore.Identity.Public.PresharedKey) {
		t.Fatal("failed rotation modified the identity")
	}
}

func TestRotateAloneSucceeds(t *testing.T) {
	a := startDaemon(t, "milo") // empty roster: nobody to announce to
	if _, err := a.RotateAddress(context.Background()); err != nil {
		t.Fatalf("rotate with empty roster: %v", err)
	}
}

func TestStatusLiveness(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	// Nobody has talked yet.
	if milo.SeenAt(fluff.Me().Key) != 0 {
		t.Fatal("liveness recorded before any contact")
	}

	src := writeSource(t, "seen nap")
	if _, err := milo.Send("fluff", src); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, ok := inboxFile(t, fluff, "nap.txt")
		return ok
	}, "fluff to receive the file")

	// The sender saw the receiver, and the receiver saw the sender.
	if since := time.Since(time.Unix(milo.SeenAt(fluff.Me().Key), 0)); since > time.Minute {
		t.Fatalf("milo has not seen fluff recently (%s)", since)
	}
	if since := time.Since(time.Unix(fluff.SeenAt(milo.Me().Key), 0)); since > time.Minute {
		t.Fatalf("fluff has not seen milo recently (%s)", since)
	}
	// Unknown cats are never seen.
	ghost := roster.Cat{Key: "nodekey:ghost"}
	if milo.SeenAt(ghost.Key) != 0 {
		t.Fatal("unknown cat has liveness")
	}
}
