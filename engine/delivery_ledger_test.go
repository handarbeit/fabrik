package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeliveryLedger_AcceptIsCheckAndRecord(t *testing.T) {
	l := newDeliveryLedger("", time.Now(), nil)
	now := time.Now()
	if !l.Accept("a", now) {
		t.Fatal("first delivery must be accepted")
	}
	if l.Accept("a", now) {
		t.Fatal("a repeat inside the window must be a duplicate")
	}
	if !l.Accept("b", now) {
		t.Fatal("a different ID must be accepted")
	}
}

// FR-003: no ID, nothing to dedupe on — apply, never drop, never record.
func TestDeliveryLedger_EmptyIDIsNeverADuplicate(t *testing.T) {
	l := newDeliveryLedger("", time.Now(), nil)
	for i := 0; i < 3; i++ {
		if !l.Accept("", time.Now()) {
			t.Fatal("an empty ID must always be accepted")
		}
	}
	if l.Len() != 0 {
		t.Fatalf("empty IDs were recorded: %d", l.Len())
	}
}

// The window is bounded: an ID older than the window is applied again.
func TestDeliveryLedger_WindowExpiry(t *testing.T) {
	l := newDeliveryLedger("", time.Now(), nil)
	l.window = time.Hour
	t0 := time.Now()
	l.Accept("a", t0)
	if l.Accept("a", t0.Add(59*time.Minute)) {
		t.Fatal("inside the window must be a duplicate")
	}
	if !l.Accept("a", t0.Add(61*time.Minute)) {
		t.Fatal("outside the window must be applied again")
	}
}

func TestDeliveryLedger_SizeCapEvictsOldestFirst(t *testing.T) {
	l := newDeliveryLedger("", time.Now(), nil)
	l.max = 3
	now := time.Now()
	for i := 0; i < 5; i++ {
		l.Accept(fmt.Sprintf("d%d", i), now.Add(time.Duration(i)*time.Second))
	}
	if l.Len() != 3 {
		t.Fatalf("Len = %d, want 3", l.Len())
	}
	at := now.Add(10 * time.Second)
	if !l.Accept("d0", at) || !l.Accept("d1", at) {
		t.Fatal("the oldest entries should have been evicted")
	}
	if l.Accept("d4", at) {
		t.Fatal("the newest entry must still be remembered")
	}
}

func TestDeliveryLedger_ForgetLetsARedeliveryThrough(t *testing.T) {
	l := newDeliveryLedger("", time.Now(), nil)
	l.Accept("a", time.Now())
	l.Forget("a")
	if !l.Accept("a", time.Now()) {
		t.Fatal("a forgotten ID must be accepted again")
	}
	l.Forget("never-seen") // no-op
}

// SC-001: one ID carried by two transports at once is applied exactly once.
func TestDeliveryLedger_ConcurrentAcceptOfOneIDWinsOnce(t *testing.T) {
	l := newDeliveryLedger("", time.Now(), nil)
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if l.Accept("same", time.Now()) {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d goroutines won the same delivery ID, want exactly 1", wins.Load())
	}
}

// A delivery applied before a restart is not applied again after it.
func TestDeliveryLedger_PersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "webhook", "deliveries.json")
	now := time.Now()
	first := newDeliveryLedger(path, now, nil)
	first.Accept("kept", now)
	first.Accept("kept2", now)
	first.Close() // the flush a restart performs

	second := newDeliveryLedger(path, now.Add(time.Minute), nil)
	if second.Len() != 2 {
		t.Fatalf("reloaded %d entries, want 2", second.Len())
	}
	if second.Accept("kept", now.Add(time.Minute)) {
		t.Fatal("a delivery applied before the restart was accepted again")
	}
	if !second.Accept("fresh", now.Add(time.Minute)) {
		t.Fatal("a new delivery must still be accepted")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat ledger file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger file mode = %v; want 0600", info.Mode().Perm())
	}
}

func TestDeliveryLedger_ReloadDropsExpiredEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	now := time.Now()
	first := newDeliveryLedger(path, now, nil)
	first.Accept("old", now)
	first.Close()

	later := newDeliveryLedger(path, now.Add(deliveryLedgerWindow+time.Hour), nil)
	if later.Len() != 0 {
		t.Fatalf("an entry older than the window survived a reload: %d", later.Len())
	}
}

func TestDeliveryLedger_MissingFileStartsEmpty(t *testing.T) {
	l := newDeliveryLedger(filepath.Join(t.TempDir(), "nope", "d.json"), time.Now(), nil)
	if l.Len() != 0 || !l.Accept("a", time.Now()) {
		t.Fatal("a missing file must start an empty window")
	}
}

