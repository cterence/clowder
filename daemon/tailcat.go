package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

// TailcatTransport is the production Transport: it runs a tailcat Server
// under the cat's identity key and dials peers with tailcat Clients
// under a separate client key. The keys must differ: server and client
// engines sharing one static key cross-deliver each other's handshakes
// (their per-side pre-shared keys differ) and the connection wedges.
type TailcatTransport struct {
	Port uint16
	Key  *tailcat.PrivateKey
	// ClientKey is the identity all outbound dials use. Peers must
	// allowlist its public form.
	ClientKey key.NodePrivate
	// DERPMapURL, if set, is where the server and dialed clients
	// fetch the DERP map from instead of tailcat's default.
	DERPMapURL string
	Logf       func(format string, args ...any)

	mu      sync.Mutex
	pending []key.NodePublic           // allows before Listen
	srv     *tailcat.Server            // non-nil after Listen
	clients map[string]*tailcat.Client // one client per peer address
	lastUse map[string]time.Time       // per address, for idle eviction
	evict   chan struct{}              // closes when the sweeper stops
}

// Idle-engine eviction: a cached client holds a live WireGuard engine
// per peer, and they accumulate without a usage cap.
const (
	idleEvictInterval = time.Minute
	idleEvictMax      = 10 * time.Minute
)

// NewTailcatTransport returns a transport serving the cat's identity on
// the given clowder protocol port, dialing out with clientKey.
func NewTailcatTransport(k *tailcat.PrivateKey, clientKey key.NodePrivate, port uint16, logf func(format string, args ...any)) *TailcatTransport {
	return &TailcatTransport{Port: port, Key: k, ClientKey: clientKey, Logf: logf}
}

// Listen starts the tailcat server. Callers learn their address via
// MyAddr once this returns.
func (t *TailcatTransport) Listen(ctx context.Context) (net.Listener, error) {
	t.mu.Lock()
	s := &tailcat.Server{
		Key:          t.Key.Private,
		PresharedKey: t.Key.Public.PresharedKey,
		DERPMapURL:   t.DERPMapURL,
		Logf:         t.Logf,
	}
	s.AllowedClients = append([]key.NodePublic(nil), t.pending...)
	t.srv = s
	t.clients = map[string]*tailcat.Client{}
	t.lastUse = map[string]time.Time{}
	t.evict = make(chan struct{})
	t.mu.Unlock()
	go t.sweepIdle(idleEvictInterval, idleEvictMax)

	ln, err := s.Listen(ctx, "tcp", fmt.Sprintf(":%d", t.Port))
	if err != nil {
		t.mu.Lock()
		t.srv = nil
		t.mu.Unlock()
		return nil, fmt.Errorf("tailcat listen: %w", err)
	}
	return ln, nil
}

// MyAddr returns the cat's tailcat address, or "" before Listen.
func (t *TailcatTransport) MyAddr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.srv == nil {
		return ""
	}
	return string(t.srv.TailcatAddr())
}

// Allow permits a peer node key to connect, before or after Listen.
func (t *TailcatTransport) Allow(peer key.NodePublic) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.srv == nil {
		t.pending = append(t.pending, peer)
		return
	}
	t.srv.AddAllowedClient(peer)
}

// Dial connects to the clowder port of the cat at the given tailcat
// address. Clients are cached per address: each holds a WireGuard
// engine, and a cat dials the same peers repeatedly.
// clientFor returns the cached client for an address, creating it on
// first use. Each client holds a WireGuard engine, and a cat dials the
// same peers repeatedly.
func (t *TailcatTransport) clientFor(addr string) *tailcat.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.clients == nil {
		t.clients = map[string]*tailcat.Client{}
	}
	c, ok := t.clients[addr]
	if !ok {
		c = &tailcat.Client{
			Server:     tailcat.Addr(addr),
			Key:        t.ClientKey,
			DERPMapURL: t.DERPMapURL,
			Logf:       t.Logf,
		}
		t.clients[addr] = c
	}
	t.lastUse[addr] = time.Now()
	return c
}

