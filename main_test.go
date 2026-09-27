package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clowder/daemon"
)

func TestInitCreatesIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLOWDER_DIR", dir)

	if err := run([]string{"init", "--name", "milo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, f := range []string{"identity.json", "me.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}

	// Init must refuse to clobber an existing identity.
	if err := run([]string{"init", "--name", "milo"}); err == nil {
		t.Error("second init succeeded, want error")
	}
}

func TestUnknownCommand(t *testing.T) {
	if err := run([]string{"purr"}); err == nil {
		t.Fatal("unknown command succeeded, want error")
	}
}

func TestStorerUsage(t *testing.T) {
	for _, args := range [][]string{{"storer"}, {"storer", "maybe"}, {"storer", "on", "off"}} {
		if err := run(args); err == nil {
			t.Errorf("storer %v succeeded, want usage error", args)
		}
	}
}

func TestSendUsage(t *testing.T) {
	if err := run([]string{"send", "only-one-arg"}); err == nil {
		t.Error("send with one arg succeeded, want usage error")
	}
}

func TestReset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLOWDER_DIR", dir)

	if err := run([]string{"reset", "--yes"}); err == nil {
		t.Error("reset without identity succeeded, want error")
	}

	if err := run([]string{"init", "--name", "milo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := run([]string{"reset", "--yes"}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); !os.IsNotExist(err) {
		t.Error("identity survived reset")
	}
	// A reset cat can be re-created.
	if err := run([]string{"init", "--name", "milo2"}); err != nil {
		t.Fatalf("re-init after reset: %v", err)
	}
}

func TestInboxSetWithoutDaemon(t *testing.T) {
	// `clow inbox --set` must not need the daemon: persist locally for
	// the next start, instead of failing on the IPC socket.
	dir := t.TempDir()
	t.Setenv("CLOWDER_DIR", dir)
	if err := run([]string{"init", "--name", "milo"}); err != nil {
		t.Fatalf("init: %v", err)
	}

	inbox := filepath.Join(t.TempDir(), "downloads")
	if err := run([]string{"inbox", "--set", inbox}); err != nil {
		t.Fatalf("inbox --set without daemon: %v", err)
	}
	if got := daemon.InboxDir(dir); got != inbox {
		t.Fatalf("persisted inbox = %q, want %q", got, inbox)
	}
	if _, err := os.Stat(inbox); err != nil {
		t.Fatalf("inbox dir not created: %v", err)
	}
}

// TestRemovedCommands pins the fetch and cats removals: fetch's job is
// done by the storer push sweep, and cats folded into `status
// --addresses`.
func TestRemovedCommands(t *testing.T) {
	for _, cmd := range []string{"fetch", "cats"} {
		err := run([]string{cmd})
		if err == nil {
			t.Errorf("%s succeeded, want unknown-command error", cmd)
			continue
		}
		if !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%s: %v, want unknown-command error", cmd, err)
		}
	}
}

// TestStorerFlagParses pins the flags-first order (`clow storer
// --max 10G on`): without a daemon the command must fail on the IPC
// dial, not on flag parsing or the usage check.
func TestStorerFlagParses(t *testing.T) {
	t.Setenv("CLOWDER_DIR", t.TempDir())
	err := run([]string{"storer", "--max", "10G", "on"})
	if err == nil {
		t.Fatal("storer succeeded without a daemon, want IPC error")
	}
	if strings.Contains(err.Error(), "flag provided but not defined") || strings.Contains(err.Error(), "usage:") {
		t.Fatalf("storer --max 10G on no longer parses: %v", err)
	}
}

// TestStatusAddressesFlagParses proves --addresses gets past flag
// parsing: without a daemon the command must fail on the IPC dial,
// not on the flag set.
func TestStatusAddressesFlagParses(t *testing.T) {
	t.Setenv("CLOWDER_DIR", t.TempDir())
	err := run([]string{"status", "--addresses"})
	if err == nil {
		t.Fatal("status succeeded without a daemon, want IPC error")
	}
	if strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("--addresses no longer parses: %v", err)
	}
}
