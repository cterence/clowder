package daemon

// Blocked cats: keys of forgotten (and, later, distrusted) cats.
// Tailcat's AllowedClients is add-only, so a removed cat's key can
// still connect at the transport layer; the daemon refuses these
// peers at the protocol level instead. Persisted in blocked.json.

import (
	"fmt"
	"path/filepath"

	"clowder/persist"
	"clowder/roster"
)

func blockedPath(dir string) string { return filepath.Join(dir, "blocked.json") }

// loadBlocked reads the persisted blocklist.
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

// saveBlocked persists the blocklist.
func saveBlocked(dir string, m map[string]bool) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return persist.SaveJSON(blockedPath(dir), keys)
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

// Distrust blocks a cat locally, in both directions, without removing
// it from the roster: one cat's own decision, never propagated (unlike
// the planned signed-leave gossip, and unlike forget). Connections
// from it are refused, sends to it refuse, and it is skipped as a
// relay. Undo with Trust. The entry stays visible in `clow cats` with
// a distrusted tag.
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

// Trust undoes Distrust: the cat's keys leave the blocklist and it
// can connect, receive sends and act as a relay again.
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

// isBlockedName reports whether a declared name belongs to a blocked
// cat. Relayed offers carry the sender's name, not its key, so this is
// the best a storer-path refusal can do.
func (d *Daemon) isBlockedName(name string) bool {
	cat, ok := d.ros.Get(name)
	return ok && d.isBlockedKey(cat.Key)
}

// distrustedNames lists the roster cats whose keys are blocked, for
// the `clow cats` and `clow status` tags.
func (d *Daemon) distrustedNames() []string {
	var out []string
	for _, c := range d.ros.All() {
		if d.isBlockedKey(c.Key) {
			out = append(out, c.Name)
		}
	}
	return out
}
