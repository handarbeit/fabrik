package channelevents

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// entry is one queued event with its delivery disposition.
type entry struct {
	Seq   uint64 `json:"seq"`
	Event Event  `json:"event"`
	// Digest entries wait for Due and are then sent as one batch.
	Digest bool      `json:"digest,omitempty"`
	Due    time.Time `json:"due,omitempty"`
	// Away marks a digest entry queued while the subscriber had no attached
	// session: the next attach delivers it individually (R8) instead of batching.
	Away bool `json:"away,omitempty"`
}

// queueFile is the persisted held queue of one subscriber.
type queueFile struct {
	Name    string `json:"name"`
	Dropped int    `json:"dropped,omitempty"`
	// DroppedTypes breaks Dropped down by event type so the drop notice can name
	// what was lost (a lost validate-settled is not the same as a lost label event).
	DroppedTypes map[EventType]int `json:"dropped_types,omitempty"`
	// Expired is how many of Dropped aged out (MaxAge) rather than being evicted
	// by overflow, so the notice names the right cause.
	Expired int     `json:"expired,omitempty"`
	NextSeq uint64  `json:"next_seq"`
	Entries []entry `json:"entries"`
}

// noteDropped counts one discarded entry of type t toward the drop notice.
func (q *queueFile) noteDropped(t EventType) {
	q.Dropped++
	if q.DroppedTypes == nil {
		q.DroppedTypes = map[EventType]int{}
	}
	q.DroppedTypes[t]++
}

// evictOne discards one entry to make room: the oldest non-immediate entry, or
// the oldest entry overall only when every entry is immediate. A flood of label
// events must not push out a held validate-settled, escalated or paused.
func (q *queueFile) evictOne() {
	idx := 0
	for i, e := range q.Entries {
		if !IsImmediate(e.Event.Type) {
			idx = i
			break
		}
	}
	q.noteDropped(q.Entries[idx].Event.Type)
	q.Entries = append(q.Entries[:idx:idx], q.Entries[idx+1:]...)
}

// push appends ev, skipping a duplicate dedup key already queued, and evicts
// entries beyond max (non-immediate first, oldest first), counting them toward
// the drop notice (R8). It reports whether the queue changed.
func (q *queueFile) push(ev Event, digest bool, due time.Time, away bool, max int) bool {
	if ev.DedupKey != "" {
		for _, e := range q.Entries {
			if e.Event.DedupKey == ev.DedupKey {
				return false
			}
		}
	}
	q.NextSeq++
	q.Entries = append(q.Entries, entry{Seq: q.NextSeq, Event: ev, Digest: digest, Due: due, Away: digest && away})
	if max > 0 && len(q.Entries) > max {
		for len(q.Entries) > max {
			q.evictOne()
		}
	}
	return true
}

// remove deletes the entries with the given sequence numbers.
func (q *queueFile) remove(seqs map[uint64]bool) {
	out := q.Entries[:0:0]
	for _, e := range q.Entries {
		if !seqs[e.Seq] {
			out = append(out, e)
		}
	}
	q.Entries = out
}

// pruneOlderThan drops entries older than cutoff, counting them (by type) as
// dropped and as expired.
func (q *queueFile) pruneOlderThan(cutoff time.Time) bool {
	changed := false
	out := q.Entries[:0:0]
	for _, e := range q.Entries {
		if e.Event.At.Before(cutoff) && !e.Event.At.IsZero() {
			q.noteDropped(e.Event.Type)
			q.Expired++
			changed = true
			continue
		}
		out = append(out, e)
	}
	q.Entries = out
	return changed
}

// releaseDigests turns the digest entries queued while the subscriber was away
// into immediate ones: a returning subscriber gets what it missed individually,
// in order (R8). A digest collected while a session was attached keeps batching
// across a brief reconnect (heartbeat reset, daemon re-exec) and is sent when due.
func (q *queueFile) releaseDigests() bool {
	changed := false
	for i := range q.Entries {
		if q.Entries[i].Digest && q.Entries[i].Away {
			q.Entries[i].Digest = false
			q.Entries[i].Away = false
			changed = true
		}
	}
	return changed
}

// batch is what the drain loop sends next: the events, the sequence numbers to
// remove on success, and (for a drop notice) the count to subtract.
type batch struct {
	events  []Event
	seqs    map[uint64]bool
	dropped int
	// expired is how many of dropped aged out rather than overflowed.
	expired int
	// droppedTypes is the per-type breakdown the notice reports; subtracted from
	// the queue once the notice is delivered.
	droppedTypes map[EventType]int
}

