package daemon

import (
	"net"

	"clowder/protocol"
)

// serveAccepted authenticates when the transport can, then serves. One
// authenticated peer may hold at most maxPeerConns concurrent serves;
// the excess is dropped at the door.
func (d *Daemon) serveAccepted(conn net.Conn) {
	pc := protocol.NewConn(conn)
	defer func() { _ = pc.Close() }()
	unregister := d.trackConn(pc)
	defer unregister()
	authKey, authed := d.tr.PeerKey(conn.RemoteAddr())
	if authed {
		id := authKey.String()
		if !d.takePeerConn(id) {
			d.cfg.logf("clowder: dropping conn from %s: more than %d concurrent connections", id, maxPeerConns)
			return
		}
		defer d.releasePeerConn(id)
	}
	d.serveConn(pc, authKey, authed)
}

// takePeerConn counts a concurrent serve for a peer key, reporting
// whether it fits under the cap.
func (d *Daemon) takePeerConn(id string) bool {
	d.peerConnsMu.Lock()
	defer d.peerConnsMu.Unlock()
	d.peerConns[id]++
	return d.peerConns[id] <= maxPeerConns
}

func (d *Daemon) releasePeerConn(id string) {
	d.peerConnsMu.Lock()
	if d.peerConns[id] <= 1 {
		delete(d.peerConns, id)
	} else {
		d.peerConns[id]--
	}
	d.peerConnsMu.Unlock()
}