// The persisted record missing or corrupt starts an empty window rather than
// failing, and the corrupt file is moved aside.
func TestDeliveryLedger_CorruptFileStartsEmptyAndIsQuarantined(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":       "{not json",
		"wrong version": `{"version":99,"entries":[{"id":"a","at":"2026-01-01T00:00:00Z"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "d.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			var logged []string
			l := newDeliveryLedger(path, time.Now(), func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
			if l.Len() != 0 || !l.Accept("a", time.Now()) {
				t.Fatal("a corrupt file must start an empty window")
			}
			if _, err := os.Stat(path + ".corrupt"); err != nil {
				t.Fatalf("corrupt file not quarantined: %v", err)
			}
			if len(logged) == 0 || !strings.Contains(logged[0], "empty delivery window") {
				t.Fatalf("no operator log line: %v", logged)
			}
		})
	}
}

func TestDeliveryLedger_FlushIsCoalescedAndSkippedWhenClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	l := newDeliveryLedger(path, time.Now(), nil)
	l.Flush() // nothing changed: no file
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a clean ledger wrote a file")
	}
	l.Accept("a", time.Now())
	l.Flush()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("dirty ledger did not flush: %v", err)
	}
}

func TestDeliveryLedger_NilIsInert(t *testing.T) {
	var l *deliveryLedger
	if !l.Accept("a", time.Now()) {
		t.Fatal("a nil ledger must accept everything")
	}
	l.Forget("a")
	l.Flush()
	l.Close()
	if l.Len() != 0 {
		t.Fatal("nil ledger has entries")
	}
}

// A flush that took its snapshot first but writes last must not clobber a newer
// flush: Flush serialises snapshot+write, so the second waits for the first.
func TestDeliveryLedger_ConcurrentFlushesNeverLeaveAStaleSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deliveries.json")
	now := time.Now()
	l := newDeliveryLedger(path, now, nil)

	inWrite := make(chan struct{})
	release := make(chan struct{})
	var parked atomic.Bool
	l.writeFn = func(p string, data []byte, perm os.FileMode) error {
		if parked.CompareAndSwap(false, true) {
			close(inWrite)
			<-release // hold the first writer after its snapshot was taken
		}
		return atomicWriteFile(p, data, perm)
	}

	l.Accept("one", now)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); l.Flush() }()
	<-inWrite // flush A holds snapshot {one} and is parked inside its write
	l.Accept("two", now)
	go func() { defer wg.Done(); l.Flush() }() // flush B snapshots {one,two}
	time.Sleep(100 * time.Millisecond)         // unserialised, B would finish here and A then overwrite it
	close(release)
	wg.Wait()

	reloaded := newDeliveryLedger(path, now, nil)
	if reloaded.Len() != 2 || reloaded.Accept("two", now) {
		t.Fatalf("disk holds %d entries and lost %q: an older snapshot overwrote a newer one", reloaded.Len(), "two")
	}
}

// #2144: Seen is a pure lookup — it must record nothing, or a replayed
// delivery would be dropped as a duplicate of the lookup that preceded it.
func TestDeliveryLedger_SeenIsReadOnly(t *testing.T) {
	var nilLedger *deliveryLedger
	if nilLedger.Seen("a") || !nilLedger.Newest().IsZero() {
		t.Fatal("nil ledger must see nothing")
	}
	l := newDeliveryLedger("", time.Now(), nil)
	now := time.Now()
	if l.Seen("a") {
		t.Fatal("empty ledger must not have seen a")
	}
	if l.Len() != 0 || l.dirty || l.timer != nil {
		t.Fatalf("Seen on an unknown ID changed state: len=%d dirty=%v", l.Len(), l.dirty)
	}
	if !l.Accept("a", now) {
		t.Fatal("Accept after Seen must still accept (Seen recorded the ID)")
	}
	if !l.Seen("a") || l.Seen("b") || l.Seen("") {
		t.Fatal("Seen must report exactly the accepted IDs")
	}
	if l.Accept("a", now) {
		t.Fatal("Accept must still dedupe after a Seen")
	}
}

func TestDeliveryLedger_Newest(t *testing.T) {
	l := newDeliveryLedger("", time.Now(), nil)
	if !l.Newest().IsZero() {
		t.Fatal("empty ledger must report a zero newest time")
	}
	t0 := time.Now().Add(-time.Hour)
	l.Accept("a", t0)
	l.Accept("b", t0.Add(10*time.Minute))
	if got := l.Newest(); !got.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("Newest = %v", got)
	}
	var in *intake
	if in.ledgerSeen("a") || !in.ledgerNewest().IsZero() {
		t.Fatal("nil intake accessors must be safe")
	}
}
