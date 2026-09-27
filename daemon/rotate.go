package daemon

// Address rotation: a cat can replace its tailcat address (new
// pre-shared key, same identity key) and announce it to the clowder.
// The announcement rides the normal roster sync, where entries are
// last-write-wins: peers simply learn our entry with a newer address.
// Rotation must reach at least one cat, or it fails and nothing
// changes — otherwise the rotator would strand itself with an address
// nobody knows.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tailscale/tailcat"

	"clowder/protocol"
	"clowder/roster"
)

// RotateAddress replaces this cat's pre-shared key — and thus its
// tailcat address — and announces the new address to every reachable
// peer. It fails without changing anything if no peer acknowledged.
// The running daemon keeps serving under the old address; restart it
// to use the new one.
func (d *Daemon) RotateAddress(ctx context.Context) (string, error) {
	me := d.Me()
	all := d.ros.All()

	newPSK := tailcat.NewPresharedKey()
	newAddr := d.prospectiveAddr(newPSK)
	newMe := me
	newMe.Addr = string(newAddr)
	newMe.Updated = time.Now().Unix()
	newMe = roster.SignCat(d.env.SignPriv, newMe)

	announced := 0
	for _, c := range all {
		if err := d.announceTo(ctx, c, newMe); err != nil {
			d.cfg.logf("clowder: announcing rotation to %s failed: %v", c.Name, err)
		} else {
			announced++
		}
	}
	if len(all) > 0 && announced == 0 {
		return "", errors.New("could not announce the new address to any cat; not rotating")
	}

	if err := UpdatePresharedKey(d.cfg.Dir, newPSK); err != nil {
		return "", fmt.Errorf("announced to %d cat(s) but failed to persist the new address: %w", announced, err)
	}
	d.cfg.logf("clowder: address rotated (announced to %d cat(s)); restart the daemon to use it", announced)
	return string(newAddr), nil
}

// prospectiveAddr builds the address the cat would have under psk: the
// same identity and DERP region, a fresh pre-shared key.
func (d *Daemon) prospectiveAddr(psk tailcat.PresharedKey) tailcat.Addr {
	ci := tailcat.ConnInfo{
		ServerPublic:      d.env.Identity.Public.ServerPublic,
		ServerDiscoPublic: discoPublicForNode(d.env.Identity.Private),
		PresharedKey:      psk,
	}
	// Keep the region we are on now, if the transport reports a tailcat
	// address; else fall back to the identity's region hint.
	if cur, err := tailcat.ParseAddr(tailcat.Addr(d.tr.MyAddr())); err == nil {
		ci.Region = cur.Region
		ci.RegionID = cur.RegionID
	} else {
		ci.RegionID = d.env.Identity.Public.RegionID
	}
	return ci.Addr()
}

// announceTo pushes a roster sync presenting newMe (our rotated entry)
// to one peer over an established connection.
func (d *Daemon) announceTo(ctx context.Context, cat roster.Cat, newMe roster.Cat) error {
	pc, err := d.connect(ctx, cat)
	if err != nil {
		return err
	}
	defer func() { _ = pc.Close() }()
	return pc.WriteMsg(&protocol.Message{Roster: &protocol.RosterSync{
		Cats: append(d.ros.All(), newMe),
	}})
}
