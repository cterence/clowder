package daemon

// Real-DERP integration tests. These hit the network (tailcat.dev's
// DERP map and relays): set CLOWDER_INTEGRATION=1 to run them locally,
// e.g.
//
//	CLOWDER_INTEGRATION=1 go test ./daemon/ -run Integration -v -count=1 -timeout 10m
//
// The CI workflow runs them in the integration job on every push and
// PR. They exercise what the loopback transport cannot: real pairing
// over DERP, the transport-level PeerKey authentication against the
// claimed hello client key, and a file transfer between two real
// tailcat stacks.

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

// engineGoroutines counts goroutines belonging to tailcat WireGuard
// engines: they exist only while an engine is alive, so an engine that
// is never closed (or never finishes closing) shows up as growth.
func engineGoroutines(t *testing.T) int {
	t.Helper()
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 1); err != nil {
		t.Fatalf("goroutine profile: %v", err)
	}
	return strings.Count(buf.String(), "created by github.com/tailscale/wireguard-go/device.NewDevice")
}

func integrationEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("CLOWDER_INTEGRATION") == "" {
		t.Skip("set CLOWDER_INTEGRATION=1 to run real-DERP integration tests")
	}
}

// startRealDaemon runs a daemon with the production tailcat transport.
// startRealDaemon runs a daemon with the production tailcat transport;
// the returned stop function tears it down early.
func startRealDaemon(t *testing.T, dir, name string) (*Daemon, func()) {
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
	stop := func() {
		cancel()
		_ = tr.Close()
	}
	t.Cleanup(stop)
	go func() { _ = d.Run(ctx) }()
	waitFor(t, func() bool { return d.Me().Addr != "" }, "%s to listen", name)
	return d, stop
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
	a, _ := startRealDaemon(t, dirA, "hostA")
	b, _ := startRealDaemon(t, dirB, "hostB")

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

// TestIntegrationFailedDialDropsClient proves the leak fix: after a
// dial to a dead peer fails, the cached tailcat client (and its
// WireGuard engine) is closed and dropped instead of lingering and
// handshaking the unreachable peer forever.
func TestIntegrationFailedDialDropsClient(t *testing.T) {
	integrationEnabled(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	a, _ := startRealDaemon(t, dirA, "hostA")
	b, stopB := startRealDaemon(t, dirB, "hostB")

	code, err := a.StartInvite(context.Background())
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := b.Join(context.Background(), code); err != nil {
		t.Fatalf("join: %v", err)
	}

	src := filepath.Join(t.TempDir(), "nap.txt")
	if err := os.WriteFile(src, []byte("offline soon"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send("hostB", src); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, ok := inboxFile(t, b, "nap.txt")
		return ok
	}, "the first send to arrive")

	// Kill hostB, then keep sending for a while: every dial fails, and
	// each failure must close the client's WireGuard engine. A leak
	// would accumulate engines; closing keeps the count bounded.
	stopB()
	time.Sleep(20 * time.Second) // let the first failures settle
	baseline := engineGoroutines(t)
	t.Logf("engine goroutines at baseline: %d", baseline)
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := a.Send("hostB", src); err != nil {
			t.Fatalf("send: %v", err)
		}
		time.Sleep(3 * time.Second)
		if g := engineGoroutines(t); g > baseline+40 {
			t.Logf("engine goroutines mid-loop: %d", g)
			break
		}
	}
	// Attempts still in flight hold engines briefly; poll for the
	// count to drain. A leak never drains — the dump then names what
	// is stuck.
	drain := time.Now().Add(30 * time.Second)
	for time.Now().Before(drain) {
		if g := engineGoroutines(t); g <= baseline+10 {
			t.Logf("engine goroutines drained to %d (baseline %d): no leak", g, baseline)
			return
		}
		time.Sleep(time.Second)
	}
	var dump bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&dump, 1); err != nil {
		t.Fatalf("goroutine profile: %v", err)
	}
	t.Fatalf("engine goroutines never drained (baseline %d, now %d); wireguard goroutines stuck:\n%s",
		baseline, engineGoroutines(t), dump.String())
}

func TestIntegrationPairSend(t *testing.T) {
	integrationEnabled(t)
	dirA := t.TempDir()
	dirB := t.TempDir()
	a, _ := startRealDaemon(t, dirA, "hostA")
	b, _ := startRealDaemon(t, dirB, "hostB")

	code, err := a.StartInvite(context.Background())
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := b.Join(context.Background(), code); err != nil {
		t.Fatalf("join: %v", err)
	}
	// The inviter commits asynchronously after the joiner's ack (the
	// confirmed protocol makes it commit last), so poll rather than
	// assert immediately.
	waitFor(t, func() bool {
		_, ok := a.Roster().Get("hostB")
		return ok
	}, "hostA to add hostB once the pairing is confirmed")
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

	// A third cat joins and must discover the whole roster right away:
	// the join kicks an immediate sync, whose handshake exchanges full
	// rosters — not on the next poll tick.
	c, _ := startRealDaemon(t, t.TempDir(), "hostC")
	code2, err := a.StartInvite(context.Background())
	if err != nil {
		t.Fatalf("invite 2: %v", err)
	}
	// Invites are one-off: a second invitation is a second code, never
	// a reuse of the first.
	if code2 == code {
		t.Fatal("two invitations produced the same code")
	}
	if err := c.Join(context.Background(), code2); err != nil {
		t.Fatalf("join 2: %v", err)
	}
	waitFor(t, func() bool {
		_, ok := c.Roster().Get("hostB")
		return ok
	}, "the freshly joined hostC to discover hostB from the roster sync")

	// The consumed code cannot pair anyone else: the invite retired with
	// its first successful join.
	negCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Join(negCtx, code2); err == nil {
		t.Fatal("a used pairing code joined again: invites must be one-off")
	}
}
