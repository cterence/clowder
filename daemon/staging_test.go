package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeStaged plants a clipboard-style source in the daemon's staging
// dir, where the CLI parks `clow send --clipboard` copies.
func writeStaged(t *testing.T, d *Daemon, content string) string {
	t.Helper()
	dir := filepath.Join(d.cfg.Dir, "staging")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "clipboard-test.txt")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestStagedSourceSurvivesStorerHold pins the scenario that bit the
// live mesh: a staged clipboard send held by a storer must keep its
// source — the hold probe re-queues the send once the storer pushes
// the file, and that re-delivery re-reads the source — until the
// recipient's signed receipt clears the entry and sweeps the copy.
func TestStagedSourceSurvivesStorerHold(t *testing.T) {
	milo := startDaemon(t, "milo", func(c *Config) { c.PollEvery = time.Hour })
	storer := startDaemon(t, "storer", func(c *Config) { c.SpoolSweepEvery = time.Hour })
	if err := storer.SetStorer(true, 1<<30); err != nil {
		t.Fatal(err)
	}
	trust(t, milo, storer)
	trust(t, storer, milo)
	niko, nikoDir := offlineCat(t)
	addCat(t, milo, niko)
	addCat(t, storer, niko)

	staged := writeStaged(t, milo, "nap for a sleeping cat")
	if _, err := milo.Send("niko", staged); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, func() bool {
		es := milo.ob.All()
		return len(es) == 1 && es[0].HeldBy != ""
	}, "storer to hold the file")
	milo.probeHeld(context.Background())
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged source vanished while the storer holds it: %v", err)
	}

	// The target wakes; the storer pushes, and niko's receipt clears
	// the entry — which is when the copy may finally go.
	nikoD := startDaemonAt(t, nikoDir)
	addCat(t, nikoD, milo.Me())
	trust(t, nikoD, storer)
	trust(t, storer, nikoD)
	storer.sweepSpoolFor(context.Background(), niko.Key)
	waitFor(t, func() bool { return len(milo.ob.All()) == 0 }, "the signed receipt to clear the outbox")
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged source survived the receipt that cleared its entry: %v", err)
	}
}

// TestStagedSourceDiesWithOutbox pins the other entry deaths: a
// staged copy is swept on delivery and on cancel, and a file the user
// picked is never the daemon's to delete.
func TestStagedSourceDiesWithOutbox(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	staged := writeStaged(t, milo, "hello from the clipboard")
	if _, err := milo.Send("fluff", staged); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, func() bool { return len(milo.ob.All()) == 0 }, "delivery to clear the outbox")
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged source survived delivery: %v", err)
	}

	plain := writeSource(t, "a file the user picked")
	if _, err := milo.Send("fluff", plain); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, func() bool { return len(milo.ob.All()) == 0 }, "delivery to clear the outbox")
	if _, err := os.Stat(plain); err != nil {
		t.Fatalf("user's source deleted by delivery: %v", err)
	}

	staged2 := writeStaged(t, milo, "changed my mind")
	niko, _ := offlineCat(t)
	addCat(t, milo, niko)
	if _, err := milo.Send("niko", staged2); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := milo.Cancel(milo.ob.All()[0].ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := os.Stat(staged2); !os.IsNotExist(err) {
		t.Fatalf("staged source survived cancel: %v", err)
	}
}
