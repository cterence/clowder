//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDir takes an exclusive advisory lock on the daemon's config
// dir, enforcing single-instance: two daemons on one cat (same
// identity) cross-write the roster and every ledger and run two
// engines with the same node key, which wedges the tunnel (see the
// two-keypair invariant). The lock is held until the returned unlock
// runs (Run's exit) and dies with the process, so the lock file a
// crashed daemon left behind is relockable — no stale-lock problem.
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "clow.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: opening lock file: %w", err)
	}
	// Flock conflicts even between two descriptors of the same
	// process, so the guard also holds for two daemons in one
	// process (tests, library use).
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("daemon: another clowder daemon is already running on %s", dir)
		}
		return nil, fmt.Errorf("daemon: locking config dir: %w", err)
	}
	return func() { _ = f.Close() }, nil
}
