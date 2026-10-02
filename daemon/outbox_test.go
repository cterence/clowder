package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A Put racing a Delete must never resurrect the entry: delivery
// retries re-Put their digest cache, and a check-then-write Put can
// straddle a concurrent Delete — CI caught it as a phantom queued
// send surviving `clow cancel`.
func TestPutRacingDeleteDoesNotResurrect(t *testing.T) {
	o := newOutbox(filepath.Join(t.TempDir(), "outbox"))
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("id-%d", i)
		if err := o.Put(Entry{ID: id, FileName: "nap.txt"}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = o.Put(Entry{ID: id, FileName: "nap.txt"})
		}()
		go func() {
			defer wg.Done()
			_ = o.Delete(id)
		}()
		wg.Wait()
		if _, err := os.Stat(filepath.Join(o.dir, id+".json")); !os.IsNotExist(err) {
			t.Fatalf("round %d: entry %s resurrected after Delete", i, id)
		}
	}
}
