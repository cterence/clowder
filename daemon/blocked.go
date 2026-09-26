package daemon

// Blocked cats: keys of forgotten (and, later, distrusted) cats.
// Tailcat's AllowedClients is add-only, so a removed cat's key can
// still connect at the transport layer; the daemon refuses these
// peers at the protocol level instead. Persisted in blocked.json.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"clowder/roster"
)

func blockedPath(dir string) string { return filepath.Join(dir, "blocked.json") }

// loadBlocked reads the persisted blocklist.
func loadBlocked(dir string) map[string]bool {
	b, err := os.ReadFile(blockedPath(dir))
	if err != nil {
		return map[string]bool{}
	}
	var keys []string
	if json.Unmarshal(b, &keys) != nil {
		return map[string]bool{}
	}
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// saveBlocked persists the blocklist.
func saveBlocked(dir string, m map[string]bool) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	b, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return fmt.Errorf("daemon: encoding blocklist: %w", err)
	}
	return os.WriteFile(blockedPath(dir), b, 0o600)
}

// isBlockedKey reports whether a node key (identity or client) belongs
// to a blocked cat.
func (d *Daemon) isBlockedKey(key string) bool {
	if key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.blocked[key]
}

// blockCat adds a cat's keys to the blocklist and persists it.
func (d *Daemon) blockCat(c roster.Cat) error {
	d.mu.Lock()
	d.blocked[c.Key] = true
	if c.ClientKey != "" {
		d.blocked[c.ClientKey] = true
	}
	err := saveBlocked(d.cfg.Dir, d.blocked)
	d.mu.Unlock()
	return err
}
