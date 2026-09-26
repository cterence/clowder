package daemon

// Pairing lets two cats exchange their real tailcat identities by
// reading a few words to each other instead of copy-pasting ~100-byte
// addresses. `clow invite` derives two ephemeral tailcat identities and
// a shared PSK from five random words; the invitee runs `clow join` with
// the same words, derives the same material, and the two daemons meet on
// a fixed DERP region. Over that throwaway WireGuard tunnel each sends
// a PairIntro carrying its real name and address, and both add each
// other to their rosters. The words never travel, and no private key is
// ever derived by the "wrong" side. Active guessing is hopeless (a
// wrong-word join dies in the WireGuard handshake, invisible to the
// daemon layer, before any protocol byte is exchangeable). But guessing
// is NOT purely active: the handshake's MAC1 is keyed by the inviter's
// word-derived static public key, so a recorded pairing handshake is
// an offline oracle for candidate word codes — see
// pairing_mac1_test.go and AGENTS pending item 1 for the finding,
// the window math, and the pairing-v2 fix.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tailscale/tailcat"
	go4mem "go4.org/mem"
	"golang.org/x/crypto/hkdf"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"

	"clowder/protocol"
	"clowder/roster"
)

const (
	// pairWordCount is the number of words in a pairing code: 50 bits
	// against active guessing at 10 bits per word.
	pairWordCount = 5
	// pairTTL is how long an invite stays valid.
	pairTTL = 5 * time.Minute
	// discoDerivationLabel replicates tailcat's disco key derivation
	// (discoPrivateForNode) so the joiner can compute the inviter's
	// pairing disco public key from the words alone.
	discoDerivationLabel = "github.com/tailscale/tailcat disco key v1"
)

// pairingKeys is the key material both sides derive from the words.
type pairingKeys struct {
	inviterPriv  key.NodePrivate
	inviterPub   key.NodePublic
	inviterDisco tailcat.DiscoPublic
	joinerPriv   key.NodePrivate
	joinerPub    key.NodePublic
	psk          tailcat.PresharedKey
}

// derivePairing derives both pairing identities and the shared PSK from
// the code words. Deterministic: both sides derive identical material.
func derivePairing(words []string) (*pairingKeys, error) {
	if len(words) != pairWordCount {
		return nil, fmt.Errorf("daemon: pairing code has %d words, want %d", len(words), pairWordCount)
	}
	for _, w := range words {
		if !slices.Contains(pairingWords, w) {
			return nil, fmt.Errorf("daemon: %q is not a pairing word", w)
		}
	}

	seed := sha512.Sum512([]byte("clowder/pair/v1\x00" + strings.Join(words, " ")))
	expand := func(label string) []byte {
		r := hkdf.New(sha256.New, seed[:], nil, []byte("clowder/pair/v1/"+label))
		b := make([]byte, 32)
		if _, err := io.ReadFull(r, b); err != nil {
			panic(err) // hkdf from a 64-byte seed cannot fail
		}
		return b
	}

	k := &pairingKeys{}
	var err error
	if k.inviterPriv, err = nodePrivateFromBytes(expand("inviter")); err != nil {
		return nil, err
	}
	if k.joinerPriv, err = nodePrivateFromBytes(expand("joiner")); err != nil {
		return nil, err
	}
	k.inviterPub = k.inviterPriv.Public()
	k.joinerPub = k.joinerPriv.Public()
	copy(k.psk[:], expand("psk"))
	k.inviterDisco = discoPublicForNode(k.inviterPriv)
	return k, nil
}

// nodePrivateFromBytes builds a node private key from 32 raw bytes via
// its text form.
func nodePrivateFromBytes(b []byte) (key.NodePrivate, error) {
	var k key.NodePrivate
	if err := k.UnmarshalText([]byte("privkey:" + hex.EncodeToString(b))); err != nil {
		return key.NodePrivate{}, fmt.Errorf("daemon: deriving node key: %w", err)
	}
	return k, nil
}

// discoPublicForNode replicates tailcat's discoPrivateForNode, deriving
// the disco public key that belongs to a node private key.
func discoPublicForNode(k key.NodePrivate) tailcat.DiscoPublic {
	raw := k.Raw32()
	mac := hmac.New(sha256.New, raw[:])
	mac.Write([]byte(discoDerivationLabel))
	discoRaw := mac.Sum(nil)
	discoRaw[0] &= 248
	discoRaw[31] &= 127
	discoRaw[31] |= 64
	return tailcat.DiscoPublic{DiscoPublic: key.DiscoPrivateFromRaw32(go4mem.B(discoRaw)).Public()}
}

