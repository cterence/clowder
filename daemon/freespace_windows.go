//go:build windows

package daemon

// freeSpace is unimplemented on Windows: receives proceed without a
// reservation check there.
var freeSpace = func(string) (uint64, bool) { return 0, false }
