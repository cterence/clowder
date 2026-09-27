package daemon

import (
	"net"

	"clowder/protocol"
)

// serveAccepted authenticates when the transport can, then serves.
func (d *Daemon) serveAccepted(conn net.Conn) {
	pc := protocol.NewConn(conn)
	defer func() { _ = pc.Close() }()
	unregister := d.trackConn(pc)
	defer unregister()
	authKey, authed := d.tr.PeerKey(conn.RemoteAddr())
	d.serveConn(pc, authKey, authed)
}
