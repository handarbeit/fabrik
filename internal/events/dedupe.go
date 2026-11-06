package events

import (
	"sync"
	"time"
)

// DefaultDedupeCapacity bounds the number of delivery IDs Dedupe remembers.
// Consistent with ADR-1113's zero-persistence philosophy: losing the whole
// cache (restart, or eviction past this bound) only costs a redundant
// review attempt, never correctness — ReviewPR's own SHA-idempotency
// (alreadyReviewedAtHead) is the real idempotency guard. Not user-facing
// config; see adrs/1254-*.md for why this is a fixed implementation detail.
const DefaultDedupeCapacity = 4096

// Dedupe is a bounded, in-memory, insertion-order-eviction cache of
// recently-seen delivery IDs, backed by a fixed-size ring buffer so its
// memory footprint never grows past capacity. Safe for concurrent use.
type Dedupe struct {
	mu       sync.Mutex
	capacity int
	seen     map[string]time.Time // id -> record time
	now      func() time.Time
	order    []string // len == capacity; ring buffer, next write at head
	head     int
	size     int // number of valid entries in order, <= capacity
}

// NewDedupe returns a Dedupe holding at most capacity delivery IDs. A
// capacity <= 0 uses DefaultDedupeCapacity.
func NewDedupe(capacity int) *Dedupe {
	if capacity <= 0 {
		capacity = DefaultDedupeCapacity
	}
	return &Dedupe{
		capacity: capacity,
		seen:     make(map[string]time.Time, capacity),
		order:    make([]string, capacity),
		now:      time.Now,
	}
}

// SeenBefore records id and reports whether it was already present. The
// first call for a given id returns false; every subsequent call for the
// same id returns true until id is evicted to make room for newer IDs. An
// empty id is never considered a duplicate of another empty id — sources
// that can't determine a delivery ID should not have their events
// collapsed together.
func (d *Dedupe) SeenBefore(id string) bool {
	if id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[id]; ok {
		return true
	}
	if d.size == d.capacity {
		delete(d.seen, d.order[d.head])
	} else {
		d.size++
	}
	d.order[d.head] = id
	d.seen[id] = d.now()
	d.head = (d.head + 1) % d.capacity
	return false
}

// Seen reports whether id is currently recorded, recording nothing. It is the
// read-only check the delivery replayer uses (#2476): SeenBefore records on
// lookup, so using it there would make a requested redelivery look like a
// duplicate and silently drop it. An empty id is never seen.
func (d *Dedupe) Seen(id string) bool {
	if id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.seen[id]
	return ok
}

// Newest returns the record time of the most recently recorded id, zero when
// nothing is recorded. Read-only.
func (d *Dedupe) Newest() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.size == 0 {
		return time.Time{}
	}
	last := (d.head - 1 + d.capacity) % d.capacity
	return d.seen[d.order[last]]
}

// ReceivedAt returns when id was recorded, and whether it is recorded. now is
// accepted for interface parity with the engine ledger (which also ages
// records out of a time window); this ring is bounded by size alone. Read-only.
func (d *Dedupe) ReceivedAt(id string, _ time.Time) (time.Time, bool) {
	if id == "" {
		return time.Time{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.seen[id]
	return t, ok
}

// SetClockForTest replaces the record-time clock. Test use only.
func (d *Dedupe) SetClockForTest(now func() time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.now = now
}
