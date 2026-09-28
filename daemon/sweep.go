package daemon

// Storer push sweep: deliver every held file whose target is reachable, per
// poll tick and right after a new deposit. The stream stays opaque to the
// storer.

import (
	"context"
	"errors"
	"io"
	"time"

	"clowder/protocol"
	"clowder/roster"
	"clowder/store"
)

func (d *Daemon) sweepSpool(ctx context.Context) {
	targets := map[string]bool{}
	for _, m := range d.spool.All() {
		targets[m.TargetKey] = true
	}
	for key := range targets {
		d.sweepSpoolFor(ctx, key)
	}
}

func (d *Daemon) sweepSpoolFor(ctx context.Context, targetKey string) {
	cat, ok := d.ros.GetByKey(targetKey)
	if !ok {
		return // unknown target: it must pair (or sync) with us first
	}
	for _, m := range d.spool.List(targetKey) {
		if err := d.deliverHeld(ctx, cat, m); err != nil {
			d.cfg.logf("clowder: pushing held %s to %s: %v", m.FileName, cat.Name, err)
			return // target unreachable; retry next sweep
		}
	}
}

// deliverHeld pushes one spooled file; the entry is deleted on delivery ack.
func (d *Daemon) deliverHeld(ctx context.Context, cat roster.Cat, m store.Meta) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	pc, err := d.connect(ctx, cat)
	if err != nil {
		return err
	}
	defer func() { _ = pc.Close() }()

	offer := &protocol.Offer{
		ID:         m.ID,
		FileName:   m.FileName,
		Size:       m.Size,
		From:       m.From,
		SHA256:     m.SHA256,
		TargetKey:  m.TargetKey,
		TargetName: cat.Name,
	}
	if err := pc.WriteMsg(&protocol.Message{Offer: offer}); err != nil {
		return err
	}
	// A refusal may mean the target already has the file; leave the entry.
	resp, err := pc.ReadMsg()
	if err != nil {
		return err
	}
	if resp.Answer == nil || !resp.Answer.OK {
		if resp.Answer != nil {
			return nil // refused: not an error, just not wanted
		}
		return errors.New("target sent no answer")
	}
	_, blob, err := d.spool.Open(m.ID)
	if err != nil {
		return err
	}
	defer blob.Close()
	_ = pc.SetDeadline(time.Now().Add(streamTimeout))
	if _, err := io.CopyN(pc.Writer(), blob, m.Size); err != nil {
		return err
	}
	_ = pc.SetDeadline(time.Now().Add(msgTimeout))
	ack, err := pc.ReadMsg()
	if err != nil {
		return err
	}
	if ack.Ack == nil || ack.Ack.Kind != protocol.AckDelivered {
		return errors.New("target did not acknowledge delivery")
	}
	if err := d.spool.Delete(m.ID); err != nil {
		d.cfg.logf("clowder: deleting delivered %s: %v", m.ID, err)
	}
	d.stats.add(func(s *Stats) { s.Pushed++ })
	d.cfg.logf("clowder: pushed held %s to %s", m.FileName, cat.Name)
	return nil
}
