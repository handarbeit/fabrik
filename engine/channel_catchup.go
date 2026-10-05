package engine

import (
	"sort"

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
// Validate and waiting on a human, or currently needs-human or escalated (the
// pause family minus stalled, which is a time-driven live signal).
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
	suspended := e.channelSuspendedUntil(now)
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
			res := attention.Classify(e.attentionInput(&st, now, threshold, suspended))
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
	return out
}

// catchUp queues the current-state snapshot for one subscriber (only the
// subscription subID when non-empty) and reports how many events it queued. A
// void call with no hub running.
func (e *Engine) catchUp(subscriber, subID string) int {
	ce := e.channelEvents()
	if ce == nil {
		return 0
	}
	defer func() {
		if r := recover(); r != nil {
			e.logf(0, "channel", "catch-up panic recovered: %v\n", r)
		}
	}()
	n := ce.hub.CatchUp(subscriber, subID, e.catchUpEvents())
	if n > 0 {
		e.logf(0, "channel", "catch-up for %q: %d event(s)\n", subscriber, n)
	}
	return n
}
