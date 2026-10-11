package channelevents

// legacyEventTypes maps event type strings written by builds that predate the
// #2129 rename (the account-wide Claude "suspension" became "usage limited")
// to their current names. It is applied once, to state loaded from disk by
// Open — never to a subscribe request, so Subscription.Validate still rejects
// the old strings. It exists only so a subscription or held entry persisted by
// a pre-rename build does not stay silently unmatched. This is the one place
// the old strings remain outside ADRs and release notes.
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
