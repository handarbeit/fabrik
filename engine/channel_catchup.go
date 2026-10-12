package engine

import (
	"sort"
	"strconv"

	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Catch-up (#1968 R2/R4, ADR-1966-b). A settle or a pause is announced once, at
// its transition, to whoever subscribes at that moment. A subscriber that
// arrives later — a new subscription, or a fresh session under an existing name —
// would never learn about an item already settled at Validate and waiting on a
// human (the headline cruise case) or already needs-human / escalated.
//
// catchUpEvents is therefore a snapshot of *current* state, built from the store
// alone (Scan + Peek, never GitHub: R9) and tagged meta catch_up=true. It is not
// a transition: it carries no dedup key, touches no episode counter and no
// per-item memory, so it cannot interfere with the live events' once-per-episode
// dedup. Observation only (R10).

// catchUpEvents lists, for every cached item, one event when it is settled at
// Validate and waiting on a human, currently suspended (a pending decision), or
// currently needs-human or escalated (the pause family minus stalled, which is a
// time-driven live signal).
func (e *Engine) catchUpEvents() []channelevents.Event {
	type ref struct {
		repo string
		n    int
	}
	var refs []ref
	e.store.Scan(func(st *itemstate.ItemState) {
		if !st.IsPR && !st.IsClosed {
			refs = append(refs, ref{st.Repo, st.Number})
		}
	})
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].repo != refs[j].repo {
			return refs[i].repo < refs[j].repo
		}
		return refs[i].n < refs[j].n
	})

	now := e.now()
	threshold := e.channelStallThreshold()
	usageLimited := e.channelUsageLimitedUntil(now)
	look := e.peekBlockerLookup()
	var validate *stages.Stage
	for _, s := range e.cfg.Stages {
		if s.Name == "Validate" {
			validate = s
			break
		}
	}

	var out []channelevents.Event
	for _, r := range refs {
		snap, ok := e.store.Peek(r.repo, r.n)
		if !ok {
			continue
		}
		st := snap.State()
		var ev channelevents.Event
		switch {
		case st.Status == "Validate" && e.validateSettledSnap(snap, &st, false) &&
			e.validateSettledNext(&st) == "waiting-for-human":
			ev = e.validateSettledEvent(snap, &st, validate)
			ev.Content = "(catch-up) " + ev.Content
		default:
			res := attention.Classify(e.attentionInput(&st, now, threshold, usageLimited, look))
			if res.State == attention.Suspended && res.Pending != nil {
				// A pending decision is current state a late subscriber must learn
				// (#2120 R6): same event as the live one, tagged catch_up.
				ev = e.decisionEvent(&st, res)
				ev.Content = "(catch-up) " + ev.Content
				// The open episode's key rides along for the hub, which restarts the
				// subscriber's reminder clock with it and clears it before queueing.
				ev.DedupKey = e.openDecisionKey(issueRef(st.Repo, st.Number))
				break
			}
			key := pauseFamilyKey(res)
			if key == "" || key == "stalled" {
				continue
			}
			ev = e.pauseEventFor(&st, key, res, now)
			ev.Content = "(catch-up) " + ev.Content
		}
		ev.Meta["catch_up"] = "true"
		if k := latestFabrikComment(&st); k.Valid {
			ev.CommentURL = k.V
		}
		out = append(out, finishEvent(ev))
	}
	// A pipeline stall is current account state, unlike an item's time-driven
	// stalled, so an open one is part of the snapshot (#2124).
	out = append(out, e.pipelineCatchUpEvents(now)...)
	// Open retry-starved and item-wedged episodes are current state too (#2170).
	out = append(out, e.anomalyCatchUpEvents(now)...)
	// Whatever a builder stamped before it was tagged (the stall builders finish
	// their event as live), every event of the snapshot is a catch_up (#2169 R3a).
	for i := range out {
		if out[i].Meta["catch_up"] == "true" {
			out[i].Meta[channelevents.MetaSource] = channelevents.SourceCatchUp
		}
	}
	return out
}

