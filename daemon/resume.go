package daemon

// Resumable receives: a receiver keeps a partially transferred file
// on disk (parts/<id>.part plus a small sidecar) and, when the same
// transfer is offered again, answers with the sealed-stream offset it
// wants the rest from instead of starting over. This works because a
// resumable sender seals every attempt of one transfer with the same
// per-stream secret, so the frames it re-sends are byte-identical to
// the originals (see envelope.SealStreamAt): the receiver's kept
// prefix stays valid across retries. Its end-to-end checks are
// unchanged — the assembled plaintext must still match the announced
// size and digest before anything lands in the inbox.
//
// Parts live under <config>/parts. They are removed on completion, on
// an offer/size/digest mismatch (a restart-from-zero), and — for
// transfers that never come back — by cleanupParts once they stop
// progressing.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clowder/envelope"
	"clowder/persist"
	"clowder/protocol"
)

// partState is the durable sidecar of a partially received transfer.
// Size and SHA256 pin the announced values of the offer the part came
// from: an offer for the same ID but different values means the part
// does not belong to it, and receiving restarts from zero.
type partState struct {
	ID        string `json:"id"`
	FileName  string `json:"file_name"`
	Size      int64  `json:"size"`    // announced sealed-stream size
	SHA256    string `json:"sha256"`  // announced plaintext digest
	Offset    int64  `json:"offset"`  // sealed-stream bytes already received
	PlainLen  int64  `json:"plain"`   // plaintext bytes already received
	UpdatedAt int64  `json:"updated"` // unix seconds of the last checkpoint
}

// partTTL is how long an unfinished part survives without progress
// before the sweep deletes it: an offer that never returns is
// eventually forgotten, like a spool entry past its TTL.
const partTTL = 7 * 24 * time.Hour

func partsDir(dir string) string          { return filepath.Join(dir, "parts") }
func partPath(dir, id string) string      { return filepath.Join(dir, "parts", id+".part") }
func partStatePath(dir, id string) string { return filepath.Join(dir, "parts", id+".part.json") }

// loadPart reads a transfer's sidecar, if any.
func loadPart(dir, id string) (partState, bool) {
	var st partState
	if _, err := persist.LoadJSON(partStatePath(dir, id), &st); err != nil || st.ID != id {
		return partState{}, false
	}
	return st, true
}

// resumePoint reports the sealed-stream offset a retry of offer o
// should continue from, 0 for a fresh receive. A part only counts
// when its sidecar matches the offer's announced file name, size and
// digest and its .part file really holds the recorded plaintext
// prefix; anything else means the part is unusable and the transfer
// starts fresh (the stale files are removed).
func (d *Daemon) resumePoint(o *protocol.Offer) int64 {
	st, ok := loadPart(d.cfg.Dir, o.ID)
	if !ok {
		return 0
	}
	if st.FileName != o.FileName || st.Size != o.Size || st.SHA256 != o.SHA256 {
		d.dropPart(o.ID)
		return 0
	}
	if st.Offset <= 0 || st.PlainLen < 0 || st.Offset >= st.Size {
		d.dropPart(o.ID)
		return 0
	}
	if fi, err := os.Stat(partPath(d.cfg.Dir, o.ID)); err != nil || fi.Size() < st.PlainLen {
		d.dropPart(o.ID)
		return 0
	}
	// Clamp to the last full-chunk boundary: a checkpoint taken after
	// the final chunk (a cut in the terminator window) names the end
	// of a short frame, which SealedToPlain rejects on both sides —
	// answering with it verbatim wedges the transfer, retrying an
	// offset no attempt can use. The plaintext past the boundary is
	// discarded and re-sent (receiveResumable truncates to it).
	resume := envelope.LastResumeBoundary(st.PlainLen)
	if resume <= envelope.HeaderLen {
		// Less than one full chunk held: not worth resuming.
		d.dropPart(o.ID)
		return 0
	}
	return resume
}

// dropPart removes a transfer's partial receive, if any.
func (d *Daemon) dropPart(id string) {
	_ = os.Remove(partPath(d.cfg.Dir, id))
	_ = os.Remove(partStatePath(d.cfg.Dir, id))
}

// checkpointPart persists a partial receive after a chunk boundary:
// the resume offset (the start of the next frame) and the plaintext
// length the .part file now holds.
func (d *Daemon) checkpointPart(o *protocol.Offer, offset, plainLen int64) {
	st := partState{
		ID:        o.ID,
		FileName:  o.FileName,
		Size:      o.Size,
		SHA256:    o.SHA256,
		Offset:    offset,
		PlainLen:  plainLen,
		UpdatedAt: time.Now().Unix(),
	}
	if err := persist.SaveJSON(partStatePath(d.cfg.Dir, o.ID), st); err != nil {
		d.cfg.logf("clowder: checkpointing %s: %v", o.ID, err)
	}
}

// cleanupParts drops parts whose sidecar is older than partTTL, plus
// orphaned files without their counterpart. Called from the poll
// tick.
func (d *Daemon) cleanupParts() {
	dir := partsDir(d.cfg.Dir)
	des, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-partTTL).Unix()
	for _, de := range des {
		name := de.Name()
		switch {
		case strings.HasSuffix(name, ".part.json"):
			id := strings.TrimSuffix(name, ".part.json")
			st, ok := loadPart(d.cfg.Dir, id)
			if !ok || st.UpdatedAt < cutoff {
				_ = os.Remove(filepath.Join(dir, id+".part"))
				_ = os.Remove(filepath.Join(dir, name))
			}
		case strings.HasSuffix(name, ".part"):
			id := strings.TrimSuffix(name, ".part")
			if _, ok := loadPart(d.cfg.Dir, id); !ok {
				_ = os.Remove(filepath.Join(dir, name))
			}
		}
	}
}

