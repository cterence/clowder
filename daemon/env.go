package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tailscale/tailcat"
)

// Me is the local cat's declared state: its name, whether it volunteers
// as a storer, and where received files land. It persists in me.json next
// to the identity.
type Me struct {
	Name   string `json:"name"`
	Storer bool   `json:"storer"`
	// Inbox is the absolute directory received files land in. Empty
	// means the default (see DefaultInbox).
	Inbox string `json:"inbox,omitempty"`
}

// DefaultInbox is where received files land when no inbox dir is set: a
// clowder folder in the OS's downloads directory (see downloadDir),
// kept distinct from the config dir. It returns "" when no suitable
// directory is known.
func DefaultInbox() string {
	d := downloadDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "clowder")
}

// InboxDir resolves the inbox directory for a config dir: the me.json
// setting if set, else the default, falling back to <dir>/inbox when
// there is no home directory.
func InboxDir(dir string) string {
	me, err := loadMe(dir)
	if err == nil && me.Inbox != "" {
		return me.Inbox
	}
	if d := DefaultInbox(); d != "" {
		return d
	}
	return filepath.Join(dir, "inbox")
}

// SetInboxAt persists the inbox dir in a config dir's me.json.
func SetInboxAt(dir, inbox string) error {
	me, err := loadMe(dir)
	if err != nil {
		return err
	}
	me.Inbox = inbox
	return saveMe(dir, me)
}

// Env holds the persistent state loaded from a config dir.
type Env struct {
	Dir      string
	Identity *tailcat.PrivateKey
	Me       Me
}

// Init creates a new cat identity in dir: a node keypair with a WireGuard
// pre-shared key (RegionID -1 picks the DERP region automatically at
// startup), plus the cat's declared name. The directory must not already
// contain an identity.
func Init(dir, name string) error {
	if name == "" {
		return errors.New("daemon: cat name is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("daemon: creating %s: %w", dir, err)
	}
	idPath := filepath.Join(dir, "identity.json")
	if _, err := os.Stat(idPath); err == nil {
		return fmt.Errorf("daemon: %s already initialized", dir)
	}

	k := tailcat.NewPrivateKey()
	k.Public.RegionID = -1 // auto-select DERP region at startup
	b, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return fmt.Errorf("daemon: encoding identity: %w", err)
	}
	if err := os.WriteFile(idPath, b, 0o600); err != nil {
		return fmt.Errorf("daemon: writing identity: %w", err)
	}
	if err := saveMe(dir, Me{Name: name}); err != nil {
		return err
	}
	return nil
}

// Open loads the identity and declared state from a config dir created by
// Init.
func Open(dir string) (*Env, error) {
	idPath := filepath.Join(dir, "identity.json")
	b, err := os.ReadFile(idPath)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("daemon: %s has no identity (run: clow init)", dir)
	}
	if err != nil {
		return nil, fmt.Errorf("daemon: reading identity: %w", err)
	}
	var k *tailcat.PrivateKey
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, fmt.Errorf("daemon: parsing %s: %w", idPath, err)
	}
	me, err := loadMe(dir)
	if err != nil {
		return nil, err
	}
	return &Env{Dir: dir, Identity: k, Me: me}, nil
}

func saveMe(dir string, me Me) error {
	b, err := json.MarshalIndent(me, "", "  ")
	if err != nil {
		return fmt.Errorf("daemon: encoding me: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "me.json"), b, 0o600); err != nil {
		return fmt.Errorf("daemon: writing me: %w", err)
	}
	return nil
}

func loadMe(dir string) (Me, error) {
	b, err := os.ReadFile(filepath.Join(dir, "me.json"))
	if os.IsNotExist(err) {
		return Me{}, nil
	}
	if err != nil {
		return Me{}, fmt.Errorf("daemon: reading me: %w", err)
	}
	var me Me
	if err := json.Unmarshal(b, &me); err != nil {
		return Me{}, fmt.Errorf("daemon: parsing me.json: %w", err)
	}
	return me, nil
}
