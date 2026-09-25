package main

import (
	"flag"
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

func TestFlagsFirst(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.String("name", "", "")
		fs.Bool("dry", false, "")
		return fs
	}
	tests := []struct {
		in, want []string
	}{
		{[]string{"addr", "--name", "milo"}, []string{"--name", "milo", "addr"}},
		{[]string{"--name", "milo", "addr"}, []string{"--name", "milo", "addr"}},
		{[]string{"--name=milo", "addr"}, []string{"--name=milo", "addr"}},
		{[]string{"a", "b", "-x"}, []string{"-x", "a", "b"}},
		{[]string{"addr", "--dry", "other"}, []string{"--dry", "addr", "other"}},
		{nil, nil},
	}
	for _, tt := range tests {
		got := flagsFirst(newFS(), tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("flagsFirst(%v) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("flagsFirst(%v) = %v, want %v", tt.in, got, tt.want)
				break
			}
		}
	}
}
