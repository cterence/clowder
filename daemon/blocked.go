package daemon

// Blocked cats: keys of forgotten/distrusted cats, refused at the protocol
// level (tailcat's AllowedClients is add-only). Persisted in blocked.json.

import (
	"fmt"
	"path/filepath"

	"clowder/persist"
	"clowder/roster"
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
	if c.ClientKey != "" {
		d.blocked[c.ClientKey] = true
	}
	err := saveBlocked(d.cfg.Dir, d.blocked)
	d.mu.Unlock()
	return err
}

// Distrust blocks a cat locally, both directions, never propagated. Undo
// with Trust; the roster entry stays, tagged in `clow cats`.
func (d *Daemon) Distrust(name string) (roster.Cat, error) {
	cat, ok := d.ros.Get(name)
	if !ok {
		return roster.Cat{}, fmt.Errorf("unknown cat %q", name)
	}
	if cat.Key == d.Me().Key {
		return cat, fmt.Errorf("cannot distrust yourself")
	}
	if err := d.blockCat(cat); err != nil {
		return cat, err
	}
	d.cfg.logf("clowder: distrusted %s (local only; roster entry kept)", name)
	return cat, nil
}

func (d *Daemon) Trust(name string) (roster.Cat, error) {
	cat, ok := d.ros.Get(name)
	if !ok {
		return roster.Cat{}, fmt.Errorf("unknown cat %q", name)
	}
	d.mu.Lock()
	delete(d.blocked, cat.Key)
	if cat.ClientKey != "" {
		delete(d.blocked, cat.ClientKey)
	}
	err := saveBlocked(d.cfg.Dir, d.blocked)
	d.mu.Unlock()
	if err != nil {
		return cat, err
	}
	d.cfg.logf("clowder: trusted %s again", name)
	return cat, nil
}

// isBlockedName matches a declared name to the blocklist — relayed offers
// carry the sender's name, not its key.
func (d *Daemon) isBlockedName(name string) bool {
	cat, ok := d.ros.Get(name)
	return ok && d.isBlockedKey(cat.Key)
}

func (d *Daemon) distrustedNames() []string {
	var out []string
	for _, c := range d.ros.All() {
		if d.isBlockedKey(c.Key) {
			out = append(out, c.Name)
		}
	}
	return out
}
