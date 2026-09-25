package daemon

import (
	"net"

	"clowder/protocol"
)

// serveAccepted authenticates the peer when the transport supports it
// (production tailcat does; the test transport does not) and runs the
// protocol on the connection.
func (d *Daemon) serveAccepted(conn net.Conn) {
	pc := protocol.NewConn(conn)
	defer func() { _ = pc.Close() }()
	authKey, authed := d.tr.PeerKey(conn.RemoteAddr())
	d.serveConn(pc, authKey, authed)
}
