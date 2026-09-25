package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

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
	Logf      func(format string, args ...any)

	mu      sync.Mutex
	pending []key.NodePublic           // allows before Listen
	srv     *tailcat.Server            // non-nil after Listen
	clients map[string]*tailcat.Client // one client per peer address
}

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
		Logf:         t.Logf,
	}
	s.AllowedClients = append([]key.NodePublic(nil), t.pending...)
	t.srv = s
	t.mu.Unlock()

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
			Server: tailcat.Addr(addr),
			Key:    t.ClientKey,
			Logf:   t.Logf,
		}
		t.clients[addr] = c
	}
	return c
}

func (t *TailcatTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	c := t.clientFor(addr)
	conn, err := c.DialTCPPort(ctx, t.Port)
	if err != nil {
		// Drop the cached client so the next attempt is rebuilt from
		// the current roster entry (the peer may have re-paired with a
		// new address).
		t.mu.Lock()
		delete(t.clients, addr)
		t.mu.Unlock()
		return nil, err
	}
	return conn, nil
}

// Ping probes the path to the cat at the given tailcat address with a
// disco ping, which also triggers direct-path discovery: probing can
// upgrade a relayed connection to a direct one.
func (t *TailcatTransport) Ping(ctx context.Context, addr string) (PathInfo, error) {
	c := t.clientFor(addr)
	res, err := c.DiscoPing(ctx)
	if err != nil {
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

// Close shuts down the server and any dialed clients.
func (t *TailcatTransport) Close() error {
	t.mu.Lock()
	srv := t.srv
	clients := t.clients
	t.srv = nil
	t.clients = nil
	t.mu.Unlock()

	for _, c := range clients {
		_ = c.Close()
	}
	if srv != nil {
		return srv.Close()
	}
	return nil
}
