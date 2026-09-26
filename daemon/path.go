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

const (
	// pathProbeTimeout bounds one status probe: a direct LAN path
	// answers in single-digit milliseconds, a DERP-relayed one in a
	// few hundred, so a full second is generous headroom.
	pathProbeTimeout = time.Second

	// pathProbeFresh is how recently a cat must have been seen for
	// `clow status` to probe its path. Cats idle longer than this (and
	// never-seen ones) are skipped: nothing answers their disco ping,
	// so probing them can only burn the deadline. Generous enough to
	// cover the sync round-robin (one peer per poll tick, so an
	// online cat's last handshake is at most a few minutes old at
	// clowder scale).
	pathProbeFresh = 10 * time.Minute
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

// pathsFor probes the path of every roster cat seen within the fresh
// window, in parallel, bounded by a short timeout per cat. Cats not
// probed (and unreachable ones) simply have no entry — the status
// route column is empty for them either way, and this keeps `clow
// status` from waiting out probe deadlines on offline cats.
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
		if seen := d.SeenAt(c.Key); seen == 0 || time.Since(time.Unix(seen, 0)) > pathProbeFresh {
			continue
		}
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
			out[c.Key] = &info
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return out
}
