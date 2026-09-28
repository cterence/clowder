package daemon

// Leaving the clowder. A leave is a signed tombstone broadcast to every
// reachable peer and carried by roster sync; recipients drop the leaver
// (roster, allowlist, spool, outbox) and re-broadcast once. Only a re-pair
// (newer signed entry) resurrects — see roster.ApplyTombstones.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"clowder/protocol"
	"clowder/roster"
)

// Bounds each leave announce (dial, handshake and write). A live cat
// answers within the transport's 10s ping bound plus a few round trips,
// so 15s only cuts paths that are effectively dead — and a cat that
// misses the direct announce still gets the tombstone via re-broadcast
// or roster sync.
var leaveTimeout = 15 * time.Second

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
	var announced atomic.Int32
	var wg sync.WaitGroup
	for _, c := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.announceLeave(ctx, c, msg); err != nil {
				d.cfg.logf("clowder: announcing leave to %s failed: %v", c.Name, err)
				return
			}
			announced.Add(1)
		}()
	}
	wg.Wait()

	d.mu.Lock()
	d.left = true
	d.mu.Unlock()
	if err := d.ros.Reset(); err != nil {
		return int(announced.Load()), err
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
	d.cfg.logf("clowder: left the clowder (told %d cat(s)); identity kept", announced.Load())
	return int(announced.Load()), nil
}

// announceLeave handshakes with one cat and delivers the leave, the
// whole exchange bounded by leaveTimeout.
func (d *Daemon) announceLeave(ctx context.Context, c roster.Cat, msg *protocol.Message) error {
	ctx, cancel := context.WithTimeout(ctx, leaveTimeout)
	defer cancel()
	pc, err := d.connect(ctx, c, leaveTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = pc.Close() }()
	_ = pc.SetDeadline(time.Now().Add(leaveTimeout))
	return pc.WriteMsg(msg)
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
			pc, err := d.connect(context.Background(), c, msgTimeout)
			if err != nil {
				return
			}
			defer func() { _ = pc.Close() }()
			_ = pc.WriteMsg(msg)
		})
	}
}
