package daemon

import (
	"os"
	"testing"
	"time"
)

// TestSlowTransferNotDuplicated reproduces the real-world double-send
// bug: a transfer slower than the retry ticker must not be started a
// second time while still streaming. Before the in-flight claims, the
// ticker spawned a duplicate deliver() every tick, and the receiver
// restarted the file from zero (a new .recv temp) for each attempt.
func TestSlowTransferNotDuplicated(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := Init(dirA, "milo"); err != nil {
		t.Fatal(err)
	}
	if err := Init(dirB, "fluff"); err != nil {
		t.Fatal(err)
	}
	// Throttled writes stretch a 2 MiB transfer well past several retry
	// ticks (150 ms in tests).
	sender := runDaemon(t, dirA, &slowTransport{perWrite: 8 * time.Millisecond})
	receiver := runDaemon(t, dirB, &LocalTransport{})
	trust(t, sender, receiver)
	trust(t, receiver, sender)

	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}
	src := writeSource(t, string(big))
	if _, err := sender.Send("fluff", src); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		got, ok := inboxFile(t, receiver, "nap.txt")
		return ok && len(got) == len(big)
	}, "the throttled file to arrive")
	waitFor(t, func() bool { return len(sender.ob.All()) == 0 }, "the outbox to drain")

	// Exactly one completed transfer in each direction's counters:
	// duplicates would have incremented them.
	if st := sender.stats.snapshot(); st.Sent != 1 {
		t.Fatalf("sender stats show %d completed sends, want 1", st.Sent)
	}
	if st := receiver.stats.snapshot(); st.Received != 1 {
		t.Fatalf("receiver stats show %d completed receives, want 1", st.Received)
	}

	// And exactly one file in the inbox: duplicates would have left
	// nap-1.txt (or stalled .recv-* temps).
	des, err := os.ReadDir(receiver.InboxDir())
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, de := range des {
		files = append(files, de.Name())
	}
	if len(files) != 1 || files[0] != "nap.txt" {
		t.Fatalf("inbox contains %v, want exactly [nap.txt]", files)
	}
}
