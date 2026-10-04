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
}

// queueFile is the persisted held queue of one subscriber.
type queueFile struct {
	Name    string  `json:"name"`
	Dropped int     `json:"dropped,omitempty"`
	NextSeq uint64  `json:"next_seq"`
	Entries []entry `json:"entries"`
}

// push appends ev, skipping a duplicate dedup key already queued, and drops the
// oldest entries beyond max, counting them toward the drop notice (R8). It
// reports whether the queue changed.
func (q *queueFile) push(ev Event, digest bool, due time.Time, max int) bool {
	if ev.DedupKey != "" {
		for _, e := range q.Entries {
			if e.Event.DedupKey == ev.DedupKey {
				return false
			}
		}
	}
	q.NextSeq++
	q.Entries = append(q.Entries, entry{Seq: q.NextSeq, Event: ev, Digest: digest, Due: due})
	if max > 0 && len(q.Entries) > max {
		over := len(q.Entries) - max
		q.Dropped += over
		q.Entries = append([]entry(nil), q.Entries[over:]...)
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

// pruneOlderThan drops entries older than cutoff, counting them as dropped.
func (q *queueFile) pruneOlderThan(cutoff time.Time) bool {
	changed := false
	out := q.Entries[:0:0]
	for _, e := range q.Entries {
		if e.Event.At.Before(cutoff) && !e.Event.At.IsZero() {
			q.Dropped++
			changed = true
			continue
		}
		out = append(out, e)
	}
	q.Entries = out
	return changed
}

// releaseDigests turns every pending digest entry into an immediate one. A
// reconnecting subscriber gets held events individually, in order (R8).
func (q *queueFile) releaseDigests() bool {
	changed := false
	for i := range q.Entries {
		if q.Entries[i].Digest {
			q.Entries[i].Digest = false
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
}

// next picks the next deliverable batch at time now. When nothing is due it
// returns ok=false and, if a digest is pending, the time it falls due.
func (q *queueFile) next(now time.Time) (b batch, wait time.Duration, ok bool) {
	if q.Dropped > 0 {
		return batch{
			events:  []Event{dropNotice(q.Dropped, now)},
			dropped: q.Dropped,
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

func dropNotice(n int, now time.Time) Event {
	word := "events"
	if n == 1 {
		word = "event"
	}
	return Event{
		Type:    EventsDropped,
		Content: fmt.Sprintf("%d %s dropped while no session was attached (queue overflow); the oldest were discarded", n, word),
		Meta:    map[string]string{"count": strconv.Itoa(n)},
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
