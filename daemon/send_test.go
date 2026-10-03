package daemon

import (
	"bytes"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cterence/clowder/envelope"
	"github.com/cterence/clowder/protocol"
	"github.com/cterence/clowder/roster"
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

// TestSendViaStorerSkipsDirect pins --storer: the direct attempt (and
// its dial timeout) is skipped entirely — even an online target gets
// the file through a storer, never straight from the sender. The
// storer's spooled counter is the witness: a direct delivery would
// land on niko without ever touching the storer.
func TestSendViaStorerSkipsDirect(t *testing.T) {
	milo := startDaemon(t, "milo", func(c *Config) { c.PollEvery = time.Hour })
	storer := startDaemon(t, "storer")
	if err := storer.SetStorer(true, 1<<30); err != nil {
		t.Fatal(err)
	}
	niko := startDaemon(t, "niko")
	trust(t, milo, storer)
	trust(t, storer, milo)
	trust(t, milo, niko)
	trust(t, niko, milo)

	if _, err := milo.SendVia("niko", writeSource(t, "nap for an online cat")); err != nil {
		t.Fatalf("SendVia: %v", err)
	}
	waitFor(t, func() bool {
		entries, err := os.ReadDir(niko.InboxDir())
		return err == nil && len(entries) == 1
	}, "niko to receive the file")
	if got := storer.stats.snapshot().Spooled; got != 1 {
		t.Fatalf("storer spooled %d files, want 1 (via-storer must route through the storer)", got)
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
	withRealAddr(inviter)

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

// A receive stream that goes silent mid-flight (sender cancelled,
// killed, or partitioned without closing) must abort at the idle
// deadline and wipe its partial file, not sit out the whole stream
// timeout holding a .tmp.
func TestReceiveIdleStreamIsCleanedUp(t *testing.T) {
	old := streamIdle
	streamIdle = 300 * time.Millisecond
	t.Cleanup(func() { streamIdle = old })

	milo := startDaemon(t, "milo") // supplies the sender identity
	receiver := startDaemon(t, "fluff")
	trust(t, receiver, milo)

	conn, err := net.Dial("tcp", receiver.Me().Addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	pc := protocol.NewConn(conn)
	if err := pc.WriteMsg(&protocol.Message{Hello: &protocol.Hello{
		Name: "milo", Key: milo.Me().Key, DialKey: milo.Me().DialKey,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.ReadMsg(); err != nil {
		t.Fatal(err)
	}
	if err := pc.WriteMsg(&protocol.Message{Roster: &protocol.RosterSync{Cats: []roster.Cat{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.ReadMsg(); err != nil {
		t.Fatal(err)
	}

	data := strings.Repeat("nap", 4096)
	var sealed bytes.Buffer
	targetPub, err := parseKey(receiver.Me().Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := envelope.SealStream(milo.env.Identity.Private, targetPub, &sealed, strings.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	o := &protocol.Offer{
		ID:         strings.Repeat("a", 32),
		FileName:   "nap.txt",
		Size:       int64(sealed.Len()),
		From:       "milo",
		FromKey:    milo.Me().Key,
		TargetKey:  receiver.Me().Key,
		TargetName: "fluff",
	}
	protocol.SignOffer(milo.env.SignPriv, o)
	if err := pc.WriteMsg(&protocol.Message{Offer: o}); err != nil {
		t.Fatal(err)
	}
	m, err := pc.ReadMsg()
	if err != nil || m.Answer == nil || !m.Answer.OK {
		t.Fatalf("receiver refused the offer: %v %+v", err, m)
	}
	// Half the sealed stream, then silence: the connection stays open.
	if _, err := conn.Write(sealed.Bytes()[:sealed.Len()/2]); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		des, err := os.ReadDir(receiver.InboxDir())
		return err == nil && len(des) == 0
	}, "the idle stream's partial file to be wiped")
}

// The status op reports how a transfer settled — delivered directly,
// or stored by a named storer — so the CLI can say which one happened
// instead of hedging.
func TestSettledOutcome(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	src := writeSource(t, "direct nap")
	id, err := milo.Send("fluff", src)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, func() bool { return len(milo.ob.All()) == 0 }, "the direct send to settle")
	resp := milo.handleIPC(Request{Op: "status"})
	if got := resp.Settled[id]; got != "delivered" {
		t.Fatalf("direct send settled as %q, want %q", got, "delivered")
	}

	// An offline target parks the file on a storer: the outcome names it.
	storer := startDaemon(t, "box")
	if err := storer.SetStorer(true, 1<<30); err != nil {
		t.Fatal(err)
	}
	trust(t, milo, storer)
	trust(t, storer, milo)
	niko, _ := offlineCat(t)
	addCat(t, milo, niko)
	addCat(t, storer, niko)

	src2 := writeSource(t, "stored nap")
	id2, err := milo.Send("niko", src2)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, func() bool { return storer.Spool().Count() == 1 }, "the storer to hold the file")
	resp = milo.handleIPC(Request{Op: "status"})
	if got := resp.Settled[id2]; got != "stored via box" {
		t.Fatalf("storer send settled as %q, want %q", got, "stored via box")
	}
}
