package engine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// decision-pending and settlement-withdrawn (#2120, ADR-2120).
//
// A Suspended item (internal/attention) is waiting for a human decision. The
// deriver announces it once per *episode* with decision-pending, using the same
// persisted episode counters validate-settled uses (hub counters dp:<ref> /
// dpclosed:<ref>, dedup key dp:<ref>:<episode>), so a restart neither replays an
// announced episode nor loses an unannounced one. Unlike the pause family, a
// first sight is not silent: an item that is suspended with no open persisted
// episode is announced once — that is what makes a pending decision visible
// after a restart. A first-seen age is only a lower bound, so an item whose
// GitHub time is not yet known (the poll-side recorder, decision_anchor.go, has
// not reached it) waits decisionAnchorGrace before it is announced "at least
// since".
//
// When an episode ends because the engine took the item back — a rebase is
// needed, new feedback arrived, a worker was dispatched — one
// settlement-withdrawn event says so, with the reason; when a human acted
// (autonomy set, advanced, a blocker cleared) nothing is sent. The same event
// withdraws a validate-settled episode (channel_settled.go).
//
// Everything here reads the store through Peek/Scan only (R9) and is
// observation-only (R10).

// decisionAnchorGrace is how long a suspended item whose GitHub time is unknown
// waits for the recorder before it is announced with a lower-bound age. A var so
// tests can shorten it; real (wall-clock) time, like the debounce.
var decisionAnchorGrace = 3 * time.Minute

// Withdrawal reasons (settlement-withdrawn meta.reason).
const (
	reasonRebaseNeeded     = "rebase-needed"
	reasonNewFeedback      = "new-feedback"
	reasonCIOnNewHead      = "ci-on-new-head"
	reasonWorkerDispatched = "worker-dispatched"
)

// decisionEpisode is the consumer's memory of an open decision-pending episode.
type decisionEpisode struct {
	key    string // the DedupKey of the announced event
	status string // the column the decision was announced in
	kind   string // attention.PendingDecision or PendingParkedBlocker
	// ev is the event as announced (without its dedup key), so a reminder
	// carries exactly the original's attributes and comment link.
	ev channelevents.Event
}

func dpKeys(ref string) (started, closed string) { return "dp:" + ref, "dpclosed:" + ref }

// optionsJSON renders options for event meta (values are strings).
func optionsJSON(opts []attention.Option) string {
	if len(opts) == 0 {
		return ""
	}
	b, err := json.Marshal(opts)
	if err != nil {
		return ""
	}
	return string(b)
}

// decisionEvent builds the decision-pending event for a suspended item without
// touching any per-item memory, so the live deriver and the catch-up snapshot
// share it.
func (e *Engine) decisionEvent(st *itemstate.ItemState, res attention.Result) channelevents.Event {
	p := res.Pending
	ev := e.baseEvent(st, channelevents.DecisionPending)
	ev.Meta["column"] = st.Status
	ev.Meta["completed_stage"] = p.CompletedStage
	ev.Meta["since"] = p.Since.UTC().Format(time.RFC3339)
	ev.Meta["since_basis"] = p.SinceBasis
	ev.Meta["autonomy"] = e.autonomyMode(st)
	if o := optionsJSON(p.Options); o != "" {
		ev.Meta["options"] = o
	}
	switch p.Kind {
	case attention.PendingParkedBlocker:
		ev.Meta["decision"] = "unblock"
		refs := make([]string, 0, len(p.Blockers))
		for _, b := range p.Blockers {
			refs = append(refs, b.Ref)
		}
		ev.Meta["blockers"] = strings.Join(refs, ",")
	default:
		ev.Meta["decision"] = "advance"
		ev.Meta["next_stage"] = p.NextStage
	}
	ev.Content = fmt.Sprintf("%s needs a decision: %s. Options: %s",
		issueRef(st.Repo, st.Number), res.Summary, attention.OptionsText(p.Options))
	return ev
}

