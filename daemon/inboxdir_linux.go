//go:build linux

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// downloadDir returns the user's downloads directory, honoring the XDG
// user-dirs configuration (which xdg-user-dir resolves, including
// locale-specific choices) when available, else the conventional
// $HOME/Downloads.
func downloadDir() string {
	if out, err := exec.Command("xdg-user-dir", "DOWNLOAD").Output(); err == nil {
		if d := strings.TrimSpace(string(out)); filepath.IsAbs(d) {
			return d
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Downloads")
}
