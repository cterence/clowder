//go:build !linux && !windows

package daemon

import (
	"os"
	"path/filepath"
)

// downloadDir returns the user's downloads directory. On macOS that is
// ~/Downloads, a system convention; elsewhere it is the conventional
// equivalent under the home directory.
func downloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Downloads")
}
