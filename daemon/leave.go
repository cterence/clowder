package daemon

// Leaving the clowder. A leave is a signed tombstone broadcast to every
// reachable peer and carried by roster sync; recipients drop the leaver
// (roster, allowlist, spool, outbox) and re-broadcast once. Only a re-pair
// (newer signed entry) resurrects — see roster.ApplyTombstones.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"clowder/protocol"
	"clowder/roster"
)

// Bounds each leave announce (dial, handshake and write). A live cat
// answers the liveness ping in well under a second, so 3s only cuts
// congested or dead paths — and a cat that misses the direct announce
// still gets the tombstone via re-broadcast or roster sync.
var leaveTimeout = 3 * time.Second

// Leave announces the signed goodbye to every cat in parallel, waits,
// and cuts any cat that does not answer within leaveTimeout, then wipes
// the clowder locally (roster, outbox, spool, blocklist — tombstones
// included; the identity is kept). When there were cats to tell and
// none answered, it fails WITHOUT wiping, so the command can simply be
// retried.
func (d *Daemon) Leave() (int, error) {
	all := d.ros.All()
	t := roster.SignTombstone(d.env.SignPriv, d.Me().Key, time.Now().Unix())
	msg := &protocol.Message{Leave: &protocol.LeaveMsg{
		Key:     t.Key,
		SignKey: t.SignKey,
		Time:    t.Time,
		Sig:     t.Sig,
	}}

	announced := 0
	if len(all) > 0 {
		announced = d.announceLeave(all, msg)
		if announced == 0 {
			return 0, fmt.Errorf("no cat answered within %s, the leave was not announced, your roster is unchanged, try again", leaveTimeout)
		}
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

// announceLeave delivers the goodbye to every cat in parallel, each
// bounded by leaveTimeout, and reports how many were reached.
func (d *Daemon) announceLeave(all []roster.Cat, msg *protocol.Message) int {
	var announced atomic.Int32
	var wg sync.WaitGroup
	for _, c := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.announceLeaveTo(c, msg); err != nil {
				d.cfg.logf("clowder: announcing leave to %s failed: %v", c.Name, err)
				return
			}
			announced.Add(1)
		}()
	}
	wg.Wait()
	return int(announced.Load())
}

// announceLeaveTo handshakes with one cat and delivers the leave, the
// whole exchange bounded by leaveTimeout.
func (d *Daemon) announceLeaveTo(c roster.Cat, msg *protocol.Message) error {
	ctx, cancel := context.WithTimeout(context.Background(), leaveTimeout)
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
