package daemon

import (
	"testing"
)

func TestStatsCountTransfers(t *testing.T) {
	milo := startDaemon(t, "milo")
	fluff := startDaemon(t, "fluff")
	trust(t, milo, fluff)
	trust(t, fluff, milo)

	src := writeSource(t, "counted nap data")
	if _, err := milo.Send("fluff", src); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, ok := inboxFile(t, fluff, "nap.txt")
		return ok
	}, "fluff to receive the file")

	sent := milo.stats.snapshot()
	if sent.Sent != 1 || sent.SentBytes != int64(len("counted nap data")) {
		t.Fatalf("sender stats = %+v", sent)
	}
	got := fluff.stats.snapshot()
	if got.Received != 1 || got.ReceivedBytes != int64(len("counted nap data")) {
		t.Fatalf("receiver stats = %+v", got)
	}

	// Stats persist across a restart of the same config dir.
	if st := loadStats(milo.cfg.Dir); st.Sent != 1 {
		t.Fatalf("persisted stats = %+v", st)
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{5 << 20, "5.0 MiB"},
		{3 << 30, "3.0 GiB"},
		{2 << 40, "2.0 TiB"},
	}
	for _, tt := range tests {
		if got := HumanBytes(tt.n); got != tt.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
