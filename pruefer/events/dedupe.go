package events

import "sync"

// DefaultDedupeCapacity bounds the number of delivery IDs Dedupe remembers.
// Consistent with ADR-1113's zero-persistence philosophy: losing the whole
// cache (restart, or eviction past this bound) only costs a redundant
// review attempt, never correctness — ReviewPR's own SHA-idempotency
// (alreadyReviewedAtHead) is the real idempotency guard. Not user-facing
// config; see adrs/1254-*.md for why this is a fixed implementation detail.
const DefaultDedupeCapacity = 4096

// Dedupe is a bounded, in-memory, insertion-order-eviction cache of
// recently-seen delivery IDs. Safe for concurrent use.
type Dedupe struct {
	mu       sync.Mutex
	capacity int
	seen     map[string]struct{}
	order    []string
}

// NewDedupe returns a Dedupe holding at most capacity delivery IDs. A
// capacity <= 0 uses DefaultDedupeCapacity.
func NewDedupe(capacity int) *Dedupe {
	if capacity <= 0 {
		capacity = DefaultDedupeCapacity
	}
	return &Dedupe{
		capacity: capacity,
		seen:     make(map[string]struct{}, capacity),
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
	d.seen[id] = struct{}{}
	d.order = append(d.order, id)
	if len(d.order) > d.capacity {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, oldest)
	}
	return false
}
