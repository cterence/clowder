package daemon

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"clowder/protocol"
)

// TestSendRefusesDuplicateName pins the ambiguous-name refusal: a name
// claimed by two keys is not a safe send target, so Send refuses
// instead of silently picking the newest claimant.
func TestSendRefusesDuplicateName(t *testing.T) {
	milo := startDaemon(t, "milo")
	niko1, _ := offlineCat(t)
	niko2, _ := offlineCat(t) // same name, different identity
	addCat(t, milo, niko1)
	addCat(t, milo, niko2)

	_, err := milo.Send("niko", writeSource(t, "nap"))
	if err == nil {
		t.Fatal("send to a duplicated name succeeded, want refusal")
	}
}

// TestJoinRefusesTakenName pins the joiner-side name check: a joiner
// whose roster already holds the inviter's name under another key
// refuses before writing its ack — the inviter commits on that ack, so
// the refusal must precede it.
func TestJoinRefusesTakenName(t *testing.T) {
	joiner := startDaemon(t, "milo")
	squatter, _ := offlineCat(t) // "niko", another key
	addCat(t, joiner, squatter)
	inviter := startDaemon(t, "niko") // same name, different identity
	// A real tailcat address, so the joiner can derive the inviter's
	// key from the intro (LocalTransport addresses do not parse).
	inviter.mu.Lock()
	inviter.meCat.Addr = string(inviter.env.Identity.Public.Addr())
	inviter.mu.Unlock()

	c1, c2 := tcpPair(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { _ = c1.Close() }()
		pc := protocol.NewConn(c1)
		// The inviter's half: intro exchange, then no ack may come.
		if _, err := pairIntroOf(pc, inviter.Me()); err != nil {
			t.Errorf("inviter intro exchange: %v", err)
		}
		if _, err := pc.ReadMsg(); err == nil {
			t.Error("joiner wrote an ack despite the name refusal")
		}
	}()
	go func() {
		defer wg.Done()
		defer func() { _ = c2.Close() }()
		pc := protocol.NewConn(c2)
		_, _, err := joinExchange(pc, joiner.Me(), func(name, key string) bool {
			return joiner.ros.NameTaken(name, key)
		})
		if !errors.Is(err, errNameTaken) {
			t.Errorf("joinExchange error = %v, want errNameTaken", err)
		}
	}()
	wg.Wait()
}

// TestSourceDigestCache pins the outbox digest cache: the first
// attempt hashes and caches, unchanged retries reuse it, and a changed
// file re-hashes.
func TestSourceDigestCache(t *testing.T) {
	milo := startDaemon(t, "milo")
	src := writeSource(t, "nap first")
	e := Entry{ID: newID(), TargetKey: "k", TargetName: "fluff",
		SourcePath: src, FileName: "nap.txt"}

	d1, s1, err := milo.sourceDigest(&e)
	if err != nil {
		t.Fatal(err)
	}
	if e.SourceSHA256 != d1 || e.SourceSize != s1 || e.SourceModNs == 0 {
		t.Fatal("digest not cached in the entry")
	}
	d2, s2, err := milo.sourceDigest(&e)
	if err != nil {
		t.Fatal(err)
	}
	if d2 != d1 || s2 != s1 {
		t.Fatal("stable file re-hashed")
	}

	if err := os.WriteFile(src, []byte("changed content"), 0o600); err != nil {
		t.Fatal(err)
	}
	d3, _, err := milo.sourceDigest(&e)
	if err != nil {
		t.Fatal(err)
	}
	if d3 == d1 {
		t.Fatal("changed file served a stale digest")
	}
}

// TestHealthStatsDepths pins the daemon's /stats snapshot against the
// live keepers.
func TestHealthStatsDepths(t *testing.T) {
	milo := startDaemon(t, "milo")
	if got := milo.healthStats(); got != (HealthStats{}) {
		t.Fatalf("fresh daemon stats = %+v, want zero", got)
	}
	// niko's roster entry exists but nothing listens there: the send
	// stays queued, so the outbox depth shows.
	niko, _ := offlineCat(t)
	addCat(t, milo, niko)
	if _, err := milo.Send("niko", writeSource(t, "pending")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if milo.healthStats().Outbox == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("stats outbox = %d, want 1", milo.healthStats().Outbox)
}
