package daemon

// Path probing: whether traffic to a cat flows direct (peer-to-peer
// UDP) or via a DERP relay. `clow status` reads the background cache
// (pathSnapshot); refreshPaths keeps it warm, and a disco ping also
// triggers direct-path discovery, so the refresh loop upgrades
// relayed paths to direct ones on its own.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"clowder/roster"
)

// Path probes the route to the named cat, waiting for the answer.
// Requires a transport that implements Pinger (the tailcat
// transport; the loopback test transport does not).
func (d *Daemon) Path(ctx context.Context, name string) (PathInfo, error) {
	cat, ok := d.ros.Get(name)
	if !ok {
		return PathInfo{}, fmt.Errorf("unknown cat %q", name)
	}
	p, ok := d.tr.(Pinger)
	if !ok {
		return PathInfo{}, errors.New("path probing not supported by this transport")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return p.Ping(ctx, cat.Addr)
}

const (
	// pathProbeTimeout bounds one background probe: a direct LAN path
	// answers in single-digit milliseconds, a DERP-relayed one in a
	// few hundred, so a full second is generous headroom.
	pathProbeTimeout = time.Second
)

// refreshPaths probes the path of every roster cat, in parallel, and
// swaps the results into the paths cache. Probing runs in the
// background (never on the status path), so an unreachable cat costs
// nothing user-visible: its ping just times out and it drops out of
// the cache for another round. Probing every cat — not just recently
// seen ones — means routes appear within one refresh of daemon start,
// before any handshake has had a chance to record liveness.
// Called once at startup and on every poll tick.
func (d *Daemon) refreshPaths(ctx context.Context) {
	p, ok := d.tr.(Pinger)
	if !ok {
		return
	}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	out := map[string]PathInfo{}
	for _, c := range d.ros.All() {
		wg.Add(1)
		go func(c roster.Cat) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, pathProbeTimeout)
			defer cancel()
			info, err := p.Ping(ctx, c.Addr)
			if err != nil {
				return
			}
			mu.Lock()
			out[c.Key] = info
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	d.mu.Lock()
	d.paths = out
	d.mu.Unlock()
}

// pathSnapshot copies the cached path info for the status op. It
// never probes and never blocks on the network: a status call answers
// instantly with whatever the last background refresh collected.
func (d *Daemon) pathSnapshot() map[string]*PathInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]*PathInfo, len(d.paths))
	for k, v := range d.paths {
		info := v
		out[k] = &info
	}
	return out
}
