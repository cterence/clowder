package daemon

// In-flight transfer progress, surfaced by `clow status`. Progress is
// counted on the sealed stream (the sender writes it, the receiver
// reads it), one update per 64 KiB chunk, and is keyed by the transfer
// ID: the outbox entry ID for sends, the offer ID for receives.

import (
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

// progressKeeper tracks in-flight transfers.
type progressKeeper struct {
	mu sync.Mutex
	m  map[string]Progress
}

func newProgressKeeper() *progressKeeper {
	return &progressKeeper{m: map[string]Progress{}}
}

// start registers an in-flight transfer.
func (k *progressKeeper) start(p Progress) {
	if p.Started == 0 {
		p.Started = time.Now().Unix()
	}
	k.mu.Lock()
	k.m[p.ID] = p
	k.mu.Unlock()
}

// add advances a transfer's byte counter.
func (k *progressKeeper) add(id string, n int64) {
	k.mu.Lock()
	if p, ok := k.m[id]; ok {
		p.Done += n
		k.m[id] = p
	}
	k.mu.Unlock()
}

// end removes a transfer, finished or failed.
func (k *progressKeeper) end(id string) {
	k.mu.Lock()
	delete(k.m, id)
	k.mu.Unlock()
}

// snapshot returns the in-flight transfers, oldest first.
func (k *progressKeeper) snapshot() []Progress {
	k.mu.Lock()
	out := make([]Progress, 0, len(k.m))
	for _, p := range k.m {
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

// countingWriter wraps a writer, advancing a transfer's progress.
type countingWriter struct {
	k  *progressKeeper
	id string
	w  writeOnly
}

type writeOnly interface{ Write([]byte) (int, error) }

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.k.add(c.id, int64(n))
	return n, err
}

// countingReader wraps a reader the same way.
type countingReader struct {
	k  *progressKeeper
	id string
	r  readOnly
}

type readOnly interface{ Read([]byte) (int, error) }

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.k.add(c.id, int64(n))
	return n, err
}
