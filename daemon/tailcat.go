package daemon

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

// TailcatTransport is the production Transport: a tailcat Server under the
// identity key, dials via tailcat Clients under a separate client key.
// The keys must differ: two engines sharing one static key (with different
// per-side PSKs) cross-deliver handshakes and wedge.
type TailcatTransport struct {
	Port uint16
	Key  *tailcat.PrivateKey
	// Outbound-dial identity; peers allowlist its public form.
	ClientKey key.NodePrivate
	// Where the server and dialed clients fetch the DERP map; empty = default.
	DERPMapURL string
	Logf       func(format string, args ...any)

	mu       sync.Mutex
	pending  []key.NodePublic           // allows before Listen
	srv      *tailcat.Server            // non-nil after Listen
	clients  map[string]*tailcat.Client // one client per peer address
	lastUse  map[string]time.Time       // per address, for idle eviction
	inflight map[string]int             // open dials per address: never evict these
	evict    chan struct{}              // closes when the sweeper stops
}

// Idle-engine eviction: a cached client holds a live WireGuard engine
// per peer, and they accumulate without a usage cap.
const (
	idleEvictInterval = time.Minute
	idleEvictMax      = 10 * time.Minute
)

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

// Dial connects to the clowder port at addr. Clients are cached per
// address (each holds a WireGuard engine, and we dial the same peers
// repeatedly); clientFor creates one on first use.
func (t *TailcatTransport) clientFor(addr string) *tailcat.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.clients == nil {
		t.clients = map[string]*tailcat.Client{}
	}
	if t.lastUse == nil {
		t.lastUse = map[string]time.Time{}
	}
	if t.inflight == nil {
		t.inflight = map[string]int{}
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

// evictIdle closes clients idle beyond maxIdle — never ones with an
// open dial (a streaming transfer outlives the idle window); a later
// dial rebuilds from the current roster entry (the peer may have
// rotated its address).
func (t *TailcatTransport) evictIdle(maxIdle time.Duration) {
	t.mu.Lock()
	now := time.Now()
	for addr, last := range t.lastUse {
		if now.Sub(last) <= maxIdle || t.inflight[addr] > 0 {
			continue
		}
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
	// Liveness first, with a real round trip: tailcat's meow Ping is one-shot
	// per client, so it lets a just-went-offline peer pass and the dial wedges
	// in SYN retries. A disco ping is a cheap round trip for a live peer, a
	// bounded failure for a dead one, and triggers direct-path discovery.
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := c.DiscoPing(pingCtx); err != nil {
		t.dropClient(addr, c)
		return nil, err
	}
	// Bound the tunnel dial too: a dead session must fail in seconds.
	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDial()
	conn, err := c.DialTCPPort(dialCtx, t.Port)
	if err != nil {
		t.dropClient(addr, c)
		return nil, err
	}
	t.mu.Lock()
	if t.inflight == nil {
		t.inflight = map[string]int{}
	}
	t.inflight[addr]++
	t.mu.Unlock()
	return &dialConn{Conn: conn, t: t, addr: addr}, nil
}

// dialConn decrements its address's in-flight count on Close, so the
// idle sweeper never closes a client an open stream is using.
type dialConn struct {
	net.Conn
	t    *TailcatTransport
	addr string
}

func (c *dialConn) Close() error {
	err := c.Conn.Close()
	c.t.mu.Lock()
	if c.t.inflight[c.addr] <= 1 {
		delete(c.t.inflight, c.addr)
	} else {
		c.t.inflight[c.addr]--
	}
	c.t.mu.Unlock()
	return err
}

// dropClient closes and forgets a cached client: one abandoned mid-handshake
// retries the unreachable peer forever. The next use rebuilds it.
func (t *TailcatTransport) dropClient(addr string, c *tailcat.Client) {
	_ = c.Close()
	t.mu.Lock()
	delete(t.clients, addr)
	t.mu.Unlock()
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
