//go:build windows

package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockDir takes an exclusive lock on the daemon's config dir,
// enforcing single-instance: two daemons on one cat (same identity)
// cross-write the roster and every ledger and run two engines with
// the same node key, which wedges the tunnel (see the two-keypair
// invariant). The lock is held until the returned unlock runs (Run's
// exit) and dies with the process, so the lock file a crashed daemon
// left behind is relockable — no stale-lock problem.
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "clow.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: opening lock file: %w", err)
	}
	// LockFileEx conflicts between handles of the same process too,
	// so the guard also holds for two daemons in one process.
	if err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &windows.Overlapped{}); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon: another clowder daemon is already running on %s", dir)
	}
	return func() { _ = f.Close() }, nil
}
