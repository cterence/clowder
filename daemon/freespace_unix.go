//go:build !windows

package daemon

import "syscall"

// freeSpace reports the bytes available to unprivileged writes under
// dir. Overridable in tests.
var freeSpace = func(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return st.Bavail * uint64(st.Bsize), true
}
