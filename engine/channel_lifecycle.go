package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Lifecycle events (#2123, ADR 2123): item-added, column-changed and closed, plus
// the engine-side merged emit that orders a landing's `merged` ahead of its
// close tracking.
//
// Everything here follows the channel layer's observation-only rules (R9/R10):
// events are built from the itemstate Change and snapshot the observer is handed,
// or from a Store.Peek inside a hook; nothing reaches GitHub, and no function here
// can change an engine decision.

// arrivalMemo is the consumer-visible record that an item was created after the
// board was populated and has not been announced yet. It is held until the
// item's labels are known and it carries a board Status, so item-added never
// reports an empty label set because labels had not loaded (FR-005).
//
// Guarded by channelEvents.mu (the observer writes it on the Apply caller's
// goroutine, the consumer reads and clears it).

// noteArrival records a pending arrival for key.
func (ce *channelEvents) noteArrival(key string) {
	if ce.arrivals == nil {
		ce.arrivals = map[string]time.Time{}
	}
	if _, ok := ce.arrivals[key]; !ok {
		ce.arrivals[key] = time.Now()
	}
}

// isLanding reports whether a landing was announced for key and not yet pruned.
func (ce *channelEvents) isLanding(key string) bool {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	_, ok := ce.landings[key]
	return ok
}

// markLanding records that key's landing was announced, so close-tracking events
// that follow can say they belong to it.
func (ce *channelEvents) markLanding(key string) {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	if ce.landings == nil {
		ce.landings = map[string]time.Time{}
	}
	ce.landings[key] = time.Now()
}

// pruneLifecycle drops arrivals that never became announceable and landing marks
// that are old enough to no longer matter.
func (ce *channelEvents) pruneLifecycle() {
	cut := time.Now().Add(-channelPrune)
	ce.mu.Lock()
	defer ce.mu.Unlock()
	for k, at := range ce.arrivals {
		if at.Before(cut) {
			delete(ce.arrivals, k)
			ce.e.logf(0, "channel", "arrival of %s never became announceable (no labels or board column within %s); dropped\n", k, channelPrune)
		}
	}
	for k, at := range ce.landings {
		if at.Before(cut) {
			delete(ce.landings, k)
		}
	}
}

// pendingArrival reports whether key has an unannounced arrival.
func (ce *channelEvents) pendingArrival(key string) bool {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	_, ok := ce.arrivals[key]
	return ok
}

// announceArrival returns the item-added event for key once the item is
// announceable: labels known, on the board (has a Status), open, not a PR. A PR
// card or an already-closed item is dropped without an event. Consumer goroutine.
func (ce *channelEvents) announceArrival(key string, st *itemstate.ItemState) []channelevents.Event {
	if !ce.pendingArrival(key) {
		return nil
	}
	if st.IsPR || st.IsClosed {
		ce.clearArrival(key)
		return nil
	}
	if !st.LabelsKnown || st.Status == "" {
		return nil
	}
	ce.clearArrival(key)
	return []channelevents.Event{ce.e.itemAddedEvent(st)}
}

func (ce *channelEvents) clearArrival(key string) {
	ce.mu.Lock()
	delete(ce.arrivals, key)
	ce.mu.Unlock()
}

// itemAddedEvent builds the arrival event from the item's snapshot.
func (e *Engine) itemAddedEvent(st *itemstate.ItemState) channelevents.Event {
	ref := issueRef(st.Repo, st.Number)
	ev := e.baseEvent(st, channelevents.ItemAdded)
	ev.Labels = append([]string(nil), st.Labels...)
	ev.DedupKey = "item-added:" + ref
	ev.Meta["title"] = st.Title
	ev.Meta["column"] = st.Status
	ev.Meta["labels"] = strings.Join(st.Labels, ",")
	ev.Meta["autonomy"] = autonomyOf(st.Labels)
	if st.Author != "" {
		ev.Meta["author"] = st.Author
	}
	if st.URL != "" {
		ev.Meta["url"] = st.URL
	}
	if st.MilestoneKnown {
		if st.Milestone != nil {
			ev.Meta["milestone"] = st.Milestone.Title
		} else {
			ev.Meta["milestone"] = "none"
		}
	}
	ev.Content = fmt.Sprintf("%s %q was added to the board at %s", ref, st.Title, st.Status)
	if a := autonomyOf(st.Labels); a != "none" {
		ev.Content += " on " + a
	}
	return ev
}

// attributionSuffix renders an attribution for event content and sets its meta.
func attributionMeta(ev *channelevents.Event, attr labelAttribution) string {
	ev.Meta["actor"] = attr.Actor
	by := attr.Actor
	if attr.Via != "" {
		ev.Meta["via"] = attr.Via
		by += " " + attr.Via
	}
	if attr.Login != "" {
		ev.Meta["actor_login"] = attr.Login
		by += " " + attr.Login
	}
	return by
}

// columnEvent builds column-changed from a Status change whose previous Status
// was known. The actor reuses the label attribution (ADR 2125): the engine's own
// write or its echo is fabrik, an overseer action is overseer with its
// subscriber, a webhook sender is human or bot, and everything seen only by the
// sweep, a deep fetch or a reconcile is unknown.
func (e *Engine) columnEvent(st *itemstate.ItemState, c itemstate.Change) channelevents.Event {
	ref := issueRef(st.Repo, st.Number)
	ev := e.baseEvent(st, channelevents.ColumnChanged)
	ev.Meta["from"] = c.PrevStatus
	ev.Meta["to"] = st.Status
	by := attributionMeta(&ev, e.labelAttribute(c))
	if !c.StatusAt.IsZero() {
		ev.Meta["status_at"] = c.StatusAt.UTC().Format(time.RFC3339)
	}
	ev.Content = fmt.Sprintf("%s moved from %s to %s (by %s)", ref, c.PrevStatus, st.Status, by)
	return ev
}

// closeTrackingLabel reports whether label tracks an issue close after a landing.
func closeTrackingLabel(label string) bool {
	return label == "fabrik:awaiting-close" || label == "fabrik:awaiting-member-close" ||
		strings.HasPrefix(label, "fabrik:credited-pr:")
}

// snapshotShowsLanding reports whether the snapshot itself says the item's work
// landed: the landing-verification or close-tracking markers, or a merged PR.
func snapshotShowsLanding(st *itemstate.ItemState) bool {
	for _, l := range st.Labels {
		if l == "fabrik:awaiting-landing-verification" || closeTrackingLabel(l) {
			return true
		}
	}
	return st.LinkedPR != nil && st.LinkedPR.Merged
}

// closedEvent builds the closed event. A close that belongs to a landing says so
// (landing=true) and is attributed to fabrik; any other close names the actor
// the change carries (human, bot or unknown) and the board column.
func (ce *channelEvents) closedEvent(key string, st *itemstate.ItemState, c itemstate.Change) channelevents.Event {
	e := ce.e
	ref := issueRef(st.Repo, st.Number)
	ev := e.baseEvent(st, channelevents.Closed)
	landing := ce.isLanding(key) || snapshotShowsLanding(st)
	attr := e.labelAttribute(c)
	if landing {
		attr = labelAttribution{Actor: "fabrik"}
	}
	by := attributionMeta(&ev, attr)
	ev.Meta["column"] = st.Status
	ev.Meta["landing"] = strconv.FormatBool(landing)
	ev.Content = fmt.Sprintf("%s was closed (by %s)", ref, by)
	if st.Status != "" {
		ev.Content += " at " + st.Status
	}
	if landing {
		ev.Content += " as part of its landing"
	}
	return ev
}

// landedEvent builds the merged event shared by the landing hook and the
// label-derived fallback, so both carry the same dedup key and one of them wins.
func (e *Engine) landedEvent(st *itemstate.ItemState, pr int) channelevents.Event {
	ref := issueRef(st.Repo, st.Number)
	ev := e.baseEvent(st, channelevents.Merged)
	if pr != 0 {
		ev.PR = pr
	}
	ev.DedupKey = fmt.Sprintf("merged:%s:%d", ref, ev.PR)
	ev.Content = fmt.Sprintf("%s landed via PR #%d", ref, ev.PR)
	return ev
}

// emitLanded announces a landing from the landing path itself, right after the
// Done move succeeded and before any close attempt, so `merged` precedes every
// downstream close-tracking event (FR-015). pr is the credited PR. The
// label-derived merged (labelDerivedEvents) shares its dedup key and stays as the
// fallback: exactly one is published.
func (e *Engine) emitLanded(item gh.ProjectItem, pr int) {
	ce := e.channelEvents()
	if ce == nil {
		return
	}
	e.hookEvent(item, "merged", func(st *itemstate.ItemState) []channelevents.Event {
		ce.markLanding(issueRef(st.Repo, st.Number))
		return []channelevents.Event{e.landedEvent(st, pr)}
	})
}
