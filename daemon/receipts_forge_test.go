package daemon

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"tailscale.com/types/key"

	"clowder/envelope"
	"clowder/protocol"
	"clowder/roster"
)

// TestForgedReceiptRejected pins receipt verification: a receipt is
// only recorded when it carries a valid signature from the pinned sign
// key of the cat it names — a sealed digest alone proves nothing, since
// anyone can seal to the sender's public key.
func TestForgedReceiptRejected(t *testing.T) {
	milo := startDaemon(t, "milo")

	// fluff: a roster entry with a pinned sign key (no daemon needed).
	fluffDir := t.TempDir()
	if err := Init(fluffDir, "fluff"); err != nil {
		t.Fatal(err)
	}
	fluffEnv, err := Open(fluffDir)
	if err != nil {
		t.Fatal(err)
	}
	addCat(t, milo, roster.Cat{
		Name:    "fluff",
		Addr:    "127.0.0.1:1",
		Key:     fluffEnv.Identity.Public.ServerPublic.String(),
		SignKey: hex.EncodeToString(fluffEnv.SignPriv.Public().(ed25519.PublicKey)),
		Updated: time.Now().Unix(),
	})

	// deliverReceipt hands a sealed receipt stream to milo's
	// receiveReceipt and reports whether the ledger recorded it.
	deliverReceipt := func(signWith ed25519.PrivateKey, from string) bool {
		env := receiptEnvelope{ID: "receipt-id", FileName: "nap.txt", DeliveredAt: time.Now().Unix()}
		if signWith != nil {
			env.Sig = ed25519.Sign(signWith, receiptPayload(env))
		}
		payload, err := cbor.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		var sealed bytes.Buffer
		recipient := milo.env.Identity.Public.ServerPublic.NodePublic
		if _, _, err := envelope.SealStream(key.NewNode(), recipient, &sealed, bytes.NewReader(payload)); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(payload)

		client, server := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			pc := protocol.NewConn(server)
			_ = milo.handleOffer(pc, &protocol.Hello{Name: from}, &protocol.Offer{
				ID: newID(), FileName: "nap.txt", Size: int64(sealed.Len()),
				SHA256: hex.EncodeToString(sum[:]), From: from,
				TargetKey: milo.Me().Key, Receipt: true,
			})
			_ = server.Close()
		}()
		cpc := protocol.NewConn(client)
		m, err := cpc.ReadMsg()
		if err != nil || m.Answer == nil || !m.Answer.OK {
			t.Fatalf("receipt offer refused: %v %+v", err, m)
		}
		if _, err := cpc.Writer().Write(sealed.Bytes()); err != nil {
			t.Fatal(err)
		}
		m, err = cpc.ReadMsg() // the ack, or EOF on rejection
		acked := err == nil && m.Ack != nil
		_ = client.Close()
		<-done
		_, ok := findReceipt(milo.receipts, "receipt-id")
		return ok && acked
	}

	if deliverReceipt(fluffEnv.SignPriv, "fluff") {
		t.Log("validly signed receipt recorded")
	} else {
		t.Fatal("validly signed receipt was not recorded")
	}
	milo.receipts.mu.Lock()
	delete(milo.receipts.m, "receipt-id")
	milo.receipts.mu.Unlock()

	// Wrong signer: the signature does not match fluff's pinned key.
	if deliverReceipt(ed25519.NewKeyFromSeed(make([]byte, 32)), "fluff") {
		t.Fatal("receipt signed by the wrong key was recorded")
	}
	// No signature at all.
	if deliverReceipt(nil, "fluff") {
		t.Fatal("unsigned receipt was recorded")
	}
	// Unknown sender name.
	if deliverReceipt(fluffEnv.SignPriv, "niko") {
		t.Fatal("receipt from an unknown cat was recorded")
	}
}