// generatePairWords picks the random pairing code words.
func generatePairWords() ([]string, error) {
	words := make([]string, pairWordCount)
	for i := range words {
		var b [2]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, fmt.Errorf("daemon: generating pairing code: %w", err)
		}
		// 65536 is a multiple of 1024, so this is unbiased.
		idx := (int(b[0])<<8 | int(b[1])) % len(pairingWords)
		words[i] = pairingWords[idx]
	}
	return words, nil
}

// parsePairCode parses a user-supplied pairing code: five words plus
// the inviter's DERP region as a numeric suffix, with words separated
// by spaces, dashes, or other punctuation (e.g.
// "hazel-meadow-quartz-amber-ember-303").
func parsePairCode(code string) (words []string, region int, err error) {
	fields := strings.FieldsFunc(strings.ToLower(code), func(r rune) bool {
		isWord := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		return !isWord && !isDigit
	})
	if len(fields) != pairWordCount+1 {
		return nil, 0, fmt.Errorf("daemon: pairing code has %d fields, want %d words and a region", len(fields), pairWordCount)
	}
	region, err = strconv.Atoi(fields[pairWordCount])
	if err != nil || region <= 0 {
		return nil, 0, fmt.Errorf("daemon: %q is not a DERP region number", fields[pairWordCount])
	}
	words, err = deriveCheckWords(fields[:pairWordCount])
	if err != nil {
		return nil, 0, err
	}
	return words, region, nil
}

func deriveCheckWords(words []string) ([]string, error) {
	if len(words) != pairWordCount {
		return nil, fmt.Errorf("daemon: pairing code has %d words, want %d", len(words), pairWordCount)
	}
	for _, w := range words {
		if !slices.Contains(pairingWords, w) {
			return nil, fmt.Errorf("daemon: %q is not a pairing word", w)
		}
	}
	return words, nil
}

// ---- invite side (daemon state) ----

// Daemon pairing state is in pair.go fields: pairMu, pairSrv, pairLn,
// pairDone.

// fetchDERPMap fetches the DERP map from url, or from tailcat's
// default map URL when url is empty.
func fetchDERPMap(ctx context.Context, url string) (*tailcfg.DERPMap, error) {
	if url == "" {
		return tailcat.FetchDERPMap(ctx)
	}
	return tailcat.FetchDERPMap(ctx, tailcat.DERPMapURL(url))
}

// StartInvite creates a pairing code, starts listening for one joiner
// on the pairing identity, and returns the code. The invite expires
// after [pairTTL] or the first successful join, and a new invite
// replaces an older one.
func (d *Daemon) StartInvite(ctx context.Context) (string, error) {
	words, err := generatePairWords()
	if err != nil {
		return "", err
	}
	keys, err := derivePairing(words)
	if err != nil {
		return "", err
	}

	d.stopInvite()

	// Pick our nearest DERP region explicitly: the joiner must be told
	// which region to meet on, and a resolved tailcat address does not
	// carry its region ID (the wire format zeroes it).
	dm, err := fetchDERPMap(ctx, d.cfg.DERPMapURL)
	if err != nil {
		return "", fmt.Errorf("daemon: fetching DERP map: %w", err)
	}
	region, err := tailcat.PickBestRegion(ctx, dm)
	if err != nil {
		return "", fmt.Errorf("daemon: picking pairing region: %w", err)
	}
	if region == 0 {
		// Netcheck found no latencies; meet on the lowest region ID.
		ids := slices.Sorted(maps.Keys(dm.Regions))
		if len(ids) == 0 {
			return "", errors.New("daemon: DERP map has no regions")
		}
		region = ids[0]
	}

	srv := &tailcat.Server{
		Key:            keys.inviterPriv,
		PresharedKey:   keys.psk,
		Region:         dm.Regions[region],
		AllowedClients: []key.NodePublic{keys.joinerPub},
		Logf:           d.cfg.Logf,
	}
	ln, err := srv.Listen(ctx, "tcp", fmt.Sprintf(":%d", DefaultPort))
	if err != nil {
		return "", fmt.Errorf("daemon: pairing listen: %w", err)
	}

	done := make(chan struct{})
	d.pairMu.Lock()
	d.pairSrv = srv
	d.pairLn = ln
	d.pairDone = done
	d.pairMu.Unlock()

	d.goBg(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			d.goBg(func() { d.servePairConn(conn) })
		}
	})
	d.goBg(func() {
		select {
		case <-time.After(pairTTL):
		case <-done:
		}
		d.stopInvite()
	})

	code := fmt.Sprintf("%s-%d", strings.Join(words, "-"), region)
	d.cfg.logf("clowder: pairing invite %s active for %s", code, pairTTL)
	return code, nil
}

