package daemon

// Loopback load benchmark for the pprof pass: one sender streaming
// 1 MiB files to one receiver over the loopback transport, end to end
// (outbox → deliver → seal → stream → unseal → inbox). Profile with:
//
//	go test ./daemon/ -bench LoopbackSend -benchmem -count=1 \
//	  -cpuprofile cpu.prof -memprofile mem.prof
//
// The Logf is silent: under -benchtime 30x the transfer path should
// dominate the profile, not log formatting.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// benchDaemon starts a loopback daemon for benchmarks, with the same
// inbox sandboxing the test harness applies.
func benchDaemon(b *testing.B, name string) *Daemon {
	b.Helper()
	b.Setenv("HOME", b.TempDir())
	dir := b.TempDir()
	if err := Init(dir, name); err != nil {
		b.Fatalf("Init: %v", err)
	}
	d, err := New(Config{
		Dir:        dir,
		RetryEvery: 150 * time.Millisecond,
		PollEvery:  150 * time.Millisecond,
		Logf:       b.Logf,
	}, &LocalTransport{})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)
	go func() { _ = d.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for d.Me().Addr == "" {
		if time.Now().After(deadline) {
			b.Fatalf("%s never listened", name)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return d
}

// BenchmarkLoopbackSend1MiB streams b.N one-mebibyte files from one
// cat to another, waiting for each to land in the inbox, so every
// iteration is one complete seal/stream/unseal round trip.
func BenchmarkLoopbackSend1MiB(b *testing.B) {
	sender := benchDaemon(b, "sender")
	receiver := benchDaemon(b, "receiver")
	trustB(b, sender, receiver)
	trustB(b, receiver, sender)

	src := filepath.Join(b.TempDir(), "nap.bin")
	buf := make([]byte, 1024*1024)
	for i := range buf {
		buf[i] = byte(i)
	}
	if err := os.WriteFile(src, buf, 0o600); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sender.Send("receiver", src); err != nil {
			b.Fatalf("send: %v", err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for !arrived(receiver.InboxDir(), i+1) {
			if time.Now().After(deadline) {
				b.Fatalf("file %d never arrived", i+1)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// arrived reports whether the inbox holds at least n delivered files,
// counting only completed ones: in-flight writes are visible as
// persist's .tmp-* temp entries and must not pass as delivered, or the
// benchmark declares victory mid-receive and tears the inbox down
// under the still-running save.
func arrived(dir string, n int) bool {
	des, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	real := 0
	for _, de := range des {
		if !strings.HasPrefix(de.Name(), ".tmp-") {
			real++
		}
	}
	return real >= n
}

func trustB(b *testing.B, d *Daemon, other *Daemon) {
	b.Helper()
	c := other.Me()
	if err := d.Roster().Add(c); err != nil {
		b.Fatalf("adding peer: %v", err)
	}
	d.allowCat(c)
}