// catchUp queues the current-state snapshot for one subscriber (only the
// subscription subID when non-empty) and reports how many events it queued. A
// void call with no hub running.
//
// A subscription that takes the situation digest (#2171) is caught up with the
// digest itself — one forced `situation` event, built by the same builder as the
// fabrik_situation tool, so what a session sees on arrival is exactly what the
// tool would say. A subscription that does not (one persisted before the digest
// existed, or an explicit list without it) keeps the per-item snapshot.
func (e *Engine) catchUp(subscriber, subID string) int {
	ce := e.channelEvents()
	if ce == nil {
		return 0
	}
	// Until the readiness pass has run, the store cannot answer (labels are still
	// being populated after a restart); that pass queues the catch-up to every
	// subscriber, this one included (#2169).
	if !ce.ready.Load() {
		return 0
	}
	defer func() {
		if r := recover(); r != nil {
			e.logf(0, "channel", "catch-up panic recovered: %v\n", r)
		}
	}()
	n := e.catchUpSituation(subscriber, subID)
	if perItem := e.perItemCatchUpWanted(subscriber, subID); perItem {
		n += ce.hub.CatchUpWhere(subscriber, subID, e.catchUpEvents(), notTakingSituation)
	}
	if n > 0 {
		e.logf(0, "channel", "catch-up for %q: %d event(s)\n", subscriber, n)
	}
	return n
}

func notTakingSituation(s channelevents.Subscription) bool { return !s.TakesSituation() }

// subscriptionsOf lists a subscriber's subscriptions, only subID's when non-empty.
func (ce *channelEvents) subscriptionsOf(subscriber, subID string) []channelevents.Subscription {
	var out []channelevents.Subscription
	for _, sub := range ce.hub.Subscriptions(subscriber) {
		if subID == "" || sub.ID == subID {
			out = append(out, sub)
		}
	}
	return out
}

// perItemCatchUpWanted reports whether any of the selected subscriptions is
// caught up with per-item events rather than the digest.
func (e *Engine) perItemCatchUpWanted(subscriber, subID string) bool {
	ce := e.channelEvents()
	if ce == nil {
		return false
	}
	for _, sub := range ce.subscriptionsOf(subscriber, subID) {
		if !sub.TakesSituation() {
			return true
		}
	}
	return false
}

// catchUpSituation queues the forced catch-up digest when any selected
// subscription takes situation, and returns how many digests it queued (0 or 1).
// Recording its fingerprint is what stops the first scheduled digest repeating it.
func (e *Engine) catchUpSituation(subscriber, subID string) int {
	ce := e.channelEvents()
	if ce == nil {
		return 0
	}
	takes, reminds := false, false
	for _, sub := range ce.subscriptionsOf(subscriber, subID) {
		if sub.TakesSituation() {
			takes = true
			reminds = reminds || sub.RemindAfterSeconds > 0
		}
	}
	if !takes {
		return 0
	}
	n := 0
	if e.pushSituation(subscriber, channelevents.TriggerCatchUp, true) {
		n = 1
	}
	if reminds {
		// The decisions are announced inside the digest now; keep the reminder
		// clock behind it as the per-item catch-up did.
		ce.hub.RestartReminders(subscriber, e.catchUpEvents())
	}
	return n
}

// openDecisionKey is the dedup key of the item's open decision-pending episode,
// or "" when none is open (or no hub runs). Counters are read from the hub, which
// guards them, so any goroutine may call it.
func (e *Engine) openDecisionKey(ref string) string {
	ce := e.channelEvents()
	if ce == nil {
		return ""
	}
	started, closed := dpKeys(ref)
	n := ce.hub.Counter(started)
	if n == 0 || n <= ce.hub.Counter(closed) {
		return ""
	}
	return started + ":" + strconv.Itoa(n)
}
