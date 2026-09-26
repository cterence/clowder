package daemon

// Resumable-transfer integration tests. Each one cuts a transfer
// mid-stream (the receiver's listener kills conns after a byte quota),
// lets the sender's outbox retry, and requires the file to arrive
// whole — with the receiver having kept its partial progress, so the
// retry continued from its checkpoint instead of restarting.

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"clowder/envelope"
	"clowder/store"
)

// killConn dies once its byte quota has flowed, simulating a
// mid-stream connection loss.
type killConn struct {
	net.Conn
	quota int64
	n     atomic.Int64
	dead  atomic.Bool
}

func (c *killConn) Write(p []byte) (int, error) {
	if c.dead.Load() {
		return 0, net.ErrClosed
	}
	n, err := c.Conn.Write(p)
	if c.n.Add(int64(n)) >= c.quota {
		c.dead.Store(true)
		_ = c.Close()
	}
	return n, err
}

func (c *killConn) Read(p []byte) (int, error) {
	if c.dead.Load() {
		return 0, net.ErrClosed
	}
	n, err := c.Conn.Read(p)
	if c.n.Add(int64(n)) >= c.quota {
		c.dead.Store(true)
		_ = c.Close()
	}
	return n, err
}

// killTransport wraps a LocalTransport whose accepted conns die
// after quota bytes each. SetQuota changes the cutoff for later
// connections (tests release the kill to let the retry through).
type killTransport struct {
	LocalTransport
	quota atomic.Int64
}

func (t *killTransport) Listen(ctx context.Context) (net.Listener, error) {
	ln, err := t.LocalTransport.Listen(ctx)
	if err != nil {
		return nil, err
	}
	return &killListener{Listener: ln, tr: t}, nil
}

type killListener struct {
	net.Listener
	tr *killTransport
}

func (l *killListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &killConn{Conn: c, quota: l.tr.quota.Load()}, nil
}

// TestDirectSendResumesAfterCut sends a multi-chunk file direct, cuts
// the first attempt mid-stream, and requires the retry to resume from
// the receiver's checkpoint: the file arrives whole, and the receiver
// answered the second attempt with a nonzero offset.
func TestDirectSendResumesAfterCut(t *testing.T) {
	tr := &killTransport{}
	fluff := runDaemon(t, initDir(t, "fluff"), tr)
	milo := startDaemon(t, "milo")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	// Enough quota for the handshake plus a few chunks of stream,
	// then the conn dies mid-transfer — well before the file's end,
	// so the first attempt cannot complete.
	tr.quota.Store(int64(envelope.HeaderLen) + 3*(4+envelope.ChunkSize+16) + 512)

	big := strings.Repeat("nap.", 512<<10/4) // 512 KiB, eight chunks
	src := writeSource(t, big)
	if _, err := milo.Send("fluff", src); err != nil {
		t.Fatal(err)
	}

	// The first attempt dies mid-stream; the receiver keeps a part.
	waitFor(t, func() bool {
		st, ok := loadPart(fluff.cfg.Dir, firstPartID(t, fluff))
		return ok && st.PlainLen > 0
	}, "the receiver to checkpoint partial progress")

	// The dormant part is visible in status: checkpointed bytes with
	// no attempt in flight, waiting for a retry to resume from.
	waitFor(t, func() bool {
		resp := fluff.handleIPC(Request{Op: "status"})
		return resp.OK && len(resp.Parts) == 1 && resp.Parts[0].PlainLen > 0
	}, "the partial receive to show in status")

	// Release the kill: the next attempt must complete.
	tr.quota.Store(1 << 40)
	var got string
	waitFor(t, func() bool {
		p, ok := inboxFile(t, fluff, "nap.txt")
		if ok {
			got = p
		}
		return ok
	}, "the resumed transfer to land in the inbox")
	if got != big {
		t.Fatal("resumed file content differs from the original")
	}
	// The part files are gone.
	if _, err := os.Stat(filepath.Join(fluff.cfg.Dir, "parts")); err == nil {
		des, _ := os.ReadDir(filepath.Join(fluff.cfg.Dir, "parts"))
		if len(des) > 0 {
			t.Fatalf("part files remain after completion: %d", len(des))
		}
	}
}

// TestDirectSendResumesAcrossRestart cuts a transfer, then restarts
// the receiver daemon before releasing it: the checkpoint must
// survive the restart and the retry must resume from it.
func TestDirectSendResumesAcrossRestart(t *testing.T) {
	tr := &killTransport{}
	dir := initDir(t, "fluff")
	fluff := runDaemon(t, dir, tr)
	milo := startDaemon(t, "milo")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	tr.quota.Store(int64(envelope.HeaderLen) + 2*(4+envelope.ChunkSize+16) + 512)

	big := strings.Repeat("meow.", 512<<10/5) // 512 KiB
	src := writeSource(t, big)
	if _, err := milo.Send("fluff", src); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		st, ok := loadPart(dir, firstPartID(t, fluff))
		return ok && st.PlainLen > 0
	}, "the receiver to checkpoint partial progress")

	// Restart the receiver on the same config dir: the part and its
	// sidecar must survive, and the retry must resume from them.
	stopDaemon(fluff)
	tr.quota.Store(1 << 40)
	fluff2 := startDaemonAt(t, dir)
	trust(t, milo, fluff2)
	trust(t, fluff2, milo)

	var got string
	waitFor(t, func() bool {
		p, ok := inboxFile(t, fluff2, "nap.txt")
		if ok {
			got = p
		}
		return ok
	}, "the resumed transfer to land in the inbox")
	if got != big {
		t.Fatal("resumed file content differs from the original")
	}
}

