package main

import (
	"os"
	"path/filepath"
	"testing"
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