// receiveResumable answers offer o with the sealed-stream offset this
// receiver wants the rest from (resume, 0 for a fresh receive), decrypts
// the attempt's stream into the partial-receive file, checkpoints after
// each chunk, and — once the assembled plaintext matches the announced
// size and digest — moves it into the inbox. It returns the whole
// plaintext size and whether the exchange completed.
func (d *Daemon) receiveResumable(pc *protocol.Conn, o *protocol.Offer, resume int64) (int64, bool) {
	if err := pc.AnswerResume(o.ID, resume); err != nil {
		return 0, false
	}
	_ = pc.SetDeadline(time.Now().Add(streamTimeout))

	if err := os.MkdirAll(partsDir(d.cfg.Dir), 0o700); err != nil {
		d.cfg.logf("clowder: receiving %s: %v", o.FileName, err)
		return 0, false
	}

	// A resumed attempt carries the header plus the frames from
	// resume onward; a fresh one carries the whole announced stream.
	// The sender must send exactly that many bytes (the size-lie
	// check mirrors saveIncoming's).
	expected := o.Size
	if resume > 0 {
		expected = int64(envelope.HeaderLen) + o.Size - resume
	}
	ex := &exactReader{r: countingReader{k: d.prog, id: o.ID, r: io.LimitReader(pc.Reader(), expected)}}

	plainKept := int64(0)
	var f *os.File
	var err error
	if resume > 0 {
		// resume names a full-chunk boundary (resumePoint clamped it),
		// so the plaintext it maps to is what the part file must hold
		// exactly. Truncate away anything past it — a torn tail from a
		// killed process, or the final short chunk a checkpoint after
		// the boundary had already written — so the suffix appends to
		// a clean prefix, and checkpoint the state before streaming.
		plainKept, _, err = envelope.SealedToPlain(resume)
		if err == nil {
			f, err = os.OpenFile(partPath(d.cfg.Dir, o.ID), os.O_WRONLY, 0o600)
		}
		if err == nil {
			err = f.Truncate(plainKept)
		}
		if err == nil {
			_, err = f.Seek(plainKept, io.SeekStart)
		}
		if err != nil {
			if f != nil {
				_ = f.Close()
			}
			d.cfg.logf("clowder: receiving %s: %v", o.FileName, err)
			return 0, false
		}
		d.checkpointPart(o, resume, plainKept)
	} else {
		d.dropPart(o.ID)
		f, err = os.OpenFile(partPath(d.cfg.Dir, o.ID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	}
	if err != nil {
		d.cfg.logf("clowder: receiving %s: %v", o.FileName, err)
		return 0, false
	}
	var suffixPlain int64
	openErr := func() error {
		defer f.Close()
		var err error
		_, _, err = envelope.OpenStreamAt(d.env.Identity.Private, ex, f, resume, func(nextOff, plainLen int64) {
			suffixPlain += plainLen
			d.checkpointPart(o, nextOff, plainKept+suffixPlain)
		})
		return err
	}()
	_ = suffixPlain
	if openErr != nil {
		// Cut the part back to the recorded prefix: a stream that
		// died mid-chunk must not leave a torn tail appended.
		if st, ok := loadPart(d.cfg.Dir, o.ID); ok && st.PlainLen > 0 {
			_ = os.Truncate(partPath(d.cfg.Dir, o.ID), st.PlainLen)
		} else {
			d.dropPart(o.ID)
		}
		return 0, false
	}
	if ex.n != expected {
		d.cfg.logf("clowder: receiving %s: protocol violation: attempt stream was %d bytes, %d expected", o.FileName, ex.n, expected)
		d.dropPart(o.ID)
		return 0, false
	}

	size, name, err := d.finishPart(o, plainKept+suffixPlain)
	if err != nil {
		d.cfg.logf("clowder: receiving %s: %v", o.FileName, err)
		d.dropPart(o.ID)
		return 0, false
	}
	_ = name
	return size, true
}

// finishPart verifies the assembled plaintext against the announced
// digest, moves the part file into the inbox under a unique name, and
// drops the partial-receive state.
func (d *Daemon) finishPart(o *protocol.Offer, plainLen int64) (int64, string, error) {
	src := partPath(d.cfg.Dir, o.ID)
	fi, err := os.Stat(src)
	if err != nil {
		return 0, "", err
	}
	if fi.Size() != plainLen {
		return 0, "", fmt.Errorf("part file holds %d bytes, accounted %d", fi.Size(), plainLen)
	}
	pf, err := os.Open(src)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, pf); err != nil {
		_ = pf.Close()
		return 0, "", err
	}
	_ = pf.Close()
	gotSha := hex.EncodeToString(h.Sum(nil))
	if gotSha != o.SHA256 {
		return 0, "", fmt.Errorf("digest mismatch: got %s, announced %s", gotSha, o.SHA256)
	}
	inbox := d.InboxDir()
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		return 0, "", err
	}
	name := inboxPath(inbox, o.FileName)
	if err := os.Rename(src, name); err != nil {
		return 0, "", err
	}
	_ = os.Remove(partStatePath(d.cfg.Dir, o.ID))
	return plainLen, name, nil
}

// partSnapshot lists partial receives for `clow status`.
func (d *Daemon) partSnapshot() []partState {
	des, err := os.ReadDir(partsDir(d.cfg.Dir))
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-partTTL).Unix()
	var out []partState
	for _, de := range des {
		if !strings.HasSuffix(de.Name(), ".part.json") {
			continue
		}
		id := strings.TrimSuffix(de.Name(), ".part.json")
		st, ok := loadPart(d.cfg.Dir, id)
		if ok && st.UpdatedAt >= cutoff {
			out = append(out, st)
		}
	}
	return out
}
