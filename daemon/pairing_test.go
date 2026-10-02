package daemon

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clowder/protocol"
	"clowder/roster"
	"github.com/tailscale/tailcat"
)

func TestPairingWordlist(t *testing.T) {
	if len(pairingWords) != 1024 {
		t.Fatalf("wordlist has %d words, want 1024 (entropy would silently drop)", len(pairingWords))
	}
	seen := map[string]bool{}
	for _, w := range pairingWords {
		if seen[w] {
			t.Fatalf("duplicate word %q", w)
		}
		seen[w] = true
		for _, r := range w {
			if r < 'a' || r > 'z' {
				t.Fatalf("word %q is not lowercase ascii", w)
			}
		}
	}
}

func TestParsePairCode(t *testing.T) {
	words, region, err := parsePairCode("Hazel-Meadow-Quartz-Amber-Ember-Petal-Ivory-Cedar-303")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(words, " ") != "hazel meadow quartz amber ember petal ivory cedar" || region != 303 {
		t.Fatalf("parsed %v, region %d", words, region)
	}

	for _, bad := range []string{
		"hazel meadow quartz amber ember petal ivory 303",           // too few words
		"hazel meadow quartz amber ember petal ivory cedar oak 303", // too many words
		"hazel meadow quartz amber ember petal ivory notaword 303",  // not in list
		"hazel meadow quartz amber ember petal ivory cedar zero",    // bad region
		"hazel meadow quartz amber ember petal ivory cedar",         // no region
		"", // empty
	} {
		if _, _, err := parsePairCode(bad); err == nil {
			t.Errorf("parse(%q) succeeded, want error", bad)
		}
	}
}

func TestDerivePairingDeterministic(t *testing.T) {
	words, _, err := parsePairCode("hazel-meadow-quartz-amber-ember-petal-ivory-cedar-303")
	if err != nil {
		t.Fatal(err)
	}
	a, err := derivePairing(words)
	if err != nil {
		t.Fatal(err)
	}
	b, err := derivePairing(words)
	if err != nil {
		t.Fatal(err)
	}
	if a.inviterPub != b.inviterPub || a.joinerPub != b.joinerPub || !a.psk.Equal(b.psk) {
		t.Fatal("derivation is not deterministic")
	}
	if a.inviterPub == a.joinerPub {
		t.Fatal("inviter and joiner identities collide")
	}
	if a.inviterPub.IsZero() || a.inviterDisco.IsZero() {
		t.Fatal("derived keys are zero")
	}

	other, _, err := parsePairCode("hazel-meadow-quartz-amber-ember-petal-cedar-ivory-303")
	if err != nil {
		t.Fatal(err)
	}
	c, err := derivePairing(other)
	if err != nil {
		t.Fatal(err)
	}
	if c.inviterPub == a.inviterPub {
		t.Fatal("different words derived the same inviter key")
	}
}