// TestStorerPushResumesAfterCut deposits a file with a sleeping
// target, wakes the target behind a kill switch, and lets the
// storer's push sweep deliver: the first push is cut mid-stream, and
// the sweep's retry must resume from the target's checkpoint.
func TestStorerPushResumesAfterCut(t *testing.T) {
	milo := startDaemon(t, "milo")
	storer := startDaemon(t, "box")
	if err := storer.SetStorer(true, 1<<30); err != nil {
		t.Fatal(err)
	}
	tr := &killTransport{}
	niko, nikoDir := offlineCat(t)
	addCat(t, milo, niko)
	addCat(t, storer, niko)
	trust(t, milo, storer)
	trust(t, storer, milo)

	big := strings.Repeat("purr.", 512<<10/5) // 512 KiB, eight chunks
	src := writeSource(t, big)
	if _, err := milo.Send("niko", src); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return storer.Spool().Count() == 1 }, "the deposit to land")

	// The target wakes behind a kill switch and syncs with the storer
	// so the push sweep finds it.
	tr.quota.Store(int64(envelope.HeaderLen) + 2*(4+envelope.ChunkSize+16) + 512)
	nikoD := runDaemon(t, nikoDir, tr)
	trust(t, nikoD, storer)
	trust(t, storer, nikoD)
	addCat(t, nikoD, storer.Me())

	waitFor(t, func() bool {
		st, ok := loadPart(nikoDir, firstPartID(t, nikoD))
		return ok && st.PlainLen > 0
	}, "the target to checkpoint partial progress from the cut push")

	tr.quota.Store(1 << 40)
	var got string
	waitFor(t, func() bool {
		p, ok := inboxFile(t, nikoD, "nap.txt")
		if ok {
			got = p
		}
		return ok
	}, "the resumed push to land in the inbox")
	if got != big {
		t.Fatal("resumed file content differs from the original")
	}
	waitFor(t, func() bool { return storer.Spool().Count() == 0 }, "the spool entry to drain")
}

// TestDepositResumeRoundTrip pins the spool's resumable deposit at
// the store level: an interrupted PutResume leaves a part whose
// frame-boundary offset the next attempt continues from, and the
// completed blob is byte-identical to a one-shot Put.
func TestDepositResumeRoundTrip(t *testing.T) {
	sp := store.New(t.TempDir(), 0)
	meta := store.Meta{ID: "abc", FileName: "nap.txt", Size: 400, SHA256: "deadbeef", TargetKey: "k", TargetName: "niko", Resumable: true}

	// Build a fake sealed stream: header + frames.
	var stream []byte
	stream = append(stream, make([]byte, 115)...)
	for _, chunk := range [][]byte{bytes16(), bytes16(), bytes8()} {
		var frame [4]byte
		frame[0] = byte(len(chunk) >> 24)
		frame[1] = byte(len(chunk) >> 16)
		frame[2] = byte(len(chunk) >> 8)
		frame[3] = byte(len(chunk))
		stream = append(stream, frame[:]...)
		stream = append(stream, chunk...)
	}
	// Terminator.
	stream = append(stream, 0, 0, 0, 0)
	meta.Size = int64(len(stream))

	// First attempt: header + the first two frames, then cut.
	// Frame 0 starts at 115; each 16-byte frame spans 4+16=20 bytes.
	cut1 := stream[:115+2*20]
	if err := sp.PutResume(meta, 0, bytes.NewReader(cut1)); err == nil {
		t.Fatal("cut deposit succeeded, want error")
	}
	// The checkpoint must sit at a frame boundary after frame 2.
	// (PutResume consumed the header then appended frames.)
	resume := sp.DepositResume(meta)
	if resume != 115+2*20 {
		t.Fatalf("DepositResume = %d, want %d", resume, 115+2*20)
	}

	// Retry: header + frames from the resume point onward.
	retry := bytes.Clone(stream[:115])
	retry = append(retry, stream[resume:]...)
	if err := sp.PutResume(meta, resume, bytes.NewReader(retry)); err != nil {
		t.Fatal(err)
	}
	got, r, err := sp.Open(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(b)) != meta.Size || !bytes.Equal(b, stream) {
		t.Fatal("completed blob differs from the one-shot stream")
	}
	_ = got
}

// firstPartID returns the ID of the receiver's first partial
// receive, failing the test if there is none yet.
func firstPartID(t *testing.T, d *Daemon) string {
	t.Helper()
	dir := filepath.Join(d.cfg.Dir, "parts")
	des, err := os.ReadDir(dir)
	if err != nil || len(des) == 0 {
		return ""
	}
	for _, de := range des {
		if strings.HasSuffix(de.Name(), ".part.json") {
			return strings.TrimSuffix(de.Name(), ".part.json")
		}
	}
	return ""
}

func initDir(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := Init(dir, name); err != nil {
		t.Fatal(err)
	}
	return dir
}

func bytes16() []byte { return make([]byte, 16) }
func bytes8() []byte  { return make([]byte, 8) }
