package channelevents

import (
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// The ledger is the persisted counterpart of #2123's in-memory LabelsKnown
// baseline (#2169 R2): it remembers, per item, the labels and board column the
// daemon last observed, so the first full observation after a restart can be
// diffed against it instead of being swallowed as a baseline. A difference is a
// change made while the daemon was down.
//
// It records what the store *observed*, not what any one subscriber was sent: a
// subscription filter must not change what counts as a downtime change, and a
// detached subscriber misses nothing the daemon saw.
//
// The observer that feeds it must not block, so Observe is a map write under the
// ledger's own mutex; the disk write is coalesced on a timer and flushed by
// Close and by the self-upgrade pre-exec hook. A crash inside the flush window
// can leave the file one flush behind, which a later start reports as a delta
// already delivered — hence downtime deltas are marked unverified.

const (
	ledgerVersion = 1
	// ledgerFlushDelay coalesces writes. Short enough that a crash loses little,
	// long enough that a burst of changes is one write.
	ledgerFlushDelay = time.Second
	// ledgerMaxAge drops a record not observed for this long, so a long-gone
	// item does not stay forever.
	ledgerMaxAge = 14 * 24 * time.Hour
)

// SettleRecord is the memory of the last validate-settled announced for an item,
// from which the next one derives its cause (#2169 R3a).
type SettleRecord struct {
	// Episode is the episode number of the announcement.
	Episode int `json:"episode"`
	// HeadSHA and Autonomy are the announced event's head_sha and autonomy.
	HeadSHA  string `json:"head_sha,omitempty"`
	Autonomy string `json:"autonomy,omitempty"`
	// Revalidated is set when the episode ended because the item was sent back
	// through Validate (fabrik:revalidate).
	Revalidated bool `json:"revalidated,omitempty"`
}

// ItemRecord is what the ledger keeps for one item.
type ItemRecord struct {
	// Labels are the item's labels minus the default churn exclusions, sorted.
	// Meaningful only when LabelsKnown.
	Labels      []string `json:"labels,omitempty"`
	LabelsKnown bool     `json:"labels_known,omitempty"`
	// Status is the board column ("" = unknown).
	Status string `json:"status,omitempty"`
	Closed bool   `json:"closed,omitempty"`
	// Settle is the last announced validate-settled.
	Settle *SettleRecord `json:"settle,omitempty"`
	// Seen is when the item was last observed.
	Seen time.Time `json:"seen"`
}

// Observation is one look at an item's state.
type Observation struct {
	// Labels are all of the item's labels; ignored unless LabelsKnown.
	Labels      []string
	LabelsKnown bool
	Status      string
	Closed      bool
}

type ledgerFile struct {
	Version int                   `json:"version"`
	Items   map[string]ItemRecord `json:"items"`
}

// Ledger persists the last-observed per-item state in items.json beside the
// hub's other state files.
type Ledger struct {
	path string
	now  func() time.Time
	logf func(format string, args ...any)

	mu       sync.Mutex
	prior    map[string]ItemRecord // what the previous run left; consumed by TakePrior
	current  map[string]ItemRecord // prior carried forward, overlaid by what this run observed
	dirty    bool
	timer    *time.Timer
	closed   bool
	writeMu  sync.Mutex // orders disk writes; taken before mu is released for I/O, never while holding mu
	writes   int
	disabled bool
}

// OpenLedger loads items.json under dir. A missing, corrupt or version-mismatched
// file yields an empty prior state — every item is then a baseline — and is
// logged, never fatal and never delivered to a subscriber.
func OpenLedger(dir string, now func() time.Time, logf func(string, ...any)) *Ledger {
	if now == nil {
		now = time.Now
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	l := &Ledger{
		path:    filepath.Join(dir, "items.json"),
		now:     now,
		logf:    logf,
		prior:   map[string]ItemRecord{},
		current: map[string]ItemRecord{},
	}
	var f ledgerFile
	ok, quarantined, err := loadJSON(l.path, &f, now())
	switch {
	case err != nil:
		logf("channel: reading item ledger: %v (every item will be a baseline)\n", err)
	case quarantined != "":
		logf("channel: corrupt item ledger quarantined to %s (every item will be a baseline)\n", quarantined)
	case ok && f.Version != ledgerVersion:
		logf("channel: item ledger version %d is not %d (every item will be a baseline)\n", f.Version, ledgerVersion)
	case ok:
		cutoff := now().Add(-ledgerMaxAge)
		for k, r := range f.Items {
			if r.Seen.Before(cutoff) {
				continue
			}
			l.prior[k] = r
			l.current[k] = r
		}
	}
	return l
}

// TakePrior returns, once, the record the previous run left for key. A second
// call, or a key with no record, reports false: an item with no persisted state
// is a baseline.
func (l *Ledger) TakePrior(key string) (ItemRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.prior[key]
	if ok {
		delete(l.prior, key)
	}
	return r, ok
}

// PeekPrior returns the not-yet-taken prior record for key without consuming it.
func (l *Ledger) PeekPrior(key string) (ItemRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.prior[key]
	return r, ok
}

// SetPriorStatus rewrites the column of a not-yet-taken prior record, so a move
// the live path already reported is not reported again as a downtime delta.
func (l *Ledger) SetPriorStatus(key, status string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r, ok := l.prior[key]; ok {
		r.Status = status
		l.prior[key] = r
	}
}

// Observe records the item's state as just observed. Unknown parts (labels not
// yet populated, an empty column) leave the carried-forward value alone. It does
// no I/O.
func (l *Ledger) Observe(key string, o Observation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.disabled {
		return
	}
	r := l.current[key]
	if o.LabelsKnown {
		r.Labels = ledgerLabels(o.Labels)
		r.LabelsKnown = true
	}
	if o.Status != "" {
		r.Status = o.Status
	}
	r.Closed = o.Closed
	r.Seen = l.now()
	l.current[key] = r
	l.scheduleLocked()
}

// Settle returns the last announced validate-settled recorded for key.
func (l *Ledger) Settle(key string) (SettleRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r, ok := l.current[key]; ok && r.Settle != nil {
		return *r.Settle, true
	}
	return SettleRecord{}, false
}

// SetSettle records the last announced validate-settled for key.
func (l *Ledger) SetSettle(key string, s SettleRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.disabled {
		return
	}
	r := l.current[key]
	r.Settle = &s
	r.Seen = l.now()
	l.current[key] = r
	l.scheduleLocked()
}

// MarkRevalidated notes that the episode last announced for key ended because
// the item was sent back through Validate.
func (l *Ledger) MarkRevalidated(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.disabled {
		return
	}
	r, ok := l.current[key]
	if !ok || r.Settle == nil || r.Settle.Revalidated {
		return
	}
	s := *r.Settle
	s.Revalidated = true
	r.Settle = &s
	l.current[key] = r
	l.scheduleLocked()
}

// scheduleLocked arms the coalesced write. l.mu held; no I/O.
func (l *Ledger) scheduleLocked() {
	l.dirty = true
	if l.timer == nil && !l.closed {
		l.timer = time.AfterFunc(ledgerFlushDelay, l.Flush)
	}
}

// Flush writes the ledger now if it changed.
func (l *Ledger) Flush() {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	l.mu.Lock()
	l.timer = nil
	if !l.dirty {
		l.mu.Unlock()
		return
	}
	l.dirty = false
	cutoff := l.now().Add(-ledgerMaxAge)
	items := make(map[string]ItemRecord, len(l.current))
	for k, r := range l.current {
		if r.Seen.Before(cutoff) {
			continue
		}
		items[k] = r
	}
	l.mu.Unlock()
	if err := saveJSON(l.path, ledgerFile{Version: ledgerVersion, Items: items}); err != nil {
		l.logf("channel: saving item ledger: %v\n", err)
		return
	}
	l.mu.Lock()
	l.writes++
	l.mu.Unlock()
}

// Close flushes and stops accepting observations. Idempotent.
func (l *Ledger) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	l.mu.Unlock()
	l.Flush()
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

// Writes reports how many times the ledger reached disk (tests).
func (l *Ledger) Writes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writes
}

// SetDisabledForTest turns recording off, so a restart finds no persisted state.
func (l *Ledger) SetDisabledForTest(off bool) {
	l.mu.Lock()
	l.disabled = off
	l.mu.Unlock()
}

// ledgerLabels filters the default churn labels out and sorts, so startup
// cleanup (fabrik:locked:*, stage:*:in_progress) never shows as a downtime delta.
func ledgerLabels(in []string) []string {
	out := make([]string, 0, len(in))
	for _, l := range in {
		if matchAny(DefaultExcludeLabels, l) {
			continue
		}
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// LedgerLabels is the form of labels the ledger stores, for callers diffing a
// prior record against the current state.
func LedgerLabels(in []string) []string { return ledgerLabels(in) }
