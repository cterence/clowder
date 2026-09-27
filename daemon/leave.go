package daemon

// Leaving the clowder. A leave is a signed tombstone broadcast to every
// reachable peer and carried by roster sync; recipients drop the leaver
// (roster, allowlist, spool, outbox) and re-broadcast once. Only a re-pair
// (newer signed entry) resurrects — see roster.ApplyTombstones.

import (
	"context"
	"time"

	"clowder/protocol"
	"clowder/roster"
)

// Leave broadcasts the leave, then wipes the clowder locally (roster,
// outbox, spool, blocklist — tombstones included); the identity is kept.
// Returns how many peers were reached; a cat nobody could reach has still
// left locally.
func (d *Daemon) Leave(ctx context.Context) (int, error) {
	me := d.Me()
	all := d.ros.All()
	t := roster.SignTombstone(d.env.SignPriv, me.Key, time.Now().Unix())
	msg := &protocol.Message{Leave: &protocol.LeaveMsg{
		Key:     t.Key,
		SignKey: t.SignKey,
		Time:    t.Time,
		Sig:     t.Sig,
	}}
	announced := 0
	for _, c := range all {
		pc, err := d.connect(ctx, c)
		if err != nil {
			d.cfg.logf("clowder: announcing leave to %s failed: %v", c.Name, err)
			continue
		}
		if err := pc.WriteMsg(msg); err != nil {
			d.cfg.logf("clowder: announcing leave to %s failed: %v", c.Name, err)
		} else {
			announced++
		}
		_ = pc.Close()
	}

	d.mu.Lock()
	d.left = true
	d.mu.Unlock()
	if err := d.ros.Reset(); err != nil {
		return announced, err
	}
	if _, err := d.ob.Clear(); err != nil {
		d.cfg.logf("clowder: clearing outbox on leave: %v", err)
	}
	for _, m := range d.spool.All() {
		if err := d.spool.Delete(m.ID); err != nil {
			d.cfg.logf("clowder: dropping held %s on leave: %v", m.FileName, err)
		}
	}
	d.mu.Lock()
	d.blocked = map[string]bool{}
	err := saveBlocked(d.cfg.Dir, d.blocked)
	d.mu.Unlock()
	if err != nil {
		d.cfg.logf("clowder: clearing blocklist on leave: %v", err)
	}
	d.cfg.logf("clowder: left the clowder (told %d cat(s)); identity kept", announced)
	return announced, nil
}

func (d *Daemon) hasLeft() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.left
}

func (d *Daemon) handleLeave(l *protocol.LeaveMsg) {
	if l.Key == d.Me().Key {
		return // our own leave echoing back
	}
	d.handleTombstone(roster.Tombstone{
		Key:     l.Key,
		SignKey: l.SignKey,
		Time:    l.Time,
		Sig:     l.Sig,
	}, true)
}

// handleTombstone applies one tombstone and re-broadcasts it; claim guards
// the re-broadcast (each leave is forwarded once per cat — ApplyTombstones
// is idempotent, the re-broadcast is not).
func (d *Daemon) handleTombstone(t roster.Tombstone, claim bool) {
	if claim && !d.claimLeave(t.Key, t.Time) {
		return
	}
	if !d.applyTombstone(t) {
		return
	}
	d.rebroadcastLeave(t)
}

// claimLeave reports false for a (leaver, time) already seen; a newer time
// is processed again.
func (d *Daemon) claimLeave(key string, at int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.leaveSeen[key] >= at {
		return false
	}
	d.leaveSeen[key] = at
	return true
}

// applyTombstone drops the leaver from roster, blocklist, spool and outbox;
// reports whether local state changed.
func (d *Daemon) applyTombstone(t roster.Tombstone) bool {
	entry, _ := d.ros.GetByKey(t.Key)
	applied := d.ros.ApplyTombstones([]roster.Tombstone{t})
	if len(applied) == 0 {
		return false
	}
	if entry.Key != "" {
		if err := d.blockCat(entry); err != nil {
			d.cfg.logf("clowder: persisting blocklist after %s left: %v", entry.Name, err)
		}
		for _, m := range d.spool.All() {
			if m.TargetKey == t.Key {
				if err := d.spool.Delete(m.ID); err != nil {
					d.cfg.logf("clowder: dropping held %s for %s: %v", m.FileName, entry.Name, err)
				}
			}
		}
		for _, e := range d.ob.All() {
			if e.TargetKey == t.Key {
				if err := d.ob.Delete(e.ID); err != nil {
					d.cfg.logf("clowder: dropping outbox entry %s: %v", e.ID, err)
				}
			}
		}
		d.cfg.logf("clowder: %s left the clowder", entry.Name)
	}
	return true
}

// rebroadcastLeave forwards an applied leave once per cat (claim-guarded).
func (d *Daemon) rebroadcastLeave(t roster.Tombstone) {
	msg := &protocol.Message{Leave: &protocol.LeaveMsg{
		Key:     t.Key,
		SignKey: t.SignKey,
		Time:    t.Time,
		Sig:     t.Sig,
	}}
	for _, c := range d.ros.All() {
		if c.Key == t.Key {
			continue
		}
		d.goBg(func() {
			pc, err := d.connect(context.Background(), c)
			if err != nil {
				return
			}
			defer func() { _ = pc.Close() }()
			_ = pc.WriteMsg(msg)
		})
	}
}
