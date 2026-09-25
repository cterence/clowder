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
