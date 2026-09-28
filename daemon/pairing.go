package daemon

// Pairing: two cats exchange real tailcat identities by reading eight
// words to each other. Both sides derive ephemeral keys and a shared PSK
// from the words, meet on a fixed DERP region, and swap PairIntros over
// the throwaway tunnel; the words never travel. Wrong words die in the
// WireGuard handshake — but its MAC1 is keyed by the word-derived static
// key, so a recorded handshake is an offline oracle for candidate codes;
// see the "Why 8 words" invariant in AGENTS.md and pairing_mac1_test.go.

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
	// 80 bits. Eight also because MAC1 makes a recorded handshake an
	// offline oracle (see AGENTS.md).
	pairWordCount = 8
	pairTTL       = 5 * time.Minute
	// Replicates tailcat's disco key derivation (discoPrivateForNode).
	discoDerivationLabel = "github.com/tailscale/tailcat disco key v1"
)

type pairingKeys struct {
	inviterPriv  key.NodePrivate
	inviterPub   key.NodePublic
	inviterDisco tailcat.DiscoPublic
	joinerPriv   key.NodePrivate
	joinerPub    key.NodePublic
	psk          tailcat.PresharedKey
}

func derivePairing(words []string) (*pairingKeys, error) {
	if _, err := deriveCheckWords(words); err != nil {
		return nil, err
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

func nodePrivateFromBytes(b []byte) (key.NodePrivate, error) {
	var k key.NodePrivate
	if err := k.UnmarshalText([]byte("privkey:" + hex.EncodeToString(b))); err != nil {
		return key.NodePrivate{}, fmt.Errorf("daemon: deriving node key: %w", err)
	}
	return k, nil
}

// discoPublicForNode replicates tailcat's discoPrivateForNode.
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

// parsePairCode parses eight words plus the inviter's DERP region as a
// numeric suffix, punctuation-tolerant (e.g. hazel-meadow-...-cedar-303).
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

// fetchDERPMap fetches the map from url, or tailcat's default when empty.
func fetchDERPMap(ctx context.Context, url string) (*tailcfg.DERPMap, error) {
	if url == "" {
		return tailcat.FetchDERPMap(ctx)
	}
	return tailcat.FetchDERPMap(ctx, tailcat.DERPMapURL(url))
}

// StartInvite creates a code and listens for one joiner on the pairing
// identity. A new invite replaces an older one; expiry is pairTTL or the
// first successful join.
func (d *Daemon) StartInvite(ctx context.Context) (string, error) {
	// The entry we present must outrank any tombstone from an earlier
	// leave of ours, or the re-pair cannot stick against it.
	d.stampMeCat()
	words, err := generatePairWords()
	if err != nil {
		return "", err
	}
	keys, err := derivePairing(words)
	if err != nil {
		return "", err
	}

	d.stopInvite()

	// The joiner must be told the region; a resolved address does not carry it.
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

func (d *Daemon) servePairConn(conn net.Conn) {
	started := time.Now()
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
	d.cfg.logf("clowder: pairing intro exchange with %s took %s", peer.Name, time.Since(started).Round(time.Millisecond))
	// Refuse a name another key already claims (name lookups would be a coin
	// flip); the refusal is explicit and the invite stays active.
	claim, err := roster.NewCat(peer.Name, peer.Addr, time.Now().Unix())
	if err == nil && d.ros.NameTaken(claim.Name, claim.Key) {
		reason := fmt.Sprintf("name %q is already taken by another cat (re-init with a different name and re-pair)", claim.Name)
		if err := pc.Answer(pairingAnswerID, false, reason); err != nil {
			d.cfg.logf("clowder: refusing duplicate name %s: %v", claim.Name, err)
		}
		d.cfg.logf("clowder: refused pairing with %s: %s", claim.Name, reason)
		// Drain the joiner's racing ack before closing: an RST would destroy
		// the refusal still on the wire.
		_ = pc.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = pc.ReadMsg()
		return
	}
	// Commit only on the joiner's ack, so a lost reply leaves the invite alive.
	if err := pairAckOf(pc); err != nil {
		d.cfg.logf("clowder: pairing with %s not confirmed, invite stays active: %v", peer.Name, err)
		return
	}
	d.cfg.logf("clowder: pairing ack from %s took %s", peer.Name, time.Since(started).Round(time.Millisecond))
	if err := d.addPeerCat(peer); err != nil {
		d.cfg.logf("clowder: adding paired cat: %v", err)
		return
	}
	// Fan out now: pairing is rare, and online cats should have the
	// full roster without waiting a poll tick.
	d.goBg(func() { d.syncPeers(context.Background()) })
	d.cfg.logf("clowder: paired with %s, exchange took %s", peer.Name, time.Since(started).Round(time.Millisecond))
	// Confirm the commit. A lost confirmation does not un-pair: the joiner
	// commits optimistically once its ack was written.
	if err := pc.WriteMsg(&protocol.Message{PairAck: &protocol.PairAck{}}); err != nil {
		d.cfg.logf("clowder: confirming pairing with %s: %v", peer.Name, err)
	}
	// Push the full roster with the confirmation (liveness included), so
	// the fresh joiner starts alive instead of "never seen" until a sync.
	rs := d.rosterMsg()
	rs.Liveness = d.livenessSnapshot()
	if err := pc.WriteMsg(&protocol.Message{Roster: rs}); err != nil {
		d.cfg.logf("clowder: pushing the roster to %s: %v", peer.Name, err)
	}
	// Hold the channel open until the joiner acks the push, then close:
	// closing with bytes still in the tunnel black-holes them, and the
	// joiner used to wait out its whole exchange deadline for a
	// confirmation the inviter had written milliseconds earlier.
	_ = pc.SetDeadline(time.Now().Add(pairCloseGrace))
	_, _ = pc.ReadMsg()
	d.goBg(d.stopInvite)
}

// addPeerCat records a paired cat. The pairing is the trust root: the
// peer's sign key is pinned here, any old tombstone is cleared, and a cat
// that had left merges again.
func (d *Daemon) addPeerCat(p *protocol.PairIntro) error {
	c, err := roster.NewCat(p.Name, p.Addr, p.Updated)
	if err != nil {
		return err
	}
	c.ClientKey = p.ClientKey
	c.SignKey = p.SignKey
	c.Storer = p.Storer
	c.Dropbox = p.Dropbox
	c.Capacity = p.Capacity
	// The joiner signs its intro, so the entry lands verbatim and a
	// stale tombstone from an old leave cannot un-pair the re-join
	// (the rejoin refusal needs a signed newer entry). A bad or absent
	// signature falls back to the unsigned pairing of older joiners.
	c.Sig = p.Sig
	if p.Sig != nil && !roster.VerifyEntry(c) {
		c.Sig = nil
		d.cfg.logf("clowder: %s's pairing intro carries an invalid signature, storing the entry unsigned", p.Name)
	}
	if err := d.ros.Add(c); err != nil {
		return err
	}
	d.ros.ClearTombstone(c.Key)
	// Re-pairing is the way back from forget: clear the blocklist entry.
	d.mu.Lock()
	delete(d.blocked, c.Key)
	if c.ClientKey != "" {
		delete(d.blocked, c.ClientKey)
	}
	blockErr := saveBlocked(d.cfg.Dir, d.blocked)
	d.left = false
	d.mu.Unlock()
	if blockErr != nil {
		d.cfg.logf("clowder: clearing blocklist after pairing %s: %v", c.Name, blockErr)
	}
	d.allowCat(c)
	d.markSeen(c.Key)
	return nil
}

// pairRegions returns the regions a joiner should try, in order: the
// encoded region leads and is retried between every other region
// (ascending), catching slow relay attaches and flapped presence.
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

const pairingAnswerID = "pairing"

// A definitive inviter refusal: abort instead of sweeping regions.
var errPairRefused = errors.New("inviter refused the pairing")

// The inviter's name collides with one we already hold: also definitive,
// no region can help.
var errNameTaken = errors.New("name already claimed by another cat")

const joinTimeout = 2 * time.Minute

// stampMeCat re-signs the local entry with a fresh Updated, so the
// entry a cat presents at pairing is newer than any tombstone from an
// earlier leave — without it, a leave-then-repair sequence loses to
// the leave's tombstone and the re-pair is silently un-paired.
func (d *Daemon) stampMeCat() {
	d.mu.Lock()
	d.meCat.Updated = time.Now().Unix()
	d.meCat = roster.SignCat(d.env.SignPriv, d.meCat)
	d.mu.Unlock()
}

// Join tries the encoded region first, then the sweep (pairRegions); the
// whole join gives up after joinTimeout.
func (d *Daemon) Join(ctx context.Context, code string) error {
	// The entry we present must outrank any tombstone from an earlier
	// leave of ours, or the re-pair cannot stick against it.
	d.stampMeCat()
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
		// No map to sweep with: encoded region only.
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
			if errors.Is(err, errPairRefused) || errors.Is(err, errNameTaken) {
				return fmt.Errorf("daemon: %w", err)
			}
			lastErr = err
			d.cfg.logf("clowder: pairing attempt on region %d failed: %v", reg, err)
			continue
		}
		if err := d.addPeerCat(peer); err != nil {
			return err
		}
		// Pull the inviter's full roster now instead of waiting for a poll tick.
		d.goBg(func() { d.syncPeers(context.WithoutCancel(ctx)) })
		d.cfg.logf("clowder: paired with %s", peer.Name)
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("daemon: reaching inviter (is `clow invite` still active?): %w", lastErr)
	}
	return errors.New("daemon: could not reach the inviter before the invite expired (is `clow invite` still active?)")
}

func (d *Daemon) pairOnRegion(ctx context.Context, keys *pairingKeys, region int, overall time.Time) (*protocol.PairIntro, error) {
	started := time.Now()
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

	// A wrong region fails the ping in seconds; the sweep retries the
	// encoded region.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := c.Ping(pingCtx); err != nil {
		return nil, err
	}
	d.cfg.logf("clowder: pairing ping on region %d took %s", region, time.Since(started).Round(time.Millisecond))
	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDial()
	dialed := time.Now()
	conn, err := c.DialTCPPort(dialCtx, DefaultPort)
	if err != nil {
		return nil, err
	}
	d.cfg.logf("clowder: pairing dial on region %d took %s", region, time.Since(dialed).Round(time.Millisecond))
	exchanged := time.Now()
	pc := protocol.NewConn(conn)
	defer func() { _ = pc.Close() }()
	// Generous bound: a slow inviter takes tens of seconds; an unconfirmed
	// attempt stays retryable on a fresh connection.
	exchangeDeadline := time.Now().Add(45 * time.Second)
	if overall.Before(exchangeDeadline) {
		exchangeDeadline = overall
	}
	_ = pc.SetDeadline(exchangeDeadline)
	peer, confirmed, err := joinExchange(pc, d.Me(), func(name, key string) bool {
		return d.ros.NameTaken(name, key)
	})
	d.cfg.logf("clowder: pairing exchange took %s (confirmed=%v)", time.Since(exchanged).Round(time.Millisecond), err == nil && confirmed)
	if err == nil && !confirmed {
		d.cfg.logf("clowder: pairing confirmation lost after our ack; committing optimistically")
	}
	if err == nil {
		// The inviter pushes its roster with the confirmation; a peer
		// that does not (older daemon) just closes, and we still pair.
		d.absorbRosterPush(pc)
	}
	return peer, err
}

