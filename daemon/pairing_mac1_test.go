package daemon

// The MAC1 offline-guess finding, pinned deliberately. The pairing
// file comment used to claim guessing was "active-only, nothing
// verifiable offline". That claim was FALSE, and this test is the
// evidence: it reproduces, from the words alone and without any
// network, the exact keyed MAC that guards every pairing handshake.
//
// The attack: anyone who can record one pairing handshake on the
// invite's DERP region (the relay operator, or whoever compromises a
// relay, can record one) holds (message bytes, mac1). wireguard-go
// keys that MAC with blake2s("mac1----" || responderStaticPub) — and
// in clowder the responder (the inviter's pairing server) derives
// its static public key from the code words. So a candidate word code
// can be tested entirely offline: derive, hash, MAC, compare.
//
// Mitigations (see AGENTS pending item 1): the invite's TTL and
// one-off-ness bound the exploit window — the words must be cracked
// before the pairing completes or expires (~5 minutes). The naive
// per-candidate cost is one X25519 plus two blake2s (the benchmark
// below measures our full derive at ~66µs; an attacker's leaner
// inviter-only pipeline is tens of µs). The sharper threat was
// precomputation: a table of inviterPub for every word code is
// universal (built once, reusable against any future capture),
// after which each candidate test is two blake2s and a big enough
// farm could fit the whole space inside the TTL — at five words
// (50 bits) that was borderline feasible for a well-resourced
// adversary. That is why pairWordCount is now EIGHT (80 bits): a
// precomputed 2^80 table of X25519s is beyond any realistic
// adversary, and the code stays read-over-the-phone speakable. The
// principled fix remains pairing v2 (random pairing-server key
// carried in the code, PAKE over the resulting unauthenticated
// tunnel), which removes the oracle entirely — revisit only if a
// non-speakable invite code ever becomes acceptable.

import (
	"bytes"
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/blake2s"
	"tailscale.com/types/key"
)

// pairingMAC1 mirrors wireguard-go's CookieGenerator.Init + the MAC
// written into every handshake message: key =
// blake2s("mac1----" || responderStaticPub), mac = blake2s-128(key,
// message). Nothing here touches the network.
func pairingMAC1(responderStaticPub key.NodePublic, msg []byte) []byte {
	h, _ := blake2s.New256(nil)
	h.Write([]byte("mac1----"))
	pub, err := hex.DecodeString(responderStaticPub.UntypedHexString())
	if err != nil {
		panic("node public key: " + err.Error())
	}
	h.Write(pub)
	var macKey [32]byte
	h.Sum(macKey[:0])

	mac, _ := blake2s.New128(macKey[:])
	mac.Write(msg)
	return mac.Sum(nil)
}

// TestPairingMAC1OfflineVerifiable demonstrates the offline oracle:
// given a captured handshake message and its MAC1, testing a
// candidate word code needs no interaction with either cat.
func TestPairingMAC1OfflineVerifiable(t *testing.T) {
	words, err := deriveCheckWords([]string{
		pairingWords[1], pairingWords[5], pairingWords[9],
		pairingWords[13], pairingWords[21], pairingWords[29],
		pairingWords[37], pairingWords[41],
	})
	if err != nil {
		t.Fatalf("deriveCheckWords: %v", err)
	}
	k, err := derivePairing(words)
	if err != nil {
		t.Fatalf("derivePairing: %v", err)
	}

	// The captured bytes: a handshake initiation's body up to the MACs
	// (the MAC covers everything before it). The shape of these bytes
	// does not matter to the oracle, only their value on the wire.
	capturedMsg := bytes.Repeat([]byte{0x7f}, 116)
	capturedMAC := pairingMAC1(k.inviterPub, capturedMsg)

	// An offline candidate tester reproduces the MAC from the words
	// alone — this is the whole attack.
	guess, err := derivePairing(words)
	if err != nil {
		t.Fatalf("derivePairing: %v", err)
	}
	if got := pairingMAC1(guess.inviterPub, capturedMsg); !bytes.Equal(got, capturedMAC) {
		t.Fatal("candidate test with the true words did not reproduce the captured MAC1")
	}

	// Wrong words do not match: the oracle is precise.
	wrong := append([]string{}, words...)
	wrong[0] = pairingWords[(1+17)%len(pairingWords)]
	if w, err := deriveCheckWords(wrong); err == nil {
		if g, err := derivePairing(w); err == nil {
			if got := pairingMAC1(g.inviterPub, capturedMsg); bytes.Equal(got, capturedMAC) {
				t.Fatal("candidate test with wrong words reproduced the captured MAC1")
			}
		}
	}
}

// BenchmarkPairingCandidateTest measures one offline guess through
// the full derive-and-MAC pipeline (an attacker's leaner
// inviter-only pipeline skips roughly half of it, but keeps the
// X25519). ~66µs per candidate on this machine.
func BenchmarkPairingCandidateTest(b *testing.B) {
	words, err := deriveCheckWords([]string{
		pairingWords[1], pairingWords[5], pairingWords[9],
		pairingWords[13], pairingWords[21], pairingWords[29],
		pairingWords[37], pairingWords[41],
	})
	if err != nil {
		b.Fatalf("deriveCheckWords: %v", err)
	}
	msg := bytes.Repeat([]byte{0x7f}, 116)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k, err := derivePairing(words)
		if err != nil {
			b.Fatalf("derivePairing: %v", err)
		}
		pairingMAC1(k.inviterPub, msg)
	}
}
