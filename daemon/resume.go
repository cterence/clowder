package daemon

// Resumable receives: a partial transfer stays on disk (parts/<id>.part
// plus a sidecar), and a re-offer answers with the offset to continue from
// instead of restarting — safe because a resumable sender re-seals every
// attempt with the same secret, so re-sent frames are byte-identical. The
// assembled plaintext must still match the announced size and digest
// before anything lands in the inbox. Stale parts are removed on mismatch
// and by cleanupParts once they stop progressing.

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

// partState is the durable sidecar of a partial receive. Size and SHA256
// pin the offer's announced values: a mismatch restarts from zero.
type partState struct {
	ID        string `json:"id"`
	FileName  string `json:"file_name"`
	Size      int64  `json:"size"`    // announced sealed-stream size
	SHA256    string `json:"sha256"`  // announced plaintext digest
	Offset    int64  `json:"offset"`  // sealed-stream bytes already received
	PlainLen  int64  `json:"plain"`   // plaintext bytes already received
	UpdatedAt int64  `json:"updated"` // unix seconds of the last checkpoint
}

// How long an unfinished part survives without progress.
const partTTL = 7 * 24 * time.Hour

// partCheckpointBytes is how much sealed stream must flow between
// sidecar checkpoints; the first chunk always checkpoints, so a cut
// early in the transfer still resumes.
const partCheckpointBytes = 4 << 20

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

// resumePoint reports the offset a retry should continue from, 0 for a
// fresh receive. A part only counts when sidecar and part file match the
// offer; anything else restarts fresh (stale files removed).
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
	// Clamp to the last full-chunk boundary: a checkpoint after the final
	// chunk names the end of a short frame, which SealedToPlain rejects —
	// answering with it wedges the transfer. The plaintext past the
	// boundary is re-sent.
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

// checkpointPart persists the resume offset and plaintext length so far.
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

// cleanupParts drops parts idle beyond partTTL, plus orphans.
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

// receiveResumable answers with our offset, decrypts into the part file,
// checkpoints per chunk, and moves the assembled plaintext into the inbox
// once it matches the announced size and digest.
func (d *Daemon) receiveResumable(pc *protocol.Conn, o *protocol.Offer, resume int64) (int64, bool) {
	if err := pc.AnswerResume(o.ID, resume); err != nil {
		return 0, false
	}
	_ = pc.SetDeadline(time.Now().Add(streamTimeout))

	if err := os.MkdirAll(partsDir(d.cfg.Dir), 0o700); err != nil {
		d.cfg.logf("clowder: receiving %s: %v", o.FileName, err)
		return 0, false
	}

	// A resumed attempt carries the header plus the frames from resume; the
	// sender must send exactly that many bytes (the size-lie check).
	expected := o.Size
	if resume > 0 {
		expected = int64(envelope.HeaderLen) + o.Size - resume
	}
	ex := &exactReader{r: countingReader{k: d.prog, id: o.ID, r: io.LimitReader(pc.Reader(), expected)}}

	plainKept := int64(0)
	var f *os.File
	var err error
	if resume > 0 {
		// Truncate the part to the boundary's plaintext — a torn tail from a
		// killed process — so the suffix appends to a clean prefix.
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
	// Checkpoint the sidecar every checkpointBytes of stream (and on
	// the first chunk), not per chunk: a sidecar write is an atomic
	// rename, and one per 64 KiB chunk multiplies disk I/O on large
	// transfers. A cut discards only the uncheckpointed tail.
	var sinceCkpt int64 = partCheckpointBytes
	prevOff := resume
	if prevOff < envelope.HeaderLen {
		prevOff = envelope.HeaderLen
	}
	openErr := func() error {
		defer f.Close()
		var err error
		_, _, err = envelope.OpenStreamAt(d.env.Identity.Private, ex, f, resume, func(nextOff, plainLen int64) {
			suffixPlain += plainLen
			sinceCkpt += nextOff - prevOff
			prevOff = nextOff
			if sinceCkpt >= partCheckpointBytes {
				d.checkpointPart(o, nextOff, plainKept+suffixPlain)
				sinceCkpt = 0
			}
		})
		return err
	}()
	if openErr != nil {
		// Cut the part back to the recorded prefix: no torn tail.
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

	size, err := d.finishPart(o, plainKept+suffixPlain)
	if err != nil {
		d.cfg.logf("clowder: receiving %s: %v", o.FileName, err)
		d.dropPart(o.ID)
		return 0, false
	}
	return size, true
}

// finishPart verifies the digest, moves the part into the inbox, drops
// the sidecar.
func (d *Daemon) finishPart(o *protocol.Offer, plainLen int64) (int64, error) {
	src := partPath(d.cfg.Dir, o.ID)
	fi, err := os.Stat(src)
	if err != nil {
		return 0, err
	}
	if fi.Size() != plainLen {
		return 0, fmt.Errorf("part file holds %d bytes, accounted %d", fi.Size(), plainLen)
	}
	pf, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, pf); err != nil {
		_ = pf.Close()
		return 0, err
	}
	_ = pf.Close()
	gotSha := hex.EncodeToString(h.Sum(nil))
	if gotSha != o.SHA256 {
		return 0, fmt.Errorf("digest mismatch: got %s, announced %s", gotSha, o.SHA256)
	}
	inbox := d.InboxDir()
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		return 0, err
	}
	if err := os.Rename(src, inboxPath(inbox, o.FileName)); err != nil {
		return 0, err
	}
	_ = os.Remove(partStatePath(d.cfg.Dir, o.ID))
	return plainLen, nil
}