// absorbRosterPush merges the roster (and liveness) the inviter pushes
// over the pairing connection, then acks: the inviter closes the
// channel on that ack, so the push and the pairing confirmation cannot
// be black-holed by a close with bytes in flight.
func (d *Daemon) absorbRosterPush(pc *protocol.Conn) {
	_ = pc.SetDeadline(time.Now().Add(5 * time.Second))
	m, err := pc.ReadMsg()
	if err != nil || m.Roster == nil {
		return
	}
	d.mergeRemote(m.Roster)
	d.mergeLiveness(m.Roster.Liveness)
	_ = pc.WriteMsg(&protocol.Message{PairAck: &protocol.PairAck{}})
}

// pairCloseGrace bounds how long the inviter waits for the joiner's
// final ack before closing anyway (an older joiner sends none).
var pairCloseGrace = 2 * time.Second

// mergeLiveness adopts the push's last-seen times, keeping the newer of
// local and remote — a display hint, not a trust decision.
func (d *Daemon) mergeLiveness(l map[string]int64) {
	if len(l) == 0 {
		return
	}
	d.mu.Lock()
	for k, v := range l {
		if v > d.liveness[k] {
			d.liveness[k] = v
		}
	}
	d.mu.Unlock()
}

// joinExchange is the joiner's half: intro out, their intro in, our ack,
// their confirmation. nameTaken, when set, refuses the pairing before
// the ack — the inviter commits on it, so a refusal must precede it —
// when the inviter's name collides with one we already hold. A lost
// confirmation still pairs us: our ack was written.
func joinExchange(pc *protocol.Conn, me roster.Cat, nameTaken func(name, key string) bool) (*protocol.PairIntro, bool, error) {
	peer, err := pairIntroOf(pc, me)
	if err != nil {
		return nil, false, err
	}
	if nameTaken != nil {
		// Same skip-on-parse as the inviter side (servePairConn): the
		// key derives from the address, so an unparseable one (never
		// in production) leaves nothing to check.
		if claim, err := roster.NewCat(peer.Name, peer.Addr, time.Now().Unix()); err == nil && nameTaken(claim.Name, claim.Key) {
			return nil, false, fmt.Errorf("%w: %q is already claimed by another cat (re-init with a fresh name)", errNameTaken, claim.Name)
		}
	}
	ackErr := pc.WriteMsg(&protocol.Message{PairAck: &protocol.PairAck{}})
	readErr := pairAckOf(pc)
	switch {
	case readErr == nil:
		return peer, true, nil
	case errors.Is(readErr, errPairRefused):
		return nil, false, readErr
	case ackErr != nil:
		return nil, false, fmt.Errorf("%w (ack write: %v)", readErr, ackErr)
	default:
		return peer, false, nil
	}
}

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

func pairIntroOf(pc *protocol.Conn, me roster.Cat) (*protocol.PairIntro, error) {
	if err := pc.WriteMsg(&protocol.Message{Pair: &protocol.PairIntro{
		Name:      me.Name,
		Addr:      me.Addr,
		ClientKey: me.ClientKey,
		SignKey:   me.SignKey,
		Storer:    me.Storer,
		Dropbox:   me.Dropbox,
		Capacity:  me.Capacity,
		Updated:   me.Updated,
		Sig:       me.Sig,
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
