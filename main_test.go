package main

import (
	"encoding/json"
	"net"
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

// Reset is a pure local wipe: while the daemon owns the dir it refuses
// outright — departure is `clow leave`, never a side effect of reset.
func TestResetRefusesWhileDaemonRuns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLOWDER_DIR", dir)
	if err := run([]string{"init", "--name", "milo"}); err != nil {
		t.Fatal(err)
	}

	gotReq := make(chan daemon.Request, 1)
	ln, err := net.Listen("unix", daemon.IPCPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req daemon.Request
			_ = json.NewDecoder(conn).Decode(&req)
			if req.Op != "" {
				gotReq <- req
			}
			_ = json.NewEncoder(conn).Encode(daemon.Response{OK: true})
			_ = conn.Close()
		}
	}()

	err = run([]string{"reset", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "stop it before resetting") {
		t.Fatalf("reset with a running daemon: err = %v, want a stop-the-daemon refusal", err)
	}
	select {
	case req := <-gotReq:
		t.Fatalf("reset sent op %q; departure must be `clow leave`, not a reset side effect", req.Op)
	default:
	}

	// The identity survives: the wipe only happens once the daemon is gone.
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err != nil {
		t.Fatalf("reset wiped the cat while its daemon runs: %v", err)
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
	for _, cmd := range []string{"fetch", "cats", "help"} {
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

// The global usage carries no per-topic footers; each command's -h
// prints its own usage, description and the hints that concern it.
func TestCommandHelp(t *testing.T) {
	u := usageText()
	for _, hint := range []string{"config dir:", "inbox:", "pairing:", "daemon:"} {
		if strings.Contains(u, hint) {
			t.Errorf("global usage still carries the %q footer", hint)
		}
	}
	for _, c := range commandDocs {
		h := helpText(c.name)
		if !strings.Contains(h, "usage: "+c.usage) {
			t.Errorf("help(%s) lacks its usage line", c.name)
		}
		for _, want := range c.hints {
			if !strings.Contains(h, want) {
				t.Errorf("help(%s) lacks hint %q", c.name, want)
			}
		}
	}
	// An unknown command's help falls back to the global usage.
	if helpText("purr") != u {
		t.Error("help(unknown) does not fall back to the global usage")
	}
}