// clearDropped subtracts a delivered notice's counts from the queue.
func (q *queueFile) clearDropped(b batch) {
	q.Dropped -= b.dropped
	q.Expired -= b.expired
	for t, n := range b.droppedTypes {
		if q.DroppedTypes[t] -= n; q.DroppedTypes[t] <= 0 {
			delete(q.DroppedTypes, t)
		}
	}
	if q.Dropped <= 0 {
		q.Dropped, q.DroppedTypes = 0, nil
	}
	if q.Expired < 0 || q.Dropped == 0 {
		q.Expired = 0
	}
}

// next picks the next deliverable batch at time now. When nothing is due it
// returns ok=false and, if a digest is pending, the time it falls due.
func (q *queueFile) next(now time.Time) (b batch, wait time.Duration, ok bool) {
	if q.Dropped > 0 {
		return batch{
			events:       []Event{dropNotice(q.Dropped, q.Expired, q.DroppedTypes, now)},
			dropped:      q.Dropped,
			expired:      q.Expired,
			droppedTypes: copyCounts(q.DroppedTypes),
		}, 0, true
	}
	for _, e := range q.Entries {
		if !e.Digest {
			return batch{events: []Event{e.Event}, seqs: map[uint64]bool{e.Seq: true}}, 0, true
		}
	}
	var due time.Time
	for _, e := range q.Entries {
		if e.Digest && (due.IsZero() || e.Due.Before(due)) {
			due = e.Due
		}
	}
	if due.IsZero() {
		return batch{}, 0, false
	}
	if !now.Before(due) {
		var evs []Event
		seqs := map[uint64]bool{}
		for _, e := range q.Entries {
			if e.Digest {
				evs = append(evs, e.Event)
				seqs[e.Seq] = true
			}
		}
		return batch{events: []Event{digestEvent(evs, now)}, seqs: seqs}, 0, true
	}
	return batch{}, due.Sub(now), false
}

func copyCounts(m map[EventType]int) map[EventType]int {
	if len(m) == 0 {
		return nil
	}
	out := make(map[EventType]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// describeCounts renders per-type counts in a stable order, e.g.
// "label-applied x3, validate-settled x1".
func describeCounts(m map[EventType]int) string {
	types := make([]string, 0, len(m))
	for t := range m {
		types = append(types, string(t))
	}
	sort.Strings(types)
	parts := make([]string, 0, len(types))
	for _, t := range types {
		parts = append(parts, fmt.Sprintf("%s x%d", t, m[EventType(t)]))
	}
	return strings.Join(parts, ", ")
}

func dropNotice(n, expired int, types map[EventType]int, now time.Time) Event {
	word := "events"
	if n == 1 {
		word = "event"
	}
	var cause string
	switch {
	case expired >= n:
		cause = "expired: held longer than the retention window"
	case expired > 0:
		cause = fmt.Sprintf("queue overflow, and %d expired after the retention window", expired)
	default:
		cause = "queue overflow; non-urgent events are discarded first, oldest first"
	}
	content := fmt.Sprintf("%d %s dropped while no session was attached (%s)", n, word, cause)
	meta := map[string]string{"count": strconv.Itoa(n)}
	if expired > 0 {
		meta["expired"] = strconv.Itoa(expired)
	}
	if len(types) > 0 {
		desc := describeCounts(types)
		content += ": " + desc
		meta["dropped_types"] = desc
	}
	return Event{
		Type:    EventsDropped,
		Content: content,
		Meta:    meta,
		At:      now,
	}
}

// digestEvent folds evs into one synthetic message.
func digestEvent(evs []Event, now time.Time) Event {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].At.Before(evs[j].At) })
	var lines []string
	for _, e := range evs {
		ref := e.Repo
		if e.Issue != 0 {
			ref = fmt.Sprintf("%s#%d", e.Repo, e.Issue)
		}
		line := fmt.Sprintf("[%s] %s", e.Type, e.Content)
		if ref != "" {
			line = fmt.Sprintf("[%s] %s: %s", e.Type, ref, e.Content)
		}
		lines = append(lines, line)
	}
	return Event{
		Type:    Digest,
		Content: strings.Join(lines, "\n"),
		Meta:    map[string]string{"count": strconv.Itoa(len(evs))},
		At:      now,
	}
}
