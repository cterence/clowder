package daemon

// Real-DERP integration tests. These hit the network (tailcat.dev's
// DERP map and relays), so they never run in CI: set CLOWDER_INTEGRATION=1
// to run them, e.g.
//
//	CLOWDER_INTEGRATION=1 go test ./daemon/ -run Integration -v -count=1 -timeout 10m
//
// They exercise what the loopback transport cannot: real pairing over
// DERP, the transport-level PeerKey authentication against the claimed
// hello client key, and a file transfer between two real tailcat
// stacks.

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func integrationEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("CLOWDER_INTEGRATION") == "" {
		t.Skip("set CLOWDER_INTEGRATION=1 to run real-DERP integration tests")
	}
}

// startRealDaemon runs a daemon with the production tailcat transport.
func startRealDaemon(t *testing.T, dir, name string) *Daemon {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // keep the default inbox in the sandbox
	if err := Init(dir, name); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg := Config{Dir: dir, RetryEvery: 2 * time.Second, PollEvery: 2 * time.Second, Logf: t.Logf}
	env, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tr := NewTailcatTransport(env.Identity, env.ClientIdentity, DefaultPort, t.Logf)
	d, err := New(cfg, tr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = tr.Close()
	})
	go func() { _ = d.Run(ctx) }()
	waitFor(t, func() bool { return d.Me().Addr != "" }, "%s to listen", name)
	return d
}

// TestIntegrationBigFile pushes a multi-gigabyte-class payload shape
// through the real path: a 64 MiB random file over DERP (or direct if
// NAT traversal succeeds), asserting integrity and — critically —
// exactly ONE completed transfer: the retry ticker must not duplicate
// a transfer that outlives it.
func TestIntegrationBigFile(t *testing.T) {
	integrationEnabled(t)
	dirA := t.TempDir()
	dirB := t.TempDir()
	a := startRealDaemon(t, dirA, "hostA")
	b := startRealDaemon(t, dirB, "hostB")

	code, err := a.StartInvite(context.Background())
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := b.Join(context.Background(), code); err != nil {
		t.Fatalf("join: %v", err)
	}

	const size = 64 << 20
	big := make([]byte, size)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(src, big, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("sending %s over the real path", HumanBytes(size))
	if _, err := a.Send("hostB", src); err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if got, ok := inboxFile(t, b, "big.bin"); ok && len(got) == size {
			// Integrity: byte-for-byte.
			for i := range big {
				if got[i] != big[i] {
					t.Fatalf("corruption at offset %d", i)
				}
			}
			if st := a.stats.snapshot(); st.Sent != 1 {
				t.Errorf("sender completed %d transfers, want 1", st.Sent)
			}
			if st := b.stats.snapshot(); st.Received != 1 {
				t.Errorf("receiver completed %d transfers, want 1", st.Received)
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("big file did not arrive within 4 minutes")
}
func TestIntegrationPairSend(t *testing.T) {
	integrationEnabled(t)
	dirA := t.TempDir()
	dirB := t.TempDir()
	a := startRealDaemon(t, dirA, "hostA")
	b := startRealDaemon(t, dirB, "hostB")

	code, err := a.StartInvite(context.Background())
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := b.Join(context.Background(), code); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, ok := a.Roster().Get("hostB"); !ok {
		t.Fatal("hostA did not add hostB")
	}
	if _, ok := b.Roster().Get("hostA"); !ok {
		t.Fatal("hostB did not add hostA")
	}

	src := filepath.Join(t.TempDir(), "nap.txt")
	if err := os.WriteFile(src, []byte("real derp nap"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send("hostB", src); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, func() bool {
		got, ok := inboxFile(t, b, "nap.txt")
		return ok && got == "real derp nap"
	}, "hostB to receive the file over real DERP")

	// A second send goes over a fresh connection: exercises the hello
	// client-key authentication again and any re-handshake state.
	src2 := filepath.Join(t.TempDir(), "nap2.txt")
	if err := os.WriteFile(src2, []byte("second nap"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send("hostB", src2); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	waitFor(t, func() bool {
		got, ok := inboxFile(t, b, "nap2.txt")
		return ok && got == "second nap"
	}, "hostB to receive the second file")
}