// TestPairIntroExchange drives the pairing exchange over an in-memory
// pipe: the joiner and inviter sides learn each other's real entries and
// add them to their rosters.
func TestPairIntroExchange(t *testing.T) {
	inviter := startDaemon(t, "milo")
	joiner := startDaemon(t, "fluff")
	// The test transport reports loopback addresses, which are not
	// valid tailcat addresses; give each cat its identity-derived one
	// for the exchange.
	for _, d := range []*Daemon{inviter, joiner} {
		withRealAddr(d)
	}

	// A TCP loopback pair, not net.Pipe: the exchange has both sides
	// writing their intro before reading, which deadlocks on unbuffered
	// pipes. Real connections (tailcat, TCP) are buffered.
	c1, c2 := tcpPair(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { _ = c1.Close() }()
		inviter.servePairConn(c1)
	}()
	go func() {
		defer wg.Done()
		defer func() { _ = c2.Close() }()
		pc := protocol.NewConn(c2)
		peer, confirmed, err := joinExchange(pc, joiner.Me(), nil)
		if err != nil {
			t.Errorf("joiner exchange: %v", err)
			return
		}
		if !confirmed {
			t.Error("joiner exchange completed without the inviter's confirmation")
		}
		if err := joiner.addPeerCat(peer); err != nil {
			t.Errorf("joiner addPeerCat: %v", err)
		}
	}()
	wg.Wait()

	for _, tc := range []struct {
		d       *Daemon
		name    string
		wantKey string
	}{
		{inviter, "fluff", joiner.Me().Key},
		{joiner, "milo", inviter.Me().Key},
	} {
		got, ok := tc.d.Roster().Get(tc.name)
		if !ok {
			t.Errorf("%s did not add %s to its roster", tc.d.Me().Name, tc.name)
			continue
		}
		if got.Key != tc.wantKey {
			t.Errorf("%s's entry for %s has key %s, want %s", tc.d.Me().Name, tc.name, got.Key, tc.wantKey)
		}
		if got.Addr == "" {
			t.Errorf("%s's entry for %s has no address", tc.d.Me().Name, tc.name)
		}
		// The pairing exchange counts as contact: liveness must be
		// recorded immediately, not wait for the sync ticker.
		if tc.d.SeenAt(tc.wantKey) == 0 {
			t.Errorf("%s did not mark %s as seen after pairing", tc.d.Me().Name, tc.name)
		}
	}
}

// tcpPair returns a connected loopback TCP pair, closed at test end.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	type pair struct{ a net.Conn }
	ch := make(chan pair, 1)
	go func() {
		a, err := ln.Accept()
		if err != nil {
			return
		}
		ch <- pair{a: a}
	}()
	b, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p := <-ch
	t.Cleanup(func() { _ = p.a.Close() })
	t.Cleanup(func() { _ = b.Close() })
	return p.a, b
}

// TestIdentityJSONRoundTrip pins the identity file format: a daemon
// restart must restore the exact same node key.
func TestIdentityJSONRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, "milo"); err != nil {
		t.Fatal(err)
	}
	env, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := env.Identity.Public.ServerPublic.String()

	// Re-read from disk as the daemon would after a restart.
	b, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var k *tailcat.PrivateKey
	if err := json.Unmarshal(b, &k); err != nil {
		t.Fatal(err)
	}
	if k.Public.ServerPublic.String() != before {
		t.Fatal("identity round trip changed the node key")
	}

	// Roster entries derived from the address must keep matching it.
	c, err := roster.NewCat("milo", string(env.Identity.Public.Addr()), 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.Key != before {
		t.Fatal("address-derived key does not match the identity key")
	}
}

// TestPairIntroUnconfirmedDoesNotCommit pins the inviter's half of the
// recovery contract: a joiner that received the intro but never
// confirmed (or whose reply was lost on the wire) must leave the
// inviter unpaired and the invite alive for the next attempt. CI lost
// the inviter's reply on flapping relay paths; committing on the
// unconfirmed exchange stranded the joiner with a retired invite.
func TestPairIntroUnconfirmedDoesNotCommit(t *testing.T) {
	inviter := startDaemon(t, "milo")
	joiner := startDaemon(t, "fluff")
	for _, d := range []*Daemon{inviter, joiner} {
		withRealAddr(d)
	}
	c1, c2 := tcpPair(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { _ = c1.Close() }()
		inviter.servePairConn(c1)
	}()
	go func() {
		defer wg.Done()
		defer func() { _ = c2.Close() }()
		pc := protocol.NewConn(c2)
		if _, err := pairIntroOf(pc, joiner.Me()); err != nil {
			t.Errorf("joiner half exchange: %v", err)
		}
		// No ack: the reply path dies here, as it did on CI.
	}()
	wg.Wait()

	if _, ok := inviter.Roster().Get("fluff"); ok {
		t.Error("inviter committed a pairing the joiner never confirmed")
	}
}

