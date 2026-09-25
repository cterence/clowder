package daemon

// Path probing: whether traffic to a cat flows direct (peer-to-peer
// UDP) or via a DERP relay. Probed on demand for `clow status`; a
// disco ping also triggers direct-path discovery, so probing can
// upgrade a relayed path to a direct one.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"clowder/roster"
)

// Path probes the route to the named cat. Requires a transport that
// implements Pinger (the tailcat transport; the loopback test
// transport does not).
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

// pathsFor probes every roster cat in parallel, bounded by a short
// timeout per cat; unreachable cats simply have no entry.
func (d *Daemon) pathsFor(ctx context.Context) map[string]*PathInfo {
	p, ok := d.tr.(Pinger)
	if !ok {
		return nil
	}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	out := map[string]*PathInfo{}
	for _, c := range d.ros.All() {
		wg.Add(1)
		go func(c roster.Cat) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			info, err := p.Ping(ctx, c.Addr)
			if err != nil {
				return
			}
			mu.Lock()
			out[c.Key] = &info
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return out
}
