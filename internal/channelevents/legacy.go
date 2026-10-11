package channelevents

// legacyEventTypes maps event type strings written by builds that predate the
// #2129 rename (the account-wide Claude "suspension" became "usage limited")
// to their current names. It is applied to state loaded from disk by Open and,
// since #2168, to a subscribe request through NormalizeEvents (a long-lived
// shim built before the rename still advertises the old strings). Hub and
// Subscription.Validate stay strict and reject them: the caller normalises first
// and reports the mapping. This is the one place the old strings remain outside
// ADRs and release notes.
var legacyEventTypes = map[EventType]EventType{
	"claude-limit-suspended": ClaudeUsageLimited,
	"claude-limit-lifted":    ClaudeUsageLimitLifted,
}

// migrateLegacyType returns t's current name.
func migrateLegacyType(t EventType) EventType {
	if n, ok := legacyEventTypes[t]; ok {
		return n
	}
	return t
}

// migrateSubscriptions rewrites legacy event types in subs in place, reporting
// whether anything changed.
func migrateSubscriptions(subs []Subscription) bool {
	changed := false
	for i := range subs {
		for j, t := range subs[i].Events {
			if n := migrateLegacyType(t); n != t {
				subs[i].Events[j] = n
				changed = true
			}
		}
	}
	return changed
}

// migrateQueue rewrites legacy event types in a held queue (entries and the
// dropped-type breakdown) in place, reporting whether anything changed.
func migrateQueue(q *queueFile) bool {
	changed := false
	for i := range q.Entries {
		if n := migrateLegacyType(q.Entries[i].Event.Type); n != q.Entries[i].Event.Type {
			q.Entries[i].Event.Type = n
			changed = true
		}
	}
	for t, c := range q.DroppedTypes {
		if n := migrateLegacyType(t); n != t {
			delete(q.DroppedTypes, t)
			q.DroppedTypes[n] += c
			changed = true
		}
	}
	return changed
}

// EventMapping reports one requested event type that was rewritten to its
// current name.
type EventMapping struct {
	From EventType `json:"from"`
	To   EventType `json:"to"`
}

// NormalizeEvents prepares the event list of a subscribe request. Legacy names
// become their current name (mapped), subscribable current names pass through,
// and anything else — unknown, or a synthetic type no subscription may name —
// is rejected. kept preserves request order and has no duplicates, so
// ["claude-limit-suspended", "claude-usage-limited"] collapses to one entry;
// mapped and rejected list each distinct input once, in request order.
//
// An empty kept is not an empty filter: a subscription with no Events matches
// every event, so a caller must refuse a request that named events yet kept none.
func NormalizeEvents(in []EventType) (kept []EventType, mapped []EventMapping, rejected []EventType) {
	seenKept := map[EventType]bool{}
	seenMapped := map[EventType]bool{}
	seenRejected := map[EventType]bool{}
	for _, t := range in {
		cur := t
		if n, ok := legacyEventTypes[t]; ok {
			cur = n
			if !seenMapped[t] {
				seenMapped[t] = true
				mapped = append(mapped, EventMapping{From: t, To: n})
			}
		}
		info, ok := Lookup(cur)
		if !ok || info.Synthetic {
			if !seenRejected[t] {
				seenRejected[t] = true
				rejected = append(rejected, t)
			}
			continue
		}
		if !seenKept[cur] {
			seenKept[cur] = true
			kept = append(kept, cur)
		}
	}
	return kept, mapped, rejected
}
