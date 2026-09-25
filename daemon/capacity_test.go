package daemon

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"1024", 1024},
		{"1k", 1 << 10},
		{"10G", 10 << 30},
		{"500MiB", 500 << 20},
		{"2t", 2 << 40},
		{"1M", 1 << 20},
	}
	for _, tt := range tests {
		got, err := ParseSize(tt.in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
	for _, bad := range []string{"", "abc", "-5G", "12x"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) succeeded, want error", bad)
		}
	}
}

func TestTryReserveAtomicFirstCome(t *testing.T) {
	d := startDaemon(t, "milo")
	const capacity = 100
	const each = 10

	var wg sync.WaitGroup
	var accepted atomic.Int64
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.tryReserve(capacity, each) {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != capacity/each {
		t.Fatalf("%d concurrent deposits accepted, want exactly %d: the reservation must be first-come-first-served and never oversubscribe",
			accepted.Load(), capacity/each)
	}

	// Releasing gives the capacity back.
	d.releaseReserve(accepted.Load() * each)
	if !d.tryReserve(capacity, capacity) {
		t.Fatal("full capacity must be reservable after release")
	}
}

func TestStorerRequiresCapacity(t *testing.T) {
	a := startDaemon(t, "milo")
	if err := a.SetStorer(true, 0); err == nil {
		t.Fatal("enabling storer without capacity succeeded, want error")
	}
	if err := a.SetDropbox(true, 0); err == nil {
		t.Fatal("enabling dropbox without capacity succeeded, want error")
	}
	if a.Me().Storer {
		t.Fatal("failed enable left the storer role on")
	}
}

func TestStorerCapacityRefusesAndRecovers(t *testing.T) {
	milo := startDaemon(t, "milo")
	storer := startDaemon(t, "box")
	niko, nikoDir := offlineCat(t)

	const cap = int64(4096)
	if err := storer.SetStorer(true, cap); err != nil {
		t.Fatal(err)
	}
	trust(t, milo, storer)
	trust(t, storer, milo)
	addCat(t, milo, niko)
	addCat(t, storer, niko)

	// A deposit bigger than the whole capacity is refused; the send
	// stays queued on the sender.
	big := string(make([]byte, cap*2))
	src := writeSource(t, big)
	if _, err := milo.Send("niko", src); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, e := range milo.ob.All() {
			if e.TargetKey == niko.Key {
				return true
			}
		}
		return false
	}, "the oversized send to stay queued")
	if storer.Spool().Count() != 0 {
		t.Fatalf("oversized deposit spooled (%d entries)", storer.Spool().Count())
	}

	// A small deposit fits.
	small := writeSource(t, string(make([]byte, 1024)))
	if _, err := milo.Send("niko", small); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return storer.Spool().Usage() > 1024 }, "the small deposit to be held")
	if got := storer.Spool().Usage(); got > cap {
		t.Fatalf("spool usage %d exceeds capacity %d", got, cap)
	}

	// A second small deposit would overflow: refused too.
	small2 := writeSource(t, string(make([]byte, 4096)))
	if _, err := milo.Send("niko", small2); err != nil {
		t.Fatal(err)
	}
	// Give the delivery attempt a moment, then check it did not land.
	waitFor(t, func() bool {
		for _, e := range milo.ob.All() {
			if e.SourcePath == small2 {
				return true
			}
		}
		return false
	}, "the overflowing send to stay queued")

	// Delivering what is held frees capacity; the queued overflow then
	// fits on the next retry.
	nikoD := startDaemonAt(t, nikoDir)
	trust(t, nikoD, storer)
	trust(t, storer, nikoD)
	waitFor(t, func() bool { return storer.Spool().Usage() == 0 }, "the held deposit to drain")
	waitFor(t, func() bool {
		for _, e := range milo.ob.All() {
			if e.SourcePath == small2 {
				return false
			}
		}
		return true
	}, "the previously refused send to land after capacity freed")
}
