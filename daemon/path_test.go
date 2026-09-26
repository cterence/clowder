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
// tailcat transport's disco ping in the refreshPaths tests.
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

// TestRefreshPathsProbesEveryCat pins the cold-start behavior: the
// background refresh probes every roster cat — including never-seen
// ones — so route lines appear within one refresh of daemon start,
// before any handshake has recorded liveness. Reachable cats land in
// the snapshot; unreachable ones simply drop out.
func TestRefreshPathsProbesEveryCat(t *testing.T) {
	pt := newPingTransport()
	dir := t.TempDir()
	if err := Init(dir, "prober"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	d := runDaemon(t, dir, pt)

	online := startDaemon(t, "online")
	addCat(t, d, online.Me())
	pt.answers[online.Me().Addr] = PathInfo{Direct: true, Endpoint: "127.0.0.1:9"}

	ghost, _ := offlineCat(t) // roster entry, no daemon, never seen
	addCat(t, d, ghost)

	d.refreshPaths(context.Background())

	paths := d.pathSnapshot()
	if got := paths[online.Me().Key]; got == nil || !got.Direct {
		t.Fatalf("paths for reachable cat: %v, want a direct entry", got)
	}
	pinged := pt.pingedAddrs()
	if _, ok := pinged[ghost.Addr]; !ok {
		t.Error("never-seen cat was not probed; routes should not wait on liveness")
	}
	if _, ok := pinged[ghost.Key]; ok {
		t.Error("probe keyed by cat key, want roster address")
	}
	if _, ok := paths[ghost.Key]; ok {
		t.Error("unreachable cat got a path entry, want none")
	}
}

// TestPathSnapshotNeverProbes pins the status-path contract: the
// snapshot answers from the cache alone, so `clow status` is instant
// no matter how slow or dead the peers are.
func TestPathSnapshotNeverProbes(t *testing.T) {
	pt := newPingTransport()
	dir := t.TempDir()
	if err := Init(dir, "prober"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	d := runDaemon(t, dir, pt)

	online := startDaemon(t, "online")
	addCat(t, d, online.Me())
	pt.answers[online.Me().Addr] = PathInfo{Direct: true, Endpoint: "127.0.0.1:9"}

	d.refreshPaths(context.Background())
	before := pt.pingedAddrs()

	// Every probe would now hang: the snapshot must still answer
	// instantly from the cache, without pinging anything.
	pt.hangs[online.Me().Addr] = true
	paths := d.pathSnapshot()
	if got := paths[online.Me().Key]; got == nil || !got.Direct {
		t.Fatalf("cached path missing: %v, want the direct entry", got)
	}
	after := pt.pingedAddrs()
	if n, m := after[online.Me().Addr], before[online.Me().Addr]; n != m {
		t.Fatalf("pathSnapshot probed %d times, want 0 (cached answers only)", n-m)
	}
}

// TestRefreshPathsProbeTimeoutIsBounded pins the background loop's
// hygiene: a cat that does not answer must not pin a refresh worker
// past the probe deadline.
func TestRefreshPathsProbeTimeoutIsBounded(t *testing.T) {
	pt := newPingTransport()
	dir := t.TempDir()
	if err := Init(dir, "prober"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	d := runDaemon(t, dir, pt)

	dark := startDaemon(t, "dark")
	addCat(t, d, dark.Me())
	pt.hangs[dark.Me().Addr] = true

	start := time.Now()
	d.refreshPaths(context.Background())
	elapsed := time.Since(start)
	if _, ok := d.pathSnapshot()[dark.Me().Key]; ok {
		t.Fatal("unreachable cat got a path entry, want none")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("refreshPaths took %s on a hanging probe, want bounded by the ~1s probe deadline", elapsed)
	}
}
