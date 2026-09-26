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
	words, region, err := parsePairCode("Hazel-Meadow-Quartz-Amber-Ember-303")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(words, " ") != "hazel meadow quartz amber ember" || region != 303 {
		t.Fatalf("parsed %v, region %d", words, region)
	}

	for _, bad := range []string{
		"hazel meadow quartz amber 303",           // too few words
		"hazel meadow quartz amber ember oak 303", // too many words
		"hazel meadow quartz amber notaword 303",  // not in list
		"hazel meadow quartz amber ember zero",    // bad region
		"hazel meadow quartz amber ember",         // no region
		"",                                        // empty
	} {
		if _, _, err := parsePairCode(bad); err == nil {
			t.Errorf("parse(%q) succeeded, want error", bad)
		}
	}
}

func TestDerivePairingDeterministic(t *testing.T) {
	words, _, err := parsePairCode("hazel-meadow-quartz-amber-ember-303")
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

	other, _, err := parsePairCode("hazel-meadow-quartz-ember-petal-303")
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
		d.mu.Lock()
		d.meCat.Addr = string(d.env.Identity.Public.Addr())
		d.mu.Unlock()
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
		peer, confirmed, err := joinExchange(pc, joiner.Me())
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
		d.mu.Lock()
		d.meCat.Addr = string(d.env.Identity.Public.Addr())
		d.mu.Unlock()
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
		d.mu.Lock()
		d.meCat.Addr = string(d.env.Identity.Public.Addr())
		d.mu.Unlock()
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
		d.mu.Lock()
		d.meCat.Addr = string(d.env.Identity.Public.Addr())
		d.mu.Unlock()
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
		if _, _, err := joinExchange(pc, pretender.Me()); !errors.Is(err, errPairRefused) {
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
		if _, _, err := joinExchange(pc, niche.Me()); err != nil {
			t.Errorf("post-refusal join: %v", err)
		}
	}()
	wg.Wait()
	if _, ok := inviter.Roster().Get("niche"); !ok {
		t.Fatal("the differently-named joiner was not added")
	}
}