// stopInvite tears down any active pairing listener.
func (d *Daemon) stopInvite() {
	d.pairMu.Lock()
	defer d.pairMu.Unlock()
	if d.pairLn != nil {
		_ = d.pairLn.Close()
	}
	if d.pairSrv != nil {
		_ = d.pairSrv.Close()
	}
	if d.pairDone != nil {
		close(d.pairDone)
	}
	d.pairLn = nil
	d.pairSrv = nil
	d.pairDone = nil
}

// servePairConn exchanges PairIntros with a joiner on the pairing
// channel and adds the peer to our roster.
func (d *Daemon) servePairConn(conn net.Conn) {
	pc := protocol.NewConn(conn)
	defer func() { _ = pc.Close() }()
	unregister := d.trackConn(pc)
	defer unregister()
	_ = pc.SetDeadline(time.Now().Add(2 * time.Minute))

	peer, err := pairIntroOf(pc, d.Me())
	if err != nil {
		d.cfg.logf("clowder: pairing exchange failed: %v", err)
		return
	}
	// Names are the human handle within a clowder: refuse a join that
	// would claim a name another key already has (duplicate names make
	// every name-based lookup a coin flip — see roster Get). The
	// refusal is an explicit message, so the joiner knows not to
	// retry, and the invite stays active for other joiners.
	claim, err := roster.NewCat(peer.Name, peer.Addr, time.Now().Unix())
	if err == nil && d.ros.NameTaken(claim.Name, claim.Key) {
		reason := fmt.Sprintf("name %q is already taken by another cat (re-init with a different name and re-pair)", claim.Name)
		if err := pc.Answer(pairingAnswerID, false, reason); err != nil {
			d.cfg.logf("clowder: refusing duplicate name %s: %v", claim.Name, err)
		}
		d.cfg.logf("clowder: refused pairing with %s: %s", claim.Name, reason)
		// The joiner races its ack against this refusal. Drain it
		// before closing: a close with unread data in flight sends an
		// RST that destroys the refusal still on the wire (CI's Linux
		// runner), and the joiner must get to read the refusal.
		_ = pc.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = pc.ReadMsg()
		return
	}
	// Commit only on the joiner's confirmation: a reply it never saw
	// must leave the invite alive for its next attempt. CI lost the
	// inviter's reply on flapping relay paths; committing on the
	// unconfirmed exchange paired the inviter and retired the invite
	// while the joiner was stranded with nothing to retry against.
	if err := pairAckOf(pc); err != nil {
		d.cfg.logf("clowder: pairing with %s not confirmed, invite stays active: %v", peer.Name, err)
		return
	}
	if err := d.addPeerCat(peer); err != nil {
		d.cfg.logf("clowder: adding paired cat: %v", err)
		return
	}
	d.cfg.logf("clowder: paired with %s", peer.Name)
	// Confirm the commit, so the joiner returns knowing the pairing is
	// durable on both sides. A lost confirmation does not un-pair it:
	// the joiner commits optimistically once its own ack was written.
	if err := pc.WriteMsg(&protocol.Message{PairAck: &protocol.PairAck{}}); err != nil {
		d.cfg.logf("clowder: confirming pairing with %s: %v", peer.Name, err)
	}
	// One confirmed pairing: retire the invite.
	d.goBg(d.stopInvite)
}