// TestPairIntroCommitsDespiteLostConfirmation pins the inviter's other
// recovery property: once the joiner's ack arrives, the inviter commits
// even if its own confirming reply is lost — the ack is the proof the
// joiner saw the intro, and the joiner commits optimistically on the
// ack-write having succeeded.
func TestPairIntroCommitsDespiteLostConfirmation(t *testing.T) {
	inviter := startDaemon(t, "milo")
	joiner := startDaemon(t, "fluff")
	for _, d := range []*Daemon{inviter, joiner} {
		withRealAddr(d)
	}
	c1, c2 := tcpPair(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { _ = c1.Close() }()
		inviter.servePairConn(c1)
	}()
	go func() {
		defer wg.Done()
		defer func() { _ = c2.Close() }()
		pc := protocol.NewConn(c2)
		if _, err := pairIntroOf(pc, joiner.Me()); err != nil {
			t.Errorf("joiner half exchange: %v", err)
			return
		}
		// The ack goes out, then the joiner vanishes before reading
		// the inviter's confirmation.
		if err := pc.WriteMsg(&protocol.Message{PairAck: &protocol.PairAck{}}); err != nil {
			t.Errorf("joiner ack: %v", err)
		}
	}()
	wg.Wait()

	if _, ok := inviter.Roster().Get("fluff"); !ok {
		t.Error("inviter did not commit a pairing the joiner confirmed")
	}
}

// TestPairingRefusesDuplicateName pins the inviter's name-uniqueness
// check: a join claiming a name another key already has is refused
// with an error the joiner understands (not a dropped connection),
// the roster is untouched, and the invite stays active for a
// differently-named joiner.
func TestPairingRefusesDuplicateName(t *testing.T) {
	inviter := startDaemon(t, "milo")
	squatter := startDaemon(t, "fluff")  // claims the name first
	pretender := startDaemon(t, "fluff") // same name, different identity
	niche := startDaemon(t, "niche")     // a legit later joiner
	for _, d := range []*Daemon{inviter, squatter, pretender, niche} {
		withRealAddr(d)
	}
	addCat(t, inviter, squatter.Me())

	c1, c2 := tcpPair(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { _ = c1.Close() }()
		inviter.servePairConn(c1)
	}()
	go func() {
		defer wg.Done()
		defer func() { _ = c2.Close() }()
		pc := protocol.NewConn(c2)
		if _, _, err := joinExchange(pc, pretender.Me(), nil); !errors.Is(err, errPairRefused) {
			t.Errorf("duplicate-name join error = %v, want errPairRefused", err)
		}
	}()
	wg.Wait()

	// The roster is unchanged: still exactly one fluff, and it is the
	// squatter.
	got, ok := inviter.Roster().Get("fluff")
	if !ok || got.Key != squatter.Me().Key {
		t.Fatalf("roster after refusal = %+v, want the original claimant", got)
	}

	// The invite survived the refusal: a differently-named cat still
	// pairs on a fresh connection.
	c3, c4 := tcpPair(t)
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { _ = c3.Close() }()
		inviter.servePairConn(c3)
	}()
	go func() {
		defer wg.Done()
		defer func() { _ = c4.Close() }()
		pc := protocol.NewConn(c4)
		if _, _, err := joinExchange(pc, niche.Me(), nil); err != nil {
			t.Errorf("post-refusal join: %v", err)
		}
	}()
	wg.Wait()
	if _, ok := inviter.Roster().Get("niche"); !ok {
		t.Fatal("the differently-named joiner was not added")
	}
}

// The join op is a blocking RPC: it replies with the pairing's
// outcome, not an in-progress status.
func TestJoinIPCSynchronous(t *testing.T) {
	a := startDaemon(t, "a")
	// Unparseable on purpose: the attempt fails at the code check, so
	// the loopback suite stays off the DERP network.
	resp := a.handleIPC(Request{Op: "join", Words: "not a pairing code"})
	if resp.OK {
		t.Fatalf("join with a bad code = %+v, want failure", resp)
	}
	if !strings.Contains(resp.Error, "pairing code") {
		t.Fatalf("join error = %q, want the code check to speak up", resp.Error)
	}
}

