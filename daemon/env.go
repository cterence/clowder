package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"

	"clowder/persist"
)

// Me is the local cat's declared state: its name, whether it volunteers
// as a storer, and where received files land. It persists in me.json next
// to the identity.
type Me struct {
	Name   string `json:"name"`
	Storer bool   `json:"storer"`
	// Dropbox marks a storer that only serves third parties: no
	// deliveries to itself, no originating sends. Implies Storer.
	Dropbox bool `json:"dropbox,omitempty"`
	// Capacity is the storer's spool budget in bytes. Enabling the
	// storer role requires one; deposits that would exceed it are
	// refused.
	Capacity int64 `json:"capacity,omitempty"`
	// Inbox is the absolute directory received files land in. Empty
	// means the default (see DefaultInbox).
	Inbox string `json:"inbox,omitempty"`
}

// ParseSize parses a human byte size like "10G", "500MiB" or "1024"
// (binary units), for storer capacity flags.
func ParseSize(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	t = strings.TrimSuffix(t, "ib")
	for _, suf := range []struct {
		s string
		m int64
	}{
		{"t", 1 << 40},
		{"g", 1 << 30},
		{"m", 1 << 20},
		{"k", 1 << 10},
	} {
		if strings.HasSuffix(t, suf.s) {
			mult = suf.m
			t = t[:len(t)-1]
			break
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("daemon: invalid size %q", s)
	}
	return n * mult, nil
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
	// ClientIdentity is the keypair used for all outbound dials. It must
	// differ from Identity: a cat runs a tailcat server and tailcat
	// clients concurrently, and two engines sharing one static key with
	// different per-side pre-shared keys cross-deliver handshakes and
	// wedge. Peers allowlist this key to authenticate our dials.
	ClientIdentity key.NodePrivate
	Me             Me
}

// clientKeyPath is where the client identity lives.
func clientKeyPath(dir string) string { return filepath.Join(dir, "clientkey.json") }

// saveIdentity writes the identity (node key, pre-shared key, region
// hint) atomically.
func saveIdentity(dir string, k *tailcat.PrivateKey) error {
	return persist.SaveJSON(filepath.Join(dir, "identity.json"), k)
}

// UpdatePresharedKey persists a new pre-shared key into the identity,
// changing the cat's tailcat address from the next daemon start. The
// running daemon keeps serving under the old address until restarted.
func UpdatePresharedKey(dir string, psk tailcat.PresharedKey) error {
	var k *tailcat.PrivateKey
	if _, err := persist.LoadJSON(filepath.Join(dir, "identity.json"), &k); err != nil {
		return fmt.Errorf("daemon: reading identity: %w", err)
	}
	if k == nil {
		return errors.New("daemon: no identity to rotate")
	}
	k.Public.PresharedKey = psk
	return saveIdentity(dir, k)
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
	if err := saveIdentity(dir, k); err != nil {
		return err
	}
	if err := writeClientKey(dir, key.NewNode()); err != nil {
		return err
	}
	if err := saveMe(dir, Me{Name: name}); err != nil {
		return err
	}
	return nil
}

// Open loads the identity and declared state from a config dir created by
// Init.
func Open(dir string) (*Env, error) {
	var k *tailcat.PrivateKey
	if _, err := persist.LoadJSON(filepath.Join(dir, "identity.json"), &k); err != nil {
		return nil, fmt.Errorf("daemon: opening %s: %w", dir, err)
	}
	if k == nil {
		return nil, fmt.Errorf("daemon: %s has no identity (run: clow init)", dir)
	}
	me, err := loadMe(dir)
	if err != nil {
		return nil, err
	}
	ck, err := loadClientKey(dir)
	if err != nil {
		return nil, err
	}
	return &Env{Dir: dir, Identity: k, ClientIdentity: ck, Me: me}, nil
}

// writeClientKey persists an outbound client identity.
func writeClientKey(dir string, priv key.NodePrivate) error {
	text, err := priv.MarshalText()
	if err != nil {
		return fmt.Errorf("daemon: encoding client key: %w", err)
	}
	return persist.SaveJSON(clientKeyPath(dir), string(text))
}

// loadClientKey loads the outbound client identity, generating one for
// identities created before client keys existed.
func loadClientKey(dir string) (key.NodePrivate, error) {
	var text string
	ok, err := persist.LoadJSON(clientKeyPath(dir), &text)
	if err != nil {
		return key.NodePrivate{}, fmt.Errorf("daemon: parsing client key: %w", err)
	}
	if !ok {
		priv := key.NewNode()
		if err := writeClientKey(dir, priv); err != nil {
			return key.NodePrivate{}, err
		}
		return priv, nil
	}
	var priv key.NodePrivate
	if err := priv.UnmarshalText([]byte(text)); err != nil {
		return key.NodePrivate{}, fmt.Errorf("daemon: client key: %w", err)
	}
	return priv, nil
}

func saveMe(dir string, me Me) error {
	return persist.SaveJSON(filepath.Join(dir, "me.json"), me)
}

func loadMe(dir string) (Me, error) {
	var me Me
	if _, err := persist.LoadJSON(filepath.Join(dir, "me.json"), &me); err != nil {
		return Me{}, err
	}
	return me, nil
}
