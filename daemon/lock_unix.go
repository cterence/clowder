//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDir takes an exclusive advisory lock on the config dir (single
// instance; the lock dies with the process, so no stale-lock problem).
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "clow.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: opening lock file: %w", err)
	}
	// Flock conflicts even between two descriptors of the same process.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("daemon: another clowder daemon is already running on %s", dir)
		}
		return nil, fmt.Errorf("daemon: locking config dir: %w", err)
	}
	return func() { _ = f.Close() }, nil
}
