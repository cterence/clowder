package daemon

// In-flight transfer progress, surfaced by `clow status`. Progress is
// counted on the sealed stream (the sender writes it, the receiver
// reads it), one update per 64 KiB chunk, and is keyed by the transfer
// ID: the outbox entry ID for sends, the offer ID for receives.

import (
	"io"
	"slices"
	"sync"
	"time"
)

// Progress is one in-flight transfer.
type Progress struct {
	ID        string `json:"id"`
	FileName  string `json:"file_name"`
	Peer      string `json:"peer"` // target when sending, sender when receiving
	Receiving bool   `json:"receiving,omitempty"`
	// Done and Total are sealed-stream bytes; the transfer ends when
	// Done reaches Total.
	Done    int64 `json:"done"`
	Total   int64 `json:"total"`
	Started int64 `json:"started"` // unix seconds
	// Bps is the transfer's current rate over a short rolling window,
	// filled in by snapshot: 0 when too little time has passed to
	// measure one.
	Bps float64 `json:"bps,omitempty"`
}

// Percent returns the completion fraction 0..1, or 0 with a zero total.
func (p Progress) Percent() float64 {
	if p.Total <= 0 {
		return 0
	}
	if p.Done > p.Total {
		return 1
	}
	return float64(p.Done) / float64(p.Total)
}

// rateWindow is how far back snapshot looks to compute a transfer's
// rate: long enough for a few chunk updates even on a slow relay,
// short enough that a stalled transfer decays to zero promptly.
const rateWindow = 3 * time.Second

// progressKeeper tracks in-flight transfers.
type progressKeeper struct {
	mu sync.Mutex
	m  map[string]Progress
	// samples holds recent (time, cumulative Done) pairs per transfer
	// for rate computation; anything older than rateWindow is pruned.
	samples map[string][]progressSample
	// now is the clock, overridable in tests.
	now func() time.Time
}

// progressSample is one rate measurement point.
type progressSample struct {
	t    time.Time
	done int64
}

func newProgressKeeper() *progressKeeper {
	return &progressKeeper{m: map[string]Progress{}, samples: map[string][]progressSample{}}
}

func (k *progressKeeper) clock() time.Time {
	if k.now != nil {
		return k.now()
	}
	return time.Now()
}

// start registers an in-flight transfer.
func (k *progressKeeper) start(p Progress) {
	if p.Started == 0 {
		p.Started = k.clock().Unix()
	}
	k.mu.Lock()
	k.m[p.ID] = p
	k.samples[p.ID] = []progressSample{{t: k.clock(), done: p.Done}}
	k.mu.Unlock()
}

// add advances a transfer's byte counter.
func (k *progressKeeper) add(id string, n int64) {
	k.mu.Lock()
	if p, ok := k.m[id]; ok {
		p.Done += n
		k.m[id] = p
		k.pruneLocked(id, k.clock())
		k.samples[id] = append(k.samples[id], progressSample{t: k.clock(), done: p.Done})
	}
	k.mu.Unlock()
}

// end removes a transfer, finished or failed.
func (k *progressKeeper) end(id string) {
	k.mu.Lock()
	delete(k.m, id)
	delete(k.samples, id)
	k.mu.Unlock()
}

// pruneLocked drops samples older than the rate window.
func (k *progressKeeper) pruneLocked(id string, now time.Time) {
	s := k.samples[id]
	cut := 0
	for cut < len(s) && now.Sub(s[cut].t) > rateWindow {
		cut++
	}
	k.samples[id] = s[cut:]
}

// snapshot returns the in-flight transfers, oldest first, each with
// its rolling-window rate.
func (k *progressKeeper) snapshot() []Progress {
	now := k.clock()
	k.mu.Lock()
	out := make([]Progress, 0, len(k.m))
	for id, p := range k.m {
		p.Bps = k.rateLocked(id, now)
		out = append(out, p)
	}
	k.mu.Unlock()
	slices.SortFunc(out, func(a, b Progress) int {
		if a.Started != b.Started {
			return int(a.Started - b.Started)
		}
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	return out
}

// rateLocked computes a transfer's bytes per second across the samples
// inside the rate window. The sample at the window's start counts as
// the baseline, so bursts do not skew the result. With no elapsed time
// in the window it reports 0.
func (k *progressKeeper) rateLocked(id string, now time.Time) float64 {
	s := k.samples[id]
	if len(s) == 0 {
		return 0
	}
	base := s[0]
	if now.Sub(base.t) > rateWindow {
		return 0 // everything is stale: the transfer stalled
	}
	elapsed := now.Sub(base.t).Seconds()
	if elapsed < 0.25 {
		return 0
	}
	return float64(s[len(s)-1].done-base.done) / elapsed
}

// countingWriter wraps a writer, advancing a transfer's progress.
type countingWriter struct {
	k  *progressKeeper
	id string
	w  io.Writer
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.k.add(c.id, int64(n))
	return n, err
}

// countingReader wraps a reader the same way.
type countingReader struct {
	k  *progressKeeper
	id string
	r  io.Reader
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.k.add(c.id, int64(n))
	return n, err
}