// decisionStep advances the item's decision-pending episode and returns the
// events to publish. Consumer goroutine (or seed, before it starts); m is the
// item's memo.
func (ce *channelEvents) decisionStep(ref string, m *itemMemo, st *itemstate.ItemState, res attention.Result) []channelevents.Event {
	if decisionPendingDisabled.Load() {
		return nil
	}
	e := ce.e
	started, closed := dpKeys(ref)
	n := ce.hub.Counter(started)
	open := n > ce.hub.Counter(closed)
	key := started + ":" + strconv.Itoa(n)

	if res.State == attention.Suspended && res.Pending != nil {
		if open && m.dp != nil && (m.dp.status != st.Status || m.dp.kind != res.Pending.Kind) {
			// The debounce swallowed the non-suspended snapshot between two
			// decisions (advanced and completed again, or a pending decision
			// that became a parked blocker): the announced episode no longer
			// describes this wait. End it silently and announce a new one.
			ce.hub.SetCounter(closed, n)
			ce.hub.ClearReminders(key)
			m.dp = nil
			open = false
		}
		if open {
			if m.dp == nil {
				// An episode announced before a restart: adopt it without a
				// re-announcement; the event is rebuilt for reminders.
				ev := e.decisionEvent(st, res)
				if k := latestFabrikComment(st); k.Valid {
					ev.CommentURL = k.V
				}
				m.dp = &decisionEpisode{key: key, status: st.Status, kind: res.Pending.Kind, ev: finishEvent(ev)}
			}
			return nil
		}
		if !res.Pending.SinceExact() {
			if m.dpSeen.IsZero() {
				m.dpSeen = time.Now()
			}
			if time.Since(m.dpSeen) < decisionAnchorGrace {
				return nil
			}
		}
		ep := ce.hub.BumpCounter(started)
		key = started + ":" + strconv.Itoa(ep)
		ev := e.decisionEvent(st, res)
		// The comment link is always attached (not deduplicated against the
		// previous event's), so a reminder can carry the same link.
		if k := latestFabrikComment(st); k.Valid {
			ev.CommentURL = k.V
			m.lastComment = k.V
		}
		ev = finishEvent(ev)
		m.dp = &decisionEpisode{key: key, status: st.Status, kind: res.Pending.Kind, ev: ev}
		out := ev
		out.DedupKey = key
		return []channelevents.Event{out}
	}

	m.dpSeen = time.Time{}
	if !open {
		m.dp = nil
		return nil
	}
	if e.episodeUnhydrated(st, m.dp) && engineTookBack(e, st, m.rebaseSeen) == "" {
		// After a restart a deep field (BlockedBy, comments) is absent until
		// the item's first deep fetch, which makes the item look un-suspended
		// for a moment. That is missing data, not a decision: keep the
		// persisted episode open so it is neither closed nor re-announced.
		return nil
	}
	// The episode ended. A withdrawal is announced only when the engine took the
	// item back; a human acting (or the item moving on) is silent.
	var out []channelevents.Event
	if reason := e.decisionWithdrawalReason(st, m.dp, m.rebaseSeen); reason != "" && !settlementWithdrawalDisabled.Load() {
		ev := e.withdrawnEvent(st, string(channelevents.DecisionPending), reason)
		ev.DedupKey = fmt.Sprintf("swdp:%s:%d", ref, n)
		out = append(out, ce.withComment(m, st, ev))
	}
	ce.hub.SetCounter(closed, n)
	ce.hub.ClearReminders(key)
	m.dp = nil
	return out
}

// episodeUnhydrated reports whether an open episode's item merely lacks deep
// data: it was never deep-fetched this run, and every shallow signal still says
// it is waiting for the same human decision (open, unpaused, same column, no
// autonomy, and either the stage-complete label or fabrik:blocked). The caller
// still ends the episode when the engine took the item back.
func (e *Engine) episodeUnhydrated(st *itemstate.ItemState, dp *decisionEpisode) bool {
	if !st.LastDeepFetchAt.IsZero() || st.IsClosed || st.IsPR {
		return false
	}
	if dp != nil && dp.status != st.Status {
		return false
	}
	if hasLabelStr(st.Labels, "fabrik:paused") {
		return false
	}
	stage := e.stageByName(st.Status)
	if stage == nil || stageAutoAdvances(e.cfg.Yolo, st.Labels, stage) {
		return false
	}
	return hasLabelStr(st.Labels, "fabrik:blocked") || hasLabelStr(st.Labels, "stage:"+st.Status+":complete")
}

