package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// deliveryLedger is the engine-wide, persisted record of recently accepted
// webhook deliveries (#2351, ADR 2351). It is the single dedupe point in front
// of delivery application, shared by every transport, so one X-GitHub-Delivery
// ID is applied once no matter how many sources carry it, how often a source
// redelivers it, or whether the engine restarted in between.
//
// The record is bounded by time (deliveryLedgerWindow) and by size
// (deliveryLedgerMaxEntries, oldest first). It is written when a delivery is
// ACCEPTED, not after it is applied: a crash between acceptance and
// application loses that delivery to the webhook path and the next reconcile
// heals it (the spec accepts this; there is no durable queue).
//
// A missing, unreadable or corrupt file starts an empty window; a corrupt file
// is moved aside to <file>.corrupt. A failed write is logged and costs only the
// restart-survival of that update.
const (
	deliveryLedgerWindow     = 24 * time.Hour
	deliveryLedgerMaxEntries = 10000
	deliveryLedgerVersion    = 1
	deliveryLedgerFlushAfter = time.Second
	deliveryLedgerFileName   = "deliveries.json"
)

type deliveryEntry struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

type deliveryLedgerFile struct {
	Version int             `json:"version"`
	Entries []deliveryEntry `json:"entries"`
}

type deliveryLedger struct {
	path   string // "" = memory only
	window time.Duration
	max    int
	logf   func(format string, args ...any)
	// writeFn persists the snapshot; atomicWriteFile unless a test overrides it.
	writeFn func(path string, data []byte, perm os.FileMode) error

	// flushMu serialises whole Flush calls (snapshot + write), so a timer-driven
	// flush and a Close/exec flush can never land out of order and leave an
	// older snapshot on disk. Lock order: flushMu before mu.
	flushMu sync.Mutex

	mu      sync.Mutex
	entries []deliveryEntry // oldest first
	seen    map[string]struct{}
	dirty   bool
	timer   *time.Timer
	closed  bool
}

// deliveryLedgerPath is where the production engine keeps the record.
func deliveryLedgerPath(fabrikDir string) string {
	return filepath.Join(fabrikDir, ".fabrik", "state", "webhook", deliveryLedgerFileName)
}

// newDeliveryLedger builds a ledger, loading path when it is non-empty. now is
// the load-time clock (expired entries are dropped on load).
func newDeliveryLedger(path string, now time.Time, logf func(format string, args ...any)) *deliveryLedger {
	l := &deliveryLedger{
		path:    path,
		window:  deliveryLedgerWindow,
		max:     deliveryLedgerMaxEntries,
		logf:    logf,
		seen:    make(map[string]struct{}),
		writeFn: atomicWriteFile,
	}
	l.load(now)
	return l
}

func (l *deliveryLedger) log(format string, args ...any) {
	if l.logf != nil {
		l.logf(format, args...)
	}
}

func (l *deliveryLedger) load(now time.Time) {
	if l.path == "" {
		return
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		if !os.IsNotExist(err) {
			l.log("cannot read %s: %v — starting with an empty delivery window\n", l.path, err)
		}
		return
	}
	var f deliveryLedgerFile
	if err := json.Unmarshal(data, &f); err != nil || f.Version != deliveryLedgerVersion {
		why := "unknown version"
		if err != nil {
			why = err.Error()
		}
		if qerr := os.Rename(l.path, l.path+".corrupt"); qerr != nil {
			l.log("%s unusable (%s) and could not be quarantined: %v — starting with an empty delivery window\n", l.path, why, qerr)
		} else {
			l.log("%s unusable (%s) — quarantined as %s.corrupt, starting with an empty delivery window\n", l.path, why, l.path)
		}
		return
	}
	for _, en := range f.Entries {
		if en.ID == "" {
			continue
		}
		if _, dup := l.seen[en.ID]; dup {
			continue
		}
		l.entries = append(l.entries, en)
		l.seen[en.ID] = struct{}{}
	}
	l.pruneLocked(now)
}

// pruneLocked drops entries older than the window and the oldest beyond the
// cap. Entries are appended in acceptance order, so the front is the oldest.
func (l *deliveryLedger) pruneLocked(now time.Time) {
	cut := 0
	for cut < len(l.entries) && now.Sub(l.entries[cut].At) > l.window {
		cut++
	}
	if over := len(l.entries) - cut - l.max; over > 0 {
		cut += over
	}
	if cut == 0 {
		return
	}
	for _, en := range l.entries[:cut] {
		delete(l.seen, en.ID)
	}
	l.entries = append([]deliveryEntry(nil), l.entries[cut:]...)
	l.dirty = true
}

// Accept is the atomic check-and-record. It reports true when id has not been
// accepted inside the window (and records it), false for a duplicate. An empty
// id is never a duplicate and is never recorded (FR-003): without an ID there
// is nothing to dedupe on, so the delivery is applied.
func (l *deliveryLedger) Accept(id string, now time.Time) bool {
	if l == nil || id == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	if _, dup := l.seen[id]; dup {
		return false
	}
	l.entries = append(l.entries, deliveryEntry{ID: id, At: now})
	l.seen[id] = struct{}{}
	l.pruneLocked(now) // enforce the cap immediately
	l.dirty = true
	l.scheduleFlushLocked()
	return true
}

// Forget removes id, so a delivery that was accepted but then dropped before
// application (queue overflow) is not suppressed if its source redelivers it.
func (l *deliveryLedger) Forget(id string) {
	if l == nil || id == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.seen[id]; !ok {
		return
	}
	delete(l.seen, id)
	for i := len(l.entries) - 1; i >= 0; i-- {
		if l.entries[i].ID == id {
			l.entries = append(l.entries[:i], l.entries[i+1:]...)
			break
		}
	}
	l.dirty = true
	l.scheduleFlushLocked()
}

// Len returns the number of recorded deliveries (tests).
func (l *deliveryLedger) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (l *deliveryLedger) scheduleFlushLocked() {
	if l.path == "" || l.closed || l.timer != nil {
		return
	}
	l.timer = time.AfterFunc(deliveryLedgerFlushAfter, func() {
		l.mu.Lock()
		l.timer = nil
		l.mu.Unlock()
		l.Flush()
	})
}

// Flush writes the record now if it changed. Called on close, and before the
// SIGHUP and self-upgrade exec (an exec skips deferred cleanup).
func (l *deliveryLedger) Flush() {
	if l == nil || l.path == "" {
		return
	}
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	l.mu.Lock()
	if !l.dirty {
		l.mu.Unlock()
		return
	}
	f := deliveryLedgerFile{Version: deliveryLedgerVersion, Entries: append([]deliveryEntry(nil), l.entries...)}
	l.dirty = false
	l.mu.Unlock()

	data, err := json.Marshal(f)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(l.path), 0o755)
	}
	if err == nil {
		err = l.writeFn(l.path, data, 0o600)
	}
	if err != nil {
		l.mu.Lock()
		l.dirty = true
		l.mu.Unlock()
		l.log("could not persist %s: %v\n", l.path, fmt.Sprint(err))
	}
}

// Close flushes and stops further scheduled flushes.
func (l *deliveryLedger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.closed = true
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	l.mu.Unlock()
	l.Flush()
}