// The inviter pushes its full roster (liveness included) with the join
// confirmation, so the fresh joiner is visibly alive immediately
// instead of showing every cat "never seen" until a sync connects.
func TestJoinRosterPush(t *testing.T) {
	inviter := startDaemon(t, "milo")
	joiner := startDaemon(t, "fluff")
	third, _ := offlineCat(t)
	addCat(t, inviter, third)
	inviter.markSeen(third.Key)
	for _, d := range []*Daemon{inviter, joiner} {
		withRealAddr(d)
	}

	c1, c2 := tcpPair(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { _ = c1.Close() }()
		inviter.servePairConn(c1)
	}()
	go func() {
		defer wg.Done()
		defer func() { _ = c2.Close() }()
		pc := protocol.NewConn(c2)
		peer, _, err := joinExchange(pc, joiner.Me(), nil)
		if err != nil {
			t.Errorf("joiner exchange: %v", err)
			return
		}
		if err := joiner.addPeerCat(peer); err != nil {
			t.Errorf("joiner addPeerCat: %v", err)
		}
		joiner.absorbRosterPush(pc)
	}()
	wg.Wait()

	if _, ok := joiner.Roster().GetByKey(third.Key); !ok {
		t.Error("joiner did not absorb the inviter's roster push")
	}
	if joiner.SeenAt(third.Key) == 0 {
		t.Error("joiner did not adopt the inviter's liveness")
	}
}

// A pairing fans out immediately: the inviter syncs its peers the
// moment it commits the joiner, so every online cat has the full roster
// without waiting a poll tick. The tick here is raised so only the
// event-driven sync can carry the new cat in time.
func TestPairingFansOutRosterSync(t *testing.T) {
	old := testPollEvery
	testPollEvery = 30 * time.Second
	t.Cleanup(func() { testPollEvery = old })

	inviter := startDaemon(t, "milo")
	third := startDaemon(t, "niche")
	joiner := startDaemon(t, "fluff")
	trust(t, inviter, third)
	trust(t, third, inviter)
	for _, d := range []*Daemon{inviter, joiner} {
		withRealAddr(d)
	}

	c1, c2 := tcpPair(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { _ = c1.Close() }()
		inviter.servePairConn(c1)
	}()
	go func() {
		defer wg.Done()
		defer func() { _ = c2.Close() }()
		pc := protocol.NewConn(c2)
		peer, _, err := joinExchange(pc, joiner.Me(), nil)
		if err != nil {
			t.Errorf("joiner exchange: %v", err)
			return
		}
		if err := joiner.addPeerCat(peer); err != nil {
			t.Errorf("joiner addPeerCat: %v", err)
		}
		joiner.absorbRosterPush(pc)
	}()
	wg.Wait()

	joinerKey := joiner.Me().Key
	waitFor(t, func() bool { _, ok := third.Roster().GetByKey(joinerKey); return ok },
		"third cat to learn the joiner via the inviter's post-pairing sync")
}

func FuzzParsePairCode(f *testing.F) {
	f.Add("hazel-meadow-quartz-amber-ember-petal-ivory-cedar-303")
	f.Add("hazel meadow quartz amber ember petal ivory cedar 303")
	f.Add("short-code-1")
	f.Add("hazel-meadow-quartz-amber-ember-petal-ivory-notaword-303")
	f.Add("hazel-meadow-quartz-amber-ember-petal-ivory-cedar-zero")
	f.Add("")
	f.Fuzz(func(t *testing.T, code string) {
		if words, region, err := parsePairCode(code); err == nil {
			if len(words) != 8 || region <= 0 {
				t.Fatalf("parsePairCode(%q) accepted invalid shape: %v %d", code, words, region)
			}
		}
	})
}

