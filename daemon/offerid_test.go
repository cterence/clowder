package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clowder/protocol"
)

// TestOfferWithBadIDRefused pins the transfer-ID guard: IDs name spool
// and parts files, so anything but our own 32-hex format — traversal
// sequences included — must be refused before any file is touched.
func TestOfferWithBadIDRefused(t *testing.T) {
	fluff := startDaemon(t, "fluff")
	for _, id := range []string{
		"",
		"foo",
		"../../evil",
		"a/b",
		"..\\evil",
		strings.Repeat("x", 32), // right length, not hex
		strings.Repeat("a", 31), // hex, wrong length
	} {
		client, server := net.Pipe()
		pc := protocol.NewConn(server)
		go func() {
			_ = fluff.handleOffer(pc, &protocol.Hello{Name: "milo"}, &protocol.Offer{
				ID: id, FileName: "nap.txt", Size: 10,
				TargetKey: fluff.Me().Key, TargetName: "fluff",
			})
			_ = server.Close()
		}()
		cpc := protocol.NewConn(client)
		m, err := cpc.ReadMsg()
		if err != nil {
			t.Fatalf("ID %q: reading answer: %v", id, err)
		}
		if m.Answer == nil || m.Answer.OK {
			t.Fatalf("ID %q was accepted", id)
		}
		if m.Answer.Reason != "invalid transfer ID" {
			t.Fatalf("ID %q: reason %q", id, m.Answer.Reason)
		}
		_ = client.Close()
	}
	// Nothing may have been created on disk for any refused ID.
	for _, dir := range []string{"parts", "spool"} {
		if des, _ := os.ReadDir(filepath.Join(fluff.cfg.Dir, dir)); len(des) != 0 {
			t.Fatalf("%s dir has %d entries, want 0", dir, len(des))
		}
	}
}
