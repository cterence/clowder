//go:build windows

package daemon

import (
	"golang.org/x/sys/windows"
)

// freeSpace reports the bytes available to the caller's quota under
// dir (GetDiskFreeSpaceEx's avail, not the volume's free total). A
// failed query disables the reservation check for that receive.
var freeSpace = func(dir string) (uint64, bool) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var free, total, avail uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &avail); err != nil {
		return 0, false
	}
	return avail, true
}