// The inviter must hold the pairing channel open until the joiner acks
// the roster push: closing with bytes in flight black-holes the
// confirmation over tailcat, and the joiner used to spend its whole
// exchange deadline waiting for a confirmation the inviter had written
// milliseconds earlier (the real-mesh 45s "slow inviter" join).
// Pinned from the joiner's side: a silent joiner keeps the channel
// open for at least the close grace.
func TestPairingHoldsChannelForJoinerAck(t *testing.T) {
	old := pairCloseGrace
	pairCloseGrace = 400 * time.Millisecond
	t.Cleanup(func() { pairCloseGrace = old })

	inviter := startDaemon(t, "milo")
	joiner := startDaemon(t, "fluff")
	for _, d := range []*Daemon{inviter, joiner} {
		withRealAddr(d)
	}

	c1, c2 := tcpPair(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { _ = c1.Close() }()
		inviter.servePairConn(c1)
	}()
	go func() {
		defer wg.Done()
		defer func() { _ = c2.Close() }()
		pc := protocol.NewConn(c2)
		if _, _, err := joinExchange(pc, joiner.Me(), nil); err != nil {
			t.Errorf("joiner exchange: %v", err)
			return
		}
		// Read the roster push, then go silent: no final ack.
		_ = pc.SetDeadline(time.Now().Add(5 * time.Second))
		if m, err := pc.ReadMsg(); err != nil || m.Roster == nil {
			t.Errorf("joiner did not receive the roster push: %v", err)
			return
		}
		start := time.Now()
		if _, err := pc.ReadMsg(); err == nil {
			t.Error("expected EOF after the inviter closed")
		}
		if held := time.Since(start); held < pairCloseGrace-100*time.Millisecond {
			t.Errorf("inviter closed after %s, want it to hold the channel for the joiner's ack (grace %s)", held, pairCloseGrace)
		}
	}()
	wg.Wait()
}

// A re-pair must survive the stale tombstone that still rides the
// mesh from the cat's original leave: the pairing entry arrives
// SIGNED (the joiner signs its intro), so the tombstone's rejoin
// refusal applies and the leaver stays paired. The old unsigned
// entry could not prove the rejoin, and the stale tombstone un-paired
// and re-blocked the cat within seconds — how the ghost state was
// born on the real mesh.
func TestRepairSurvivesStaleTombstone(t *testing.T) {
	inviter := startDaemon(t, "milo")
	fdir := t.TempDir()
	if err := Init(fdir, "fluff"); err != nil {
		t.Fatal(err)
	}
	fluff := startDaemonAt(t, fdir)
	// The loopback transport's address is not a valid tailcat address;
	// pair against the identity-derived one, re-signed so the intro's
	// signature covers it.
	withRealAddr(fluff)

	// The tombstone from fluff's leave AFTER its daemon started —
	// newer than the entry it was created with, as a real
	// leave-then-repair sequence produces — still carried by peers
	// that never saw the re-pair.
	stale := roster.SignTombstone(fluff.env.SignPriv, fluff.Me().Key, time.Now().Add(-2*time.Second).Unix())
	// Join stamps the entry it presents: it must outrank the tombstone.
	fluff.stampMeCat()

	c1, c2 := tcpPair(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { _ = c1.Close() }()
		inviter.servePairConn(c1)
	}()
	go func() {
		defer wg.Done()
		defer func() { _ = c2.Close() }()
		pc := protocol.NewConn(c2)
		if _, _, err := joinExchange(pc, fluff.Me(), nil); err != nil {
			t.Errorf("joiner exchange: %v", err)
		}
	}()
	wg.Wait()

	if _, ok := inviter.Roster().Get("fluff"); !ok {
		t.Fatal("pairing did not add fluff")
	}
	// A peer's sync still carries the stale tombstone.
	inviter.mergeRemote(&protocol.RosterSync{Tombstones: []roster.Tombstone{stale}})
	if _, ok := inviter.Roster().Get("fluff"); !ok {
		t.Fatal("a stale tombstone un-paired a freshly re-paired cat")
	}
	if inviter.isBlockedKey(fluff.Me().Key) {
		t.Fatal("the stale tombstone re-blocked the re-paired cat")
	}
}
