package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// channelStallThreshold is the one stall threshold the read API's attention
// view uses. A subscription cannot override it, so a stalled push can never be
// looser than the view it mirrors.
func (e *Engine) channelStallThreshold() time.Duration {
	return localAPIBackend{e: e}.stallThreshold(0)
}

func (e *Engine) channelUsageLimitedUntil(now time.Time) time.Time {
	d, _ := e.claudeUsageLimitedUntilTime(now)
	return d
}

// pauseFamilyKey maps the attention classification onto the pause-family
// events. "" is none. The mapping is the classifier's own: paused and
// awaiting-input are its needs-human state, escalated and stalled its states of
// the same name, so these events can never disagree with fabrik_board. A
// landing-verification failure is a separate event, and a cruise item waiting
// for a merge decision is validate-settled's business, not a pause.
func pauseFamilyKey(res attention.Result) string {
	switch res.State {
	case attention.NeedsHuman:
		switch res.Code {
		case attention.CodeAwaitingMergeDecision:
			return ""
		case "fabrik:awaiting-input":
			return "awaiting-input"
		default:
			return "paused"
		}
	case attention.Escalated:
		if res.Code == "fabrik:landing-verification-failed" {
			return ""
		}
		return "escalated"
	case attention.Stalled:
		return "stalled"
	}
	return ""
}

func parseIssueRef(ref string) (repo string, n int, ok bool) {
	i := strings.LastIndex(ref, "#")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(ref[i+1:])
	return ref[:i], n, err == nil
}

// withComment attaches the latest cached 🏭 comment link when it is not the
// one the previous event for this item already carried.
func (ce *channelEvents) withComment(m *itemMemo, st *itemstate.ItemState, ev channelevents.Event) channelevents.Event {
	if k := latestFabrikComment(st); k.Valid && k.V != m.lastComment {
		ev.CommentURL = k.V
		m.lastComment = k.V
	}
	return ev
}

// evaluateItem re-derives the state-based events for one item after its
// debounce elapsed. Items the store no longer holds are skipped (Peek, never
// Get: a miss must not trigger a GitHub fetch).
func (ce *channelEvents) evaluateItem(key string, d *dirtyItem) {
	repo, n, ok := parseIssueRef(key)
	if !ok {
		return
	}
	snap, ok := ce.e.store.Peek(repo, n)
	if !ok {
		return
	}
	st := snap.State()
	ce.enqueueDerived(ce.announceArrival(key, &st))
	// The memo is not created here: derive() treats "no memo" as a first sight
	// and records the silent baseline.
	if m := ce.memo[key]; m != nil {
		m.rebaseSeen = d != nil && d.rebaseSeen
		defer func() { m.rebaseSeen = false }()
	}
	ce.closeSettleEpisode(key, &st)
	ce.enqueueDerived(ce.derive(key, &st, d))
}

// enqueueDerived publishes events derived on the consumer goroutine. It runs on
// that goroutine already, so it appends straight to the hub rather than the
// queue (which would only defer them to the next loop turn).
func (ce *channelEvents) enqueueDerived(evs []channelevents.Event) {
	for _, ev := range evs {
		ce.hub.Publish(finishEvent(ev))
	}
}

