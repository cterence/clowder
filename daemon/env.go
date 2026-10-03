package daemon

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"

	"github.com/cterence/clowder/persist"
)

// Me is the local cat's declared state, persisted in me.json.
type Me struct {
	Name   string `json:"name"`
	Storer bool   `json:"storer"`
	// Storer that only serves third parties. Implies Storer.
	Dropbox bool `json:"dropbox,omitempty"`
	// Spool budget in bytes; enabling the storer role requires one.
	Capacity int64 `json:"capacity,omitempty"`
	// Absolute inbox dir; empty means the default.
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
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("daemon: size %q overflows", s)
	}
	return n * mult, nil
}

// DefaultInbox is the clowder folder in the OS's downloads directory, or
// "" when no suitable directory is known.
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

// SetInboxAt persists the inbox dir in a config dir's me.json and
// creates the directory.
func SetInboxAt(dir, inbox string) error {
	me, err := loadMe(dir)
	if err != nil {
		return err
	}
	me.Inbox = inbox
	if err := saveMe(dir, me); err != nil {
		return err
	}
	return os.MkdirAll(inbox, 0o700)
}

// Env holds the persistent state loaded from a config dir.
type Env struct {
	Dir      string
	Identity *tailcat.PrivateKey
	// Outbound-dial keypair. Must differ from Identity: two engines sharing one
	// static key cross-deliver handshakes and wedge. Peers allowlist it.
	ClientIdentity key.NodePrivate
	// Ed25519 keypair derived from the node key seed (not stored; the identity
	// regenerates it). Signs the cat's roster entries and its leave.
	SignPriv ed25519.PrivateKey
	Me       Me
}

// clientKeyPath is where the client identity lives.
func clientKeyPath(dir string) string { return filepath.Join(dir, "clientkey.json") }

// saveIdentity writes the identity (node key, pre-shared key, region
// hint) atomically.
func saveIdentity(dir string, k *tailcat.PrivateKey) error {
	return persist.SaveJSON(filepath.Join(dir, "identity.json"), k)
}

// Init creates a new identity in dir (RegionID -1 auto-picks the DERP
// region at startup) with the given name. Refuses if dir has an identity.
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
	raw := k.Private.Raw32()
	signPriv := ed25519.NewKeyFromSeed(raw[:])
	return &Env{Dir: dir, Identity: k, ClientIdentity: ck, SignPriv: signPriv, Me: me}, nil
}

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
