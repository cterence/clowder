package daemon

// Leaving the clowder. A cat's leave is a signed forget-me: the
// leaver broadcasts a leave tombstone to every reachable peer, each
// recipient drops the leaver (roster entry, allowlist, held files,
// pending sends) and re-broadcasts once, and the tombstone rides
// every roster sync so offline peers catch up on wake. A tombstone
// outlives unsigned re-adds; only a newer entry signed by the
// leaver's own sign key (a re-pair) resurrects it — see
// roster.ApplyTombstones.

import (
	"context"
	"time"

	"clowder/protocol"
	"clowder/roster"
)

// Leave broadcasts a signed forget-me to every reachable peer, then
// discards the clowder locally: roster, outbox, spool and blocklist
// are wiped (tombstones included, so a later pairing starts clean).
// The identity is kept; pair again with invite/join to join a
// clowder. It returns the number of peers the leave reached — a cat
// nobody could reach has still left locally.
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

// hasLeft reports whether the cat has left the clowder.
func (d *Daemon) hasLeft() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.left
}

// handleLeave processes a leave received from a peer on an open
// connection: claim it, apply it, re-broadcast once.
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

// handleTombstone applies one leave tombstone and re-broadcasts it to
// every other reachable peer. claim guards the re-broadcast: each
// leave is processed and forwarded once per cat (claim false for
// tombstones discovered in a roster sync, where a duplicate would
// loop between sync partners — ApplyTombstones is idempotent, the
// re-broadcast is not).
func (d *Daemon) handleTombstone(t roster.Tombstone, claim bool) {
	if claim && !d.claimLeave(t.Key, t.Time) {
		return
	}
	if !d.applyTombstone(t) {
		return
	}
	d.rebroadcastLeave(t)
}

// claimLeave records that a leave (leaver, time) is being processed,
// reporting false for one this cat has already seen. A newer time
// for the same leaver (a leave, then a re-pair, then another leave)
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

// applyTombstone verifies and applies one leave tombstone locally:
// the leaver leaves the roster, the allowlist (blocked.json, since
// tailcat's AllowedClients is add-only), the spool and the outbox.
// It reports whether the tombstone changed local state.
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

// rebroadcastLeave forwards an applied leave to every reachable
// peer except the leaver. First-arrival claim on the receiving side
// bounds the flood: each cat re-broadcasts once.
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
