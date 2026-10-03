package daemon

// Blocked cats: keys of forgotten cats, refused at the protocol level
// (tailcat's AllowedClients is add-only). Persisted in blocked.json.

import (
	"path/filepath"

	"github.com/cterence/clowder/persist"
	"github.com/cterence/clowder/protocol"
	"github.com/cterence/clowder/roster"
)

func blockedPath(dir string) string { return filepath.Join(dir, "blocked.json") }

func loadBlocked(dir string) map[string]bool {
	var keys []string
	if _, err := persist.LoadJSON(blockedPath(dir), &keys); err != nil {
		return map[string]bool{} // unreadable blocklist: start empty
	}
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func saveBlocked(dir string, m map[string]bool) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return persist.SaveJSON(blockedPath(dir), keys)
}

func (d *Daemon) isBlockedKey(key string) bool {
	if key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.blocked[key]
}

func (d *Daemon) blockCat(c roster.Cat) error {
	d.mu.Lock()
	d.blocked[c.Key] = true
	if c.DialKey != "" {
		d.blocked[c.DialKey] = true
	}
	err := saveBlocked(d.cfg.Dir, d.blocked)
	d.mu.Unlock()
	return err
}

// isBlockedName matches a declared name to the blocklist — relayed offers
// from pre-key senders carry only the sender's name, not its key.
func (d *Daemon) isBlockedName(name string) bool {
	cat, ok := d.ros.Get(name)
	return ok && d.isBlockedKey(cat.Key)
}

// isBlockedSender checks an offer's sender: by node key when the offer
// carries one, else by name for pre-key senders. The key path works
// after a forget — the roster no longer resolves the name then.
func (d *Daemon) isBlockedSender(o *protocol.Offer) bool {
	if o.FromKey != "" {
		return d.isBlockedKey(o.FromKey)
	}
	return d.isBlockedName(o.From)
}
