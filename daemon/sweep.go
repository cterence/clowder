package daemon

// Storer push sweep: a storer does not wait for its targets to poll.
// It periodically tries to deliver every held file whose target is in
// its roster, and retries right after accepting a new deposit, so a
// target that comes online receives its files within a poll interval.
// The sealed stream was sealed by the original sender to the target,
// so the storer relays it without being able to read it.

import (
	"context"
	"errors"
	"io"
	"time"

	"clowder/envelope"
	"clowder/protocol"
	"clowder/roster"
	"clowder/store"
)

// sweepSpool attempts delivery of every held file whose target is a
// known, reachable cat. Called from the poll ticker.
func (d *Daemon) sweepSpool(ctx context.Context) {
	targets := map[string]bool{}
	for _, m := range d.spool.All() {
		targets[m.TargetKey] = true
	}
	for key := range targets {
		d.sweepSpoolFor(ctx, key)
	}
}

// sweepSpoolFor attempts delivery of the files held for one target.
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

// deliverHeld pushes one spooled file to its target over a fresh
// connection, using the same Offer/stream/Ack flow as a direct send.
// The spool entry is deleted once the target acknowledges delivery.
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
		Receipt:    m.Receipt,
		Resumable:  m.Resumable,
	}
	if err := pc.WriteMsg(&protocol.Message{Offer: offer}); err != nil {
		return err
	}
	// The target may already have the file (racing its own pull): it
	// answers no and we leave the entry for the pull path to reconcile.
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
	// A resumable answer names the sealed offset the target already
	// holds: replay the stored header plus the stream from that
	// offset onward (a pure byte skip — the stream stays opaque).
	// The header must be re-sent so the target can recover the
	// per-stream secret, exactly like a sender's resumed attempt.
	resume := resp.Answer.Resume
	if resume > 0 {
		if _, err := io.CopyN(pc.Writer(), blob, envelope.HeaderLen); err != nil {
			return err
		}
		if _, err := blob.Seek(resume, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(pc.Writer(), blob, m.Size-resume); err != nil {
			return err
		}
	} else if _, err := io.CopyN(pc.Writer(), blob, m.Size); err != nil {
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
	d.stats.add(func(s *Stats) { s.Fetched++ })
	d.cfg.logf("clowder: pushed held %s to %s", m.FileName, cat.Name)
	return nil
}