// addPeerCat records a cat from a PairIntro in the roster and allows it
// to connect.
func (d *Daemon) addPeerCat(p *protocol.PairIntro) error {
	c, err := roster.NewCat(p.Name, p.Addr, time.Now().Unix())
	if err != nil {
		return err
	}
	c.ClientKey = p.ClientKey
	c.Storer = p.Storer
	c.Dropbox = p.Dropbox
	if err := d.ros.Add(c); err != nil {
		return err
	}
	d.allowCat(c)
	// We just talked to this cat over the pairing channel: it is very
	// much "seen".
	d.markSeen(c.Key)
	return nil
}

// ---- join side ----

// pairRegions returns the DERP regions a joiner should try to reach an
// inviter on, in order. The encoded region leads and is RETRIED
// between every other region: a pairing server still attaching to its
// relay (slow machines) is caught by the next encoded-region retry, a
// server whose relay presence flapped onto another region is caught
// when that region's turn comes, and a healthy meeting still answers
// on the first attempt. Other regions follow ascending by ID.
func pairRegions(ctx context.Context, derpMapURL string, encoded int) ([]int, error) {
	dm, err := fetchDERPMap(ctx, derpMapURL)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(dm.Regions))
	for id := range dm.Regions {
		ids = append(ids, int(id))
	}
	slices.Sort(ids)
	out := make([]int, 0, 2*len(ids)+1)
	out = append(out, encoded)
	for _, id := range ids {
		if id != encoded {
			out = append(out, id, encoded)
		}
	}
	return out, nil
}

// pairingAnswerID fills the Answer's ID on the pairing channel, where
// there is no transfer to identify — the joiner reads only OK/Reason.
const pairingAnswerID = "pairing"

// errPairRefused marks an inviter's definitive refusal (e.g. the
// joiner's name is already claimed): retrying other regions or
// reconnects cannot help, so the join aborts instead of sweeping.
var errPairRefused = errors.New("inviter refused the pairing")

// joinTimeout bounds a whole join attempt: the encoded-region try plus
// however much of the sweep fits.
const joinTimeout = 2 * time.Minute

// Join connects to the inviter's pairing channel with the given code
// and exchanges real identities. It tries the region encoded in the
// code first, then sweeps the other regions (see pairRegions): each
// attempt is bounded, and the whole join gives up after joinTimeout.
func (d *Daemon) Join(ctx context.Context, code string) error {
	words, region, err := parsePairCode(code)
	if err != nil {
		return err
	}
	keys, err := derivePairing(words)
	if err != nil {
		return err
	}

	regions, err := pairRegions(ctx, d.cfg.DERPMapURL, region)
	if err != nil {
		// No map to sweep with: fall back to the encoded region only,
		// the pre-sweep behavior.
		d.cfg.logf("clowder: fetching DERP map for the pairing sweep: %v", err)
		regions = []int{region}
	}

	deadline := time.Now().Add(joinTimeout)
	var lastErr error
	for _, reg := range regions {
		if time.Now().After(deadline) {
			break
		}
		peer, err := d.pairOnRegion(ctx, keys, reg, deadline)
		if err != nil {
			if errors.Is(err, errPairRefused) {
				return fmt.Errorf("daemon: %w", err)
			}
			lastErr = err
			d.cfg.logf("clowder: pairing attempt on region %d failed: %v", reg, err)
			continue
		}
		if err := d.addPeerCat(peer); err != nil {
			return err
		}
		// Discover the rest of the clowder now: the protocol handshake
		// exchanges full rosters, so this sync pulls in every cat the
		// inviter knows instead of waiting for the next poll tick.
		d.goBg(func() { d.syncPeers(context.WithoutCancel(ctx)) })
		d.cfg.logf("clowder: paired with %s", peer.Name)
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("daemon: reaching inviter (is `clow invite` still active?): %w", lastErr)
	}
	return errors.New("daemon: could not reach the inviter before the invite expired (is `clow invite` still active?)")
}

