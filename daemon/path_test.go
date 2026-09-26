package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// pingTransport wraps the loopback transport with a controllable
// Pinger: it records every probed address, answers for known ones,
// and can hang a probe until its context dies — standing in for the
// tailcat transport's disco ping in the pathsFor tests.
type pingTransport struct {
	*LocalTransport
	mu      sync.Mutex
	pinged  map[string]int
	answers map[string]PathInfo
	hangs   map[string]bool
}

func newPingTransport() *pingTransport {
	return &pingTransport{
		LocalTransport: &LocalTransport{},
		pinged:         map[string]int{},
		answers:        map[string]PathInfo{},
		hangs:          map[string]bool{},
	}
}

func (p *pingTransport) Ping(ctx context.Context, addr string) (PathInfo, error) {
	p.mu.Lock()
	p.pinged[addr]++
	_, hang := p.hangs[addr]
	p.mu.Unlock()
	if hang {
		<-ctx.Done()
		return PathInfo{}, ctx.Err()
	}
	if info, ok := p.answers[addr]; ok {
		return info, nil
	}
	return PathInfo{}, errors.New("ping: no such peer")
}

func (p *pingTransport) pingedAddrs() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.pinged))
	for k, v := range p.pinged {
		out[k] = v
	}
	return out
}

// markSeenAt sets a cat's last-seen time, simulating a handshake at
// the given moment without one.
func markSeenAt(d *Daemon, key string, at time.Time) {
	d.mu.Lock()
	d.liveness[key] = at.Unix()
	d.mu.Unlock()
}

// TestPathsForSkipsCatsNotSeenRecently pins `clow status` latency: a
// never-seen cat (roster-synced, offline) or one idle past the fresh
// window must not be probed at all — its disco ping cannot succeed,
// so it can only burn the deadline, and unreachable cats show no
// route either way. A recently seen cat is probed and reported.
func TestPathsForSkipsCatsNotSeenRecently(t *testing.T) {
	pt := newPingTransport()
	dir := t.TempDir()
	if err := Init(dir, "prober"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	d := runDaemon(t, dir, pt)

	online := startDaemon(t, "online")
	addCat(t, d, online.Me())
	pt.answers[online.Me().Addr] = PathInfo{Direct: true, Endpoint: "127.0.0.1:9"}
	markSeenAt(d, online.Me().Key, time.Now())

	idle, _ := offlineCat(t)
	addCat(t, d, idle)
	markSeenAt(d, idle.Key, time.Now().Add(-15*time.Minute))

	ghost, _ := offlineCat(t)
	addCat(t, d, ghost)

	paths := d.pathsFor(context.Background())
	if got := paths[online.Me().Key]; got == nil || !got.Direct {
		t.Fatalf("paths for recently seen cat: %v, want a direct entry", got)
	}
	pinged := pt.pingedAddrs()
	if _, ok := pinged[idle.Addr]; ok {
		t.Error("cat idle past the fresh window was probed; its ping can only burn the deadline")
	}
	if _, ok := pinged[ghost.Addr]; ok {
		t.Error("never-seen cat was probed; its ping can only burn the deadline")
	}
}

// TestPathsForProbeTimeoutIsBounded pins the other half of status
// latency: a recently seen cat that has since gone dark (the probe
// hangs) must not hold pathsFor longer than the probe deadline.
func TestPathsForProbeTimeoutIsBounded(t *testing.T) {
	pt := newPingTransport()
	dir := t.TempDir()
	if err := Init(dir, "prober"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	d := runDaemon(t, dir, pt)

	dark := startDaemon(t, "dark")
	addCat(t, d, dark.Me())
	pt.hangs[dark.Me().Addr] = true
	markSeenAt(d, dark.Me().Key, time.Now())

	start := time.Now()
	paths := d.pathsFor(context.Background())
	elapsed := time.Since(start)
	if _, ok := paths[dark.Me().Key]; ok {
		t.Fatal("unreachable cat got a path entry, want none")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("pathsFor took %s on a hanging probe, want bounded by the ~1s probe deadline", elapsed)
	}
}