// decisionWithdrawalReason says why the engine took a pending decision back, or
// "" when the episode simply ended (a human decided, the item moved, it was
// closed, paused or blocked).
func (e *Engine) decisionWithdrawalReason(st *itemstate.ItemState, dp *decisionEpisode, rebaseSeen bool) string {
	if st.IsClosed || st.IsPR {
		return ""
	}
	if dp != nil && (st.Status != dp.status || dp.kind != attention.PendingDecision) {
		return ""
	}
	if hasLabelStr(st.Labels, "fabrik:paused") || hasLabelStr(st.Labels, "fabrik:blocked") {
		return ""
	}
	stage := e.stageByName(st.Status)
	if stage == nil || stageAutoAdvances(e.cfg.Yolo, st.Labels, stage) {
		return "" // a human set an autonomy label
	}
	if dp == nil && !hasLabelStr(st.Labels, "stage:"+st.Status+":complete") && !hasReworkingLabel(st.Labels) {
		return ""
	}
	return engineTookBack(e, st, rebaseSeen)
}

func hasReworkingLabel(labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, reworkingLabelPrefix) {
			return true
		}
	}
	return false
}

// engineTookBack names why an item that was waiting for a human no longer is,
// for a reason on the closed list, or "".
func engineTookBack(e *Engine, st *itemstate.ItemState, rebaseSeen bool) string {
	switch {
	case hasLabelStr(st.Labels, "fabrik:rebase-needed") || rebaseSeen:
		return reasonRebaseNeeded
	case st.Worker != nil || hasReworkingLabel(st.Labels) || hasLabelStr(st.Labels, "fabrik:editing"):
		return reasonWorkerDispatched
	case e.cachedPendingFeedbackOf(st).any():
		return reasonNewFeedback
	}
	return ""
}

// withdrawnEvent builds a settlement-withdrawn event. withdrew is the event
// type whose announcement no longer holds.
func (e *Engine) withdrawnEvent(st *itemstate.ItemState, withdrew, reason string) channelevents.Event {
	ev := e.baseEvent(st, channelevents.SettlementWithdrawn)
	ev.Meta["withdrew"] = withdrew
	ev.Meta["reason"] = reason
	if lpr := st.LinkedPR; lpr != nil && lpr.HeadSHA != "" {
		ev.Meta["head_sha"] = lpr.HeadSHA
	}
	why := map[string]string{
		reasonRebaseNeeded:     "the PR no longer merges cleanly and needs a rebase",
		reasonNewFeedback:      "new feedback arrived that must be addressed first",
		reasonCIOnNewHead:      "the PR head moved, so CI must run again on the new head",
		reasonWorkerDispatched: "the engine started working on it again",
	}[reason]
	ev.Content = fmt.Sprintf("%s is no longer ready (%s): %s. The earlier %s event no longer holds; wait for a fresh one before acting on it",
		issueRef(st.Repo, st.Number), reason, why, withdrew)
	return ev
}

// remindDecisions re-announces still-open decision-pending episodes to the
// subscriptions that asked for reminders (R6a). Consumer goroutine, on the tick.
func (ce *channelEvents) remindDecisions() {
	if !ce.hub.RemindersEnabled() {
		return
	}
	for ref, m := range ce.memo {
		if m.dp == nil || m.dp.key == "" {
			continue
		}
		repo, n, ok := parseIssueRef(ref)
		if !ok {
			continue
		}
		snap, ok := ce.e.store.Peek(repo, n)
		if !ok {
			continue
		}
		st := snap.State()
		if st.IsClosed {
			continue
		}
		ce.hub.PublishReminders(m.dp.ev, m.dp.key)
	}
}
