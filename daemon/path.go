package daemon

// Path probing: direct (peer-to-peer UDP) or via a DERP relay. refreshPaths
// keeps the background cache warm (`clow status` never probes); a disco
// ping also triggers direct-path discovery, upgrading relayed paths.

import (
	"context"
	"sync"
	"time"

	"clowder/roster"
)

const (
	// Bounds one background probe (a DERP-relayed answer is a few hundred ms).
	pathProbeTimeout = time.Second
)

// refreshPaths probes every roster cat in parallel and swaps the results
// into the cache. Runs only in the background, at startup and per poll tick.
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

// pathSnapshot copies the cache; it never probes or blocks.
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
