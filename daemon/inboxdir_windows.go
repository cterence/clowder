//go:build windows

package daemon

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// downloadDir returns the user's downloads directory via the Windows
// known-folder API, which follows folder redirection, falling back to
// the conventional <home>\Downloads.
func downloadDir() string {
	d, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0)
	if err == nil && filepath.IsAbs(d) {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Downloads")
}
