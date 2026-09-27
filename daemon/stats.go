package daemon

import (
	"fmt"
	"path/filepath"
	"sync"

	"clowder/persist"
)

// Stats are a cat's lifetime counters, persisted in stats.json so they
// survive daemon restarts. Byte counts are plaintext bytes: what a user
// actually sent or received, not the sealed stream size.
type Stats struct {
	Sent          int64 `json:"sent"`       // files delivered to their target
	SentBytes     int64 `json:"sent_bytes"` // plaintext bytes delivered
	Received      int64 `json:"received"`   // files accepted into the inbox
	ReceivedBytes int64 `json:"received_bytes"`
	Spooled       int64 `json:"spooled"` // files parked for offline cats
	Pushed        int64 `json:"pushed"`  // held files pushed to their target by the sweep
}

// loadStats reads the counters from a config dir, starting at zero for a
// fresh cat.
func loadStats(dir string) Stats {
	var s Stats
	_, _ = persist.LoadJSON(filepath.Join(dir, "stats.json"), &s)
	return s
}

// statsKeeper guards the counters with their own mutex, kept separate
// so hot transfer paths never contend with roster state.
type statsKeeper struct {
	mu   sync.Mutex
	s    Stats
	path string
}

func newStatsKeeper(dir string) *statsKeeper {
	return &statsKeeper{s: loadStats(dir), path: filepath.Join(dir, "stats.json")}
}

// add applies fn to the counters and persists them.
func (k *statsKeeper) add(fn func(*Stats)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	fn(&k.s)
	// Persist failures are ignored: the counters keep running in
	// memory and persist on the next add.
	_ = persist.SaveJSON(k.path, k.s)
}

// snapshot returns the current counters.
func (k *statsKeeper) snapshot() Stats {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.s
}

// humanBytes renders a byte count for humans (KiB/MiB/GiB/TiB).
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	switch {
	case f >= unit*unit*unit*unit:
		return fmt.Sprintf("%.1f TiB", f/(unit*unit*unit*unit))
	case f >= unit*unit*unit:
		return fmt.Sprintf("%.1f GiB", f/(unit*unit*unit))
	case f >= unit*unit:
		return fmt.Sprintf("%.1f MiB", f/(unit*unit))
	default:
		return fmt.Sprintf("%.1f KiB", f/unit)
	}
}
