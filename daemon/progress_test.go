package daemon

import (
	"context"
	"net"
	"testing"
	"time"
)

// slowConn throttles writes so transfers take long enough to observe
// mid-flight.
type slowConn struct {
	net.Conn
	perWrite time.Duration
}

func (c slowConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		time.Sleep(c.perWrite)
	}
	return n, err
}

// slowTransport is a LocalTransport whose dials return slowConns.
type slowTransport struct {
	LocalTransport
	perWrite time.Duration
}

func (t *slowTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := t.LocalTransport.Dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	return slowConn{Conn: conn, perWrite: t.perWrite}, nil
}

// TestTransferProgress observes both sides of a throttled transfer:
// the sender's outbox entry reports percent-complete while in flight,
// and the receiver reports a matching receiving progress; both
// disappear when the transfer ends.
func TestTransferProgress(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := Init(dirA, "milo"); err != nil {
		t.Fatal(err)
	}
	if err := Init(dirB, "fluff"); err != nil {
		t.Fatal(err)
	}
	// Throttle the sender's writes so the transfer spans many
	// observable chunk updates.
	sender := runDaemon(t, dirA, &slowTransport{perWrite: 5 * time.Millisecond})
	receiver := runDaemon(t, dirB, &LocalTransport{})
	trust(t, sender, receiver)
	trust(t, receiver, sender)

	// ~1 MiB so the throttled transfer spans many chunks.
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i)
	}
	src := writeSource(t, string(big))
	if _, err := sender.Send("fluff", src); err != nil {
		t.Fatal(err)
	}

	sawSending, sawReceiving := false, false
	waitFor(t, func() bool {
		for _, p := range sender.prog.snapshot() {
			if !p.Receiving && p.Done > 0 && p.Done < p.Total {
				sawSending = true
			}
		}
		for _, p := range receiver.prog.snapshot() {
			if p.Receiving && p.Done > 0 && p.Done < p.Total {
				sawReceiving = true
			}
		}
		return sawSending && sawReceiving
	}, "mid-flight progress on both sides")

	waitFor(t, func() bool {
		got, ok := inboxFile(t, receiver, "nap.txt")
		return ok && len(got) == len(big)
	}, "the throttled file to arrive")

	// Progress state is cleaned up once the transfer ends.
	waitFor(t, func() bool {
		return len(sender.prog.snapshot()) == 0 && len(receiver.prog.snapshot()) == 0
	}, "progress state to drain")
}

func TestProgressPercent(t *testing.T) {
	p := Progress{Done: 25, Total: 100}
	if p.Percent() != 0.25 {
		t.Fatalf("Percent() = %v, want 0.25", p.Percent())
	}
	if (Progress{Total: 0}).Percent() != 0 {
		t.Fatal("zero total must be zero percent")
	}
	if (Progress{Done: 200, Total: 100}).Percent() != 1 {
		t.Fatal("overflow must clamp to 1")
	}
}

func TestProgressRate(t *testing.T) {
	// A rolling window: the reported rate covers only recent samples,
	// not the whole transfer's average, and a transfer with no elapsed
	// window reports zero instead of a divide-by-zero spike.
	k := newProgressKeeper()
	base := time.Now()
	k.now = func() time.Time { return base }
	k.start(Progress{ID: "t1", FileName: "nap.txt", Total: 1 << 20})

	if got := k.snapshot()[0].Bps; got != 0 {
		t.Fatalf("fresh transfer reports %.0f B/s, want 0", got)
	}

	// 256 KiB per second for three seconds.
	for i := 1; i <= 3; i++ {
		base = base.Add(time.Second)
		k.add("t1", 256*1024)
	}
	// The window spans the whole transfer: the oldest sample (start,
	// 0 done) baselines the rate; now (+3s) has 768 KiB done.
	got := k.snapshot()[0].Bps
	want := 256 * 1024.0
	if got < want*0.9 || got > want*1.1 {
		t.Fatalf("rate = %.0f B/s, want ~%.0f B/s", got, want)
	}

	// Samples older than the window are pruned, so a stalled transfer
	// decays to zero rather than reporting its historic rate.
	base = base.Add(30 * time.Second)
	if got := k.snapshot()[0].Bps; got != 0 {
		t.Fatalf("stalled transfer reports %.0f B/s, want 0", got)
	}
}

// A snapshot that lands between chunk arrivals must still report the
// rate: the stall check belongs to the newest sample, not the oldest
// one in the window. Checking the base blinked the rate out of
// `clow send` on nearly every poll once the transfer passed one
// window in age.
func TestProgressRateBetweenChunks(t *testing.T) {
	k := newProgressKeeper()
	base := time.Now()
	k.now = func() time.Time { return base }
	k.start(Progress{ID: "t1", FileName: "nap.txt", Total: 1 << 20})
	// A mature transfer: 64 KiB chunks every 10ms.
	for i := 0; i < 350; i++ {
		base = base.Add(10 * time.Millisecond)
		k.add("t1", 64*1024)
	}
	base = base.Add(50 * time.Millisecond) // between chunk arrivals
	got := k.snapshot()[0].Bps
	want := 6.4 * 1024 * 1024 // 64 KiB per 10ms
	if got < want*0.9 || got > want*1.1 {
		t.Fatalf("rate = %.0f B/s, want ~%.0f B/s (a poll between chunks must not zero the rate)", got, want)
	}
	// Genuinely stalled: nothing arrived for a whole window.
	base = base.Add(11 * time.Second)
	if got := k.snapshot()[0].Bps; got != 0 {
		t.Fatalf("stalled transfer reports %.0f B/s, want 0", got)
	}
}
