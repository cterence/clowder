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

// TestCancelPendingSend pins `clow cancel`: one send by ID, then the
// cancel-all form on a fresh queue.
func TestCancelPendingSend(t *testing.T) {
	milo := startDaemon(t, "milo")
	niko, _ := offlineCat(t)
	addCat(t, milo, niko)

	id, err := milo.Send("niko", writeSource(t, "never mind"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := milo.Cancel(id); err != nil || n != 1 {
		t.Fatalf("Cancel(id) = %d, %v; want 1, nil", n, err)
	}
	if len(milo.ob.All()) != 0 {
		t.Fatal("cancelled send is still queued")
	}

	if _, err := milo.Send("niko", writeSource(t, "never mind either")); err != nil {
		t.Fatal(err)
	}
	if n, err := milo.Cancel(""); err != nil || n != 1 {
		t.Fatalf("Cancel(all) = %d, %v; want 1, nil", n, err)
	}
	if len(milo.ob.All()) != 0 {
		t.Fatal("cancel-all left a send queued")
	}
}

// Cancel must abort the in-flight stream at the wire: the sender's
// connection closes, the receiver's partial file is cleaned up, and
// the file never lands. The old cancel only dropped the outbox entry
// and let the stream run on, stranding the receiver with a .tmp file
// until its stream deadline.
func TestCancelAbortsInFlightStream(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := Init(dirA, "milo"); err != nil {
		t.Fatal(err)
	}
	if err := Init(dirB, "fluff"); err != nil {
		t.Fatal(err)
	}
	sender := runDaemon(t, dirA, &slowTransport{perWrite: 50 * time.Millisecond})
	receiver := runDaemon(t, dirB, &LocalTransport{})
	trust(t, sender, receiver)
	trust(t, receiver, sender)

	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i)
	}
	src := writeSource(t, string(big))
	id, err := sender.Send("fluff", src)
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the stream is mid-flight, then cancel it.
	waitFor(t, func() bool {
		for _, p := range sender.prog.snapshot() {
			if p.Done > 64*1024 {
				return true
			}
		}
		return false
	}, "the transfer to be streaming")
	if _, err := sender.Cancel(id); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return len(sender.prog.snapshot()) == 0 },
		"the cancelled attempt to end promptly")
	// The receiver must have nothing: no finished file, no .tmp.
	des, err := os.ReadDir(receiver.InboxDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(des) != 0 {
		var names []string
		for _, de := range des {
			names = append(names, de.Name())
		}
		t.Fatalf("receiver kept files after the cancel: %v", names)
	}
}