// derive computes the pause-family, awaiting-input-stale and cycle-limit-near
// events for one item. Consumer goroutine only (it owns the memo).
func (ce *channelEvents) derive(key string, st *itemstate.ItemState, d *dirtyItem) []channelevents.Event {
	e := ce.e
	// A label-less record (probe-seeded) carries no usable classification: wait
	// for the first label-bearing population, which then baselines silently
	// (the restart-flood fix, #2123).
	if st.IsPR || !st.LabelsKnown {
		return nil
	}
	now := e.now()
	res := attention.Classify(e.attentionInput(st, now, e.channelStallThreshold(), e.channelUsageLimitedUntil(now), e.peekBlockerLookup()))
	m, known := ce.memo[key]
	if !known {
		// First sight of this item (a board population, or an item created
		// after startup): record the baseline silently — an item's existing
		// state is not a transition.
		m = ce.memoFor(key)
		m.pauseKey = pauseFamilyKey(res)
		m.staleDone = hasLabelStr(st.Labels, "fabrik:awaiting-input")
		ce.cycleNearEvents(m, st, true)
		// Unlike the pause family, a pending decision is announced at first sight
		// (once per episode): that is what makes it visible after a restart.
		return ce.decisionStep(key, m, st, res)
	}
	var out []channelevents.Event

	// Pause family: one event per transition into the state.
	if k := pauseFamilyKey(res); k != m.pauseKey {
		m.pauseKey = k
		if k != "" {
			out = append(out, ce.pauseEvent(m, st, k, res, now))
		}
	}

	// awaiting-input-stale: the label was present when an invocation was
	// recorded, that run did not emit FABRIK_BLOCKED_ON_INPUT, and the engine
	// did not pause the item itself. Once per awaiting-input episode.
	hasAwaiting := hasLabelStr(st.Labels, "fabrik:awaiting-input")
	if !hasAwaiting {
		m.staleDone = false
	} else if d != nil && d.staleCandidate && !m.staleDone &&
		!st.LastInvocationBlocked && !st.StageState.PausedByEngine[st.Status] {
		m.staleDone = true
		ev := e.baseEvent(st, channelevents.AwaitingInputStale)
		ev.Content = fmt.Sprintf("%s still carries fabrik:awaiting-input, but its latest run finished without asking for input; the label may be stale", issueRef(st.Repo, st.Number))
		out = append(out, ce.withComment(m, st, ev))
	}

	out = append(out, ce.cycleNearEvents(m, st, false)...)
	out = append(out, ce.decisionStep(key, m, st, res)...)
	return out
}

func (ce *channelEvents) pauseEvent(m *itemMemo, st *itemstate.ItemState, key string, res attention.Result, now time.Time) channelevents.Event {
	return ce.withComment(m, st, ce.e.pauseEventFor(st, key, res, now))
}

// pauseEventFor builds the pause-family event for an item without touching any
// per-item memory, so the live deriver and the catch-up snapshot share it.
func (e *Engine) pauseEventFor(st *itemstate.ItemState, key string, res attention.Result, now time.Time) channelevents.Event {
	var typ channelevents.EventType
	var lead string
	switch key {
	case "awaiting-input":
		typ, lead = channelevents.AwaitingInput, "is waiting for your reply"
	case "escalated":
		typ, lead = channelevents.Escalated, "was escalated: the engine stopped and needs a human"
	case "stalled":
		typ, lead = channelevents.Stalled, "has stalled"
	default:
		typ, lead = channelevents.Paused, "is paused"
	}
	ev := e.baseEvent(st, typ)
	ev.Content = fmt.Sprintf("%s %s: %s", issueRef(st.Repo, st.Number), lead, res.Summary)
	ev.Meta["code"] = res.Code
	ev.Meta["reason"] = res.Summary
	// The same cause fabrik_status reports for this state (#2121); absent when
	// the engine does not know one, never guessed.
	if res.Cause != "" {
		ev.Meta["cause"] = res.Cause
	}
	// A pause-family event that waits on a human says how to act, in the words
	// of the engine's own pause comment (R-options-1/2): never a generic resume
	// method that contradicts it.
	if o := optionsJSON(res.Options); o != "" {
		ev.Meta["options"] = o
	}
	if res.ResumeVia != "" {
		ev.Meta["resume"] = res.ResumeVia
	}
	if !res.ProgressAt.IsZero() {
		ev.Meta["progress_age_seconds"] = strconv.FormatInt(secs(now.Sub(res.ProgressAt)), 10)
	}
	return ev
}

