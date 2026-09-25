package daemon

// Pairing lets two cats exchange their real tailcat identities by
// reading a few words to each other instead of copy-pasting ~100-byte
// addresses. `clow invite` derives two ephemeral tailcat identities and
// a shared PSK from five random words; the invitee runs `clow join` with
// the same words, derives the same material, and the two daemons meet on
// a fixed DERP region. Over that throwaway WireGuard tunnel each sends
// a PairIntro carrying its real name and address, and both add each
// other to their rosters. The words never travel, no private key is
// ever derived by the "wrong" side, and guessing is active-only: wrong
// words fail the meow handshake, with nothing verifiable offline.

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
	dm, err := tailcat.FetchDERPMap(ctx)
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

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.servePairConn(conn)
		}
	}()
	go func() {
		select {
		case <-time.After(pairTTL):
		case <-done:
		}
		d.stopInvite()
	}()

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
	_ = pc.SetDeadline(time.Now().Add(2 * time.Minute))

	peer, err := pairIntroOf(pc, d.Me())
	if err != nil {
		d.cfg.logf("clowder: pairing exchange failed: %v", err)
		return
	}
	if err := d.addPeerCat(peer); err != nil {
		d.cfg.logf("clowder: adding paired cat: %v", err)
		return
	}
	d.cfg.logf("clowder: paired with %s", peer.Name)
	// One successful pairing: retire the invite.
	go d.stopInvite()
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
	if err := d.ros.Add(c); err != nil {
		return err
	}
	d.allowCat(c)
	return nil
}

// ---- join side ----

// Join connects to the inviter's pairing channel with the given code
// and exchanges real identities.
func (d *Daemon) Join(ctx context.Context, code string) error {
	words, region, err := parsePairCode(code)
	if err != nil {
		return err
	}
	keys, err := derivePairing(words)
	if err != nil {
		return err
	}

	ci := tailcat.ConnInfo{
		ServerPublic:      tailcat.NodePublic{NodePublic: keys.inviterPub},
		ServerDiscoPublic: keys.inviterDisco,
		PresharedKey:      keys.psk,
		RegionID:          tailcfg.DERPRegionID(region),
	}
	c := &tailcat.Client{
		Server: ci.Addr(),
		Key:    keys.joinerPriv,
		Logf:   d.cfg.Logf,
	}
	conn, err := c.DialTCPPort(ctx, DefaultPort)
	if err != nil {
		return fmt.Errorf("daemon: reaching inviter (is `clow invite` still active?): %w", err)
	}
	pc := protocol.NewConn(conn)
	defer func() { _ = pc.Close() }()
	_ = pc.SetDeadline(time.Now().Add(2 * time.Minute))

	peer, err := pairIntroOf(pc, d.Me())
	if err != nil {
		return fmt.Errorf("daemon: pairing exchange: %w", err)
	}
	if err := d.addPeerCat(peer); err != nil {
		return err
	}
	d.cfg.logf("clowder: paired with %s", peer.Name)
	return nil
}

// pairIntroOf sends our intro and returns the peer's.
func pairIntroOf(pc *protocol.Conn, me roster.Cat) (*protocol.PairIntro, error) {
	if err := pc.WriteMsg(&protocol.Message{Pair: &protocol.PairIntro{
		Name:      me.Name,
		Addr:      me.Addr,
		ClientKey: me.ClientKey,
		Storer:    me.Storer,
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
