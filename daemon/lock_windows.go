//go:build windows

package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockDir takes an exclusive lock on the config dir (single instance; the
// lock dies with the process, so no stale-lock problem).
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "clow.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: opening lock file: %w", err)
	}
	// LockFileEx conflicts between handles of the same process too.
	if err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &windows.Overlapped{}); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon: another clowder daemon is already running on %s", dir)
	}
	return func() { _ = f.Close() }, nil
}