// cycleNearEvents fires cycle-limit-near when a counter reaches limit-1, once
// per approach: the memory re-arms when the counter drops back below limit-1
// (a no-op review cycle is decremented). seed records the baseline silently.
// Attempts are excluded (they fail the stage rather than pause it), and an
// unknown or too-small limit never fires — zero config must not read as "limit 0".
func (ce *channelEvents) cycleNearEvents(m *itemMemo, st *itemstate.ItemState, seed bool) []channelevents.Event {
	var out []channelevents.Event
	for _, kind := range counterKinds {
		if kind == "attempts" {
			continue
		}
		lim := ce.e.limitFor(kind)
		if lim.Kind != "limit" || lim.Value < 2 {
			continue
		}
		n := counterValue(st.StageState, kind, st.Status)
		k := kind + "@" + st.Status
		switch {
		case seed:
			m.near[k] = n >= lim.Value-1
		case n >= lim.Value:
			m.near[k] = true
		case n == lim.Value-1 && n >= 1:
			if !m.near[k] {
				m.near[k] = true
				ev := ce.e.baseEvent(st, channelevents.CycleLimitNear)
				ev.Content = fmt.Sprintf("%s is one %s short of its limit (%d of %d)", issueRef(st.Repo, st.Number), kind, n, lim.Value)
				ev.Meta["counter"] = kind
				ev.Meta["count"] = strconv.Itoa(n)
				ev.Meta["limit"] = strconv.Itoa(lim.Value)
				out = append(out, ce.withComment(m, st, ev))
			}
		default:
			m.near[k] = false
		}
	}
	return out
}

func (ce *channelEvents) armCounters(m *itemMemo, st *itemstate.ItemState, seed bool) {
	ce.cycleNearEvents(m, st, seed)
}

// onTick runs the time-driven checks: stalled (the same classifier as the read
// API, over every cached item) and the lazily lifted Claude usage-limit hold.
func (ce *channelEvents) onTick() {
	e := ce.e
	now := e.now()
	threshold := e.channelStallThreshold()
	usageLimited := e.channelUsageLimitedUntil(now)
	look := e.scanBlockerIndex()
	var out []channelevents.Event
	e.store.Scan(func(st *itemstate.ItemState) {
		if st.IsPR || !st.LabelsKnown {
			return
		}
		key := issueRef(st.Repo, st.Number)
		m := ce.memo[key]
		if m == nil {
			// First sight of an item outside an observed change: baseline only.
			// Same baseline derive() records, so a later non-seed evaluation cannot
			// read an item already at limit-1 as a fresh transition.
			m = ce.memoFor(key)
			res := attention.Classify(e.attentionInput(st, now, threshold, usageLimited, look))
			m.pauseKey = pauseFamilyKey(res)
			m.staleDone = hasLabelStr(st.Labels, "fabrik:awaiting-input")
			ce.cycleNearEvents(m, st, true)
			out = append(out, ce.decisionStep(key, m, st, res)...)
			return
		}
		res := attention.Classify(e.attentionInput(st, now, threshold, usageLimited, look))
		if k := pauseFamilyKey(res); k != m.pauseKey {
			m.pauseKey = k
			if k != "" {
				out = append(out, ce.pauseEvent(m, st, k, res, now))
			}
		}
		// The tick also drives the one time-based part of the decision step: the
		// grace before a lower-bound announcement.
		out = append(out, ce.decisionStep(key, m, st, res)...)
	})
	ce.enqueueDerived(out)
	ce.remindDecisions()
	ce.checkClaudeLimit()
	ce.checkPipeline()
	ce.checkAnomalies()
	ce.publishSituations()
}

// checkClaudeLimit emits claude-usage-limited / -lifted on the edge of the
// account-wide usage-limit hold, including a lift that happens only by the deadline
// passing (which has no transition call in the engine).
func (ce *channelEvents) checkClaudeLimit() {
	e := ce.e
	now := e.now()
	until := e.channelUsageLimitedUntil(now)
	limited := !until.IsZero()
	if limited == ce.claudeLimited {
		return
	}
	ce.claudeLimited = limited
	ev := channelevents.Event{Type: channelevents.ClaudeUsageLimitLifted, At: now, Meta: map[string]string{}}
	if limited {
		ev.Type = channelevents.ClaudeUsageLimited
		ev.Content = fmt.Sprintf("Claude usage limit hit: dispatch held until %s", until.UTC().Format(time.RFC3339))
		ev.Meta["until"] = until.UTC().Format(time.RFC3339)
	} else {
		ev.Content = "Claude usage limit lifted: dispatch resumes"
	}
	ce.enqueueDerived([]channelevents.Event{ev})
}
