package daemon

// Sender-side hold probes: an AckStored is not proof the storer kept the
// file, so the outbox entry survives the deposit. Each poll tick a probe
// asks every holding storer to prove it still has the transfer; a
// confirmed hold settles the entry for good, a denied one re-queues the
// send.

import (
	"context"
	"sync"
	"time"

	"github.com/cterence/clowder/protocol"
)

func (d *Daemon) probeHeld(ctx context.Context) {
	// One pass at a time: a stalled network must not stack probes.
	if !d.holdProbing.CompareAndSwap(false, true) {
		return
	}
	defer d.holdProbing.Store(false)
	var wg sync.WaitGroup
	for _, e := range d.ob.All() {
		if e.HeldBy == "" {
			continue
		}
		wg.Add(1)
		go func(e Entry) {
			defer wg.Done()
			d.probeHeldEntry(ctx, e)
		}(e)
	}
	wg.Wait()
}

func (d *Daemon) probeHeldEntry(ctx context.Context, e Entry) {
	cat, ok := d.ros.GetByKey(e.HeldBy)
	if !ok || d.isBlockedKey(e.HeldBy) {
		d.unhold(e, "holding storer is no longer trusted")
		return
	}
	pc, err := d.connect(ctx, cat, msgTimeout)
	if err != nil {
		return // unreachable: ask again next pass
	}
	defer func() { _ = pc.Close() }()
	if err := pc.WriteMsg(&protocol.Message{Ack: &protocol.Ack{ID: e.ID, Kind: protocol.AckHolding}}); err != nil {
		return
	}
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	m, err := pc.ReadMsg()
	if err != nil || m.Answer == nil {
		return
	}
	if m.Answer.OK {
		// A confirmed hold settles but does not delete (#8): only the
		// recipient's signed receipt clears the entry — a lying storer
		// answers "holding" without keeping the file.
		d.settle(e.ID, "stored via "+cat.Name)
		d.cfg.logf("clowder: %s confirmed holding %s", cat.Name, e.FileName)
		return
	}
	d.unhold(e, "storer no longer holds it")
}

func (d *Daemon) unhold(e Entry, why string) {
	e.HeldBy = ""
	if err := d.ob.Put(e); err != nil {
		d.cfg.logf("clowder: re-queueing %s: %v", e.ID, err)
	}
	d.cfg.logf("clowder: %s to %s re-queued: %s", e.FileName, e.TargetName, why)
}
