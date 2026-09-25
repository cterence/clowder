package daemon

import (
	"context"
	"net"

	"tailscale.com/types/key"
)

// Transport abstracts how the daemon reaches other cats. The production
// implementation (TailcatTransport) runs a tailcat server and dials peers
// through tailcat clients over the DERP-relayed WireGuard data plane. The
// loopback implementation in transport_test.go runs the same protocol over
// plain TCP for tests.
type Transport interface {
	// Listen starts accepting connections from allowed peers. MyAddr
	// becomes valid after Listen returns.
	Listen(ctx context.Context) (net.Listener, error)
	// MyAddr returns the address other cats should use to reach this
	// cat (a tailcat address in production). Empty before Listen.
	MyAddr() string
	// Dial connects to the cat at addr.
	Dial(ctx context.Context, addr string) (net.Conn, error)
	// Allow permits a peer node key to connect. Idempotent; must work
	// both before and after Listen.
	Allow(peer key.NodePublic)
	// PeerKey returns the authenticated node key of a connected peer,
	// when the transport can authenticate (production transport can).
	PeerKey(remote net.Addr) (key.NodePublic, bool)
	// Close shuts the transport down.
	Close() error
}

// parseKey converts a node public key in string form (as stored in
// rosters) back to a key.
func parseKey(s string) (key.NodePublic, error) {
	var k key.NodePublic
	if err := k.UnmarshalText([]byte(s)); err != nil {
		return key.NodePublic{}, err
	}
	return k, nil
}