// idleKeys returns the addresses unused for longer than maxIdle: the
// eviction candidates. Each candidate holds a live WireGuard engine,
// and engines accumulate per peer without this.
func idleKeys(lastUse map[string]time.Time, now time.Time, maxIdle time.Duration) []string {
	var out []string
	for addr, last := range lastUse {
		if now.Sub(last) > maxIdle {
			out = append(out, addr)
		}
	}
	return out
}

// evictIdle closes clients idle beyond maxIdle. Called from the
// sweeper goroutine started at Listen; a later dial rebuilds the
// client from the current roster entry (the peer may have rotated its
// address).
func (t *TailcatTransport) evictIdle(maxIdle time.Duration) {
	t.mu.Lock()
	idle := idleKeys(t.lastUse, time.Now(), maxIdle)
	for _, addr := range idle {
		if c, ok := t.clients[addr]; ok {
			_ = c.Close()
		}
		delete(t.clients, addr)
		delete(t.lastUse, addr)
	}
	t.mu.Unlock()
}

// sweepIdle runs the eviction loop until the transport closes.
func (t *TailcatTransport) sweepIdle(interval, maxIdle time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			t.evictIdle(maxIdle)
		case <-t.evict:
			return
		}
	}
}

func (t *TailcatTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	c := t.clientFor(addr)
	// Liveness first, with a real round trip: tailcat's meow Ping is
	// one-shot per client (a cached client that has ever talked to the
	// peer reports success instantly, dead or not), so probing with it
	// lets a just-went-offline peer pass and the dial below wedges in
	// netstack SYN retries for the caller's whole context. A disco ping
	// has no such one-shot state: one ping is a cheap round trip for a
	// live peer and a bounded failure for a dead one, and it also
	// triggers direct-path discovery.
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := c.DiscoPing(pingCtx); err != nil {
		t.dropClient(addr, c)
		return nil, err
	}
	// Bound the tunnel dial as well: a peer that pongs but has a dead
	// tunnel session must fail in seconds, not starve the storer
	// fallback until the (multi-minute) delivery context ends.
	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDial()
	conn, err := c.DialTCPPort(dialCtx, t.Port)
	if err != nil {
		t.dropClient(addr, c)
		return nil, err
	}
	return conn, nil
}

// dropClient closes and forgets a cached client: a client holds a live
// WireGuard engine, and one abandoned mid-handshake keeps retrying the
// unreachable peer forever (log spam and a goroutine leak). The next
// use rebuilds it from the current roster entry (the peer may have
// re-paired with a new address).
func (t *TailcatTransport) dropClient(addr string, c *tailcat.Client) {
	_ = c.Close()
	t.mu.Lock()
	delete(t.clients, addr)
	t.mu.Unlock()
}

// Ping probes the path to the cat at the given tailcat address with a
// disco ping, which also triggers direct-path discovery: probing can
// upgrade a relayed connection to a direct one.
func (t *TailcatTransport) Ping(ctx context.Context, addr string) (PathInfo, error) {
	c := t.clientFor(addr)
	res, err := c.DiscoPing(ctx)
	if err != nil {
		// A failed probe leaves the engine mid-handshake with an
		// unreachable peer: close it rather than leak the retries.
		t.dropClient(addr, c)
		return PathInfo{}, err
	}
	if res.Err != "" {
		return PathInfo{}, errors.New(res.Err)
	}
	if res.Endpoint != "" {
		return PathInfo{Direct: true, Endpoint: res.Endpoint}, nil
	}
	return PathInfo{DERPRegionID: int(res.DERPRegionID), DERPRegionCode: res.DERPRegionCode}, nil
}

// PeerKey returns the authenticated node key of a connected peer.
func (t *TailcatTransport) PeerKey(remote net.Addr) (key.NodePublic, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.srv == nil {
		return key.NodePublic{}, false
	}
	return t.srv.PeerKey(remote)
}

// Close shuts down the server, the eviction sweeper, and any dialed
// clients.
func (t *TailcatTransport) Close() error {
	t.mu.Lock()
	srv := t.srv
	clients := t.clients
	t.srv = nil
	t.clients = nil
	if t.evict != nil {
		close(t.evict)
		t.evict = nil // Close may run again (Run's shutdown plus a stop func)
	}
	t.mu.Unlock()

	for _, c := range clients {
		_ = c.Close()
	}
	if srv != nil {
		return srv.Close()
	}
	return nil
}