// pairOnRegion reaches the inviter's pairing channel on one DERP
// region and exchanges intros over it. Each stage is bounded so a
// region where the inviter is absent costs seconds, not minutes.
func (d *Daemon) pairOnRegion(ctx context.Context, keys *pairingKeys, region int, overall time.Time) (*protocol.PairIntro, error) {
	ci := tailcat.ConnInfo{
		ServerPublic:      tailcat.NodePublic{NodePublic: keys.inviterPub},
		ServerDiscoPublic: keys.inviterDisco,
		PresharedKey:      keys.psk,
		RegionID:          tailcfg.DERPRegionID(region),
	}
	c := &tailcat.Client{
		Server:     ci.Addr(),
		Key:        keys.joinerPriv,
		DERPMapURL: d.cfg.DERPMapURL,
		Logf:       d.cfg.Logf,
	}
	defer func() { _ = c.Close() }()

	// A meow ping answers only where the inviter's pairing server is
	// actually connected, so a wrong region fails here in seconds. A
	// live server answers in well under a second; the bound only needs
	// to cover the relay round trip, not server startup (the sweep
	// retries the encoded region instead).
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := c.Ping(pingCtx); err != nil {
		return nil, err
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDial()
	conn, err := c.DialTCPPort(dialCtx, DefaultPort)
	if err != nil {
		return nil, err
	}
	pc := protocol.NewConn(conn)
	defer func() { _ = pc.Close() }()
	// Bound the exchange, but generously: a slow inviter can take tens
	// of seconds to answer (CI showed ~35s between accept and reply),
	// so a tight deadline strands the joiner. And since the inviter no
	// longer commits — or retires its invite — until the exchange is
	// confirmed, a timed-out attempt can be retried on a fresh
	// connection instead of eating the whole join budget.
	exchangeDeadline := time.Now().Add(45 * time.Second)
	if overall.Before(exchangeDeadline) {
		exchangeDeadline = overall
	}
	_ = pc.SetDeadline(exchangeDeadline)
	peer, confirmed, err := joinExchange(pc, d.Me())
	if err == nil && !confirmed {
		d.cfg.logf("clowder: pairing confirmation lost after our ack; committing optimistically")
	}
	return peer, err
}

// joinExchange runs the joiner's half of a confirmed pairing exchange:
// intro out, the inviter's intro in, an ack telling the inviter its reply
// arrived, and the inviter's confirmation that it committed. The inviter
// commits only after our ack, and confirms with its own; losing that
// confirmation does not un-pair us — our ack was written, so the inviter
// has everything it needs — so the exchange reports complete either way.
func joinExchange(pc *protocol.Conn, me roster.Cat) (*protocol.PairIntro, bool, error) {
	peer, err := pairIntroOf(pc, me)
	if err != nil {
		return nil, false, err
	}
	ackErr := pc.WriteMsg(&protocol.Message{PairAck: &protocol.PairAck{}})
	readErr := pairAckOf(pc)
	switch {
	case readErr == nil:
		return peer, true, nil
	case errors.Is(readErr, errPairRefused):
		// The refusal may have raced our ack write; what we read is
		// the accurate diagnosis.
		return nil, false, readErr
	case ackErr != nil:
		// The ack never landed (the inviter likely refused and closed
		// while we were writing), so there is nothing to commit
		// optimistically on.
		return nil, false, fmt.Errorf("%w (ack write: %v)", readErr, ackErr)
	default:
		// Our ack was written, so the inviter has everything it needs
		// to commit; a lost confirmation does not un-pair us.
		return peer, false, nil
	}
}

// pairAckOf waits for the peer's confirmation on the pairing channel:
// a PairAck from the joiner, or — on the joiner's side — the inviter's
// commit confirmation, or its explicit refusal.
func pairAckOf(pc *protocol.Conn) error {
	m, err := pc.ReadMsg()
	if err != nil {
		return err
	}
	if m.PairAck != nil {
		return nil
	}
	if m.Answer != nil && !m.Answer.OK {
		return fmt.Errorf("%w: %s", errPairRefused, m.Answer.Reason)
	}
	return fmt.Errorf("expected pair ack, got %s", m.Kind())
}

// pairIntroOf sends our intro and returns the peer's.
func pairIntroOf(pc *protocol.Conn, me roster.Cat) (*protocol.PairIntro, error) {
	if err := pc.WriteMsg(&protocol.Message{Pair: &protocol.PairIntro{
		Name:      me.Name,
		Addr:      me.Addr,
		ClientKey: me.ClientKey,
		Storer:    me.Storer,
		Dropbox:   me.Dropbox,
	}}); err != nil {
		return nil, err
	}
	m, err := pc.ReadMsg()
	if err != nil {
		return nil, err
	}
	if m.Pair == nil || m.Pair.Name == "" || m.Pair.Addr == "" {
		return nil, errors.New("peer sent no pairing intro")
	}
	return m.Pair, nil
}
