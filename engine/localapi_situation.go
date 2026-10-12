package engine

import (
	"sort"
	"time"

	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// The situation view (#2171, ADR 2171): one rolled-up "what needs you" answer.
// buildSituation is the only builder; fabrik_situation, the requested and
// scheduled `situation` digest and the attach/reconnect catch-up all call it, so
// the three can never drift apart. It is a second *view*, not a second
// classifier: every item is placed by internal/attention's own (State, Code), the
// gate holds and retry facts come from the same anomalyState the item-wedged and
// retry-starved events read, and poll, worker and merge-train facts come from
// Health's own sources.
//
// It reads the Store (Scan/Peek), the health mirror and the anomaly memory only —
// never GitHub (R9): TestLocalAPISourceDoesNotReachGitHub scans this file.

// situationSectionMax bounds each section (R4). Ten is the order of the
// five-entry snapshot that motivated the view, with headroom.
const situationSectionMax = 10

// situationScope narrows the item sections and item-scoped anomalies to what a
// subscriber's subscriptions select. A nil scope selects everything. The account
// sections (merge train, health, pipeline anomalies) are never narrowed.
type situationScope func(channelevents.Event) bool

// situationItem is one placed item before bounding.
type situationItem struct {
	entry   localapi.SituationEntry
	repo    string
	number  int
	heldFor time.Duration // gates only; negative = unknown
}

// buildSituation builds the view as of now.
func (e *Engine) buildSituation(scope situationScope) localapi.SituationView {
	now := e.now()
	lb := localAPIBackend{e: e}
	threshold := lb.stallThreshold(0)
	usageLimited := lb.usageLimitedUntil(now)

	// Gate holds are read before the Scan (their own lock, never nested in the
	// store's); a hold already announced as item-wedged is reported under
	// anomalies, so an item is not listed twice.
	holds := map[string]wedgeHold{}
	wedged := map[string]bool{}
	for _, h := range e.gateHolds() {
		if h.announced {
			wedged[h.ref] = true
			continue
		}
		if cur, ok := holds[h.ref]; !ok || h.firstHeld.Before(cur.firstHeld) {
			holds[h.ref] = h
		}
	}

	// Blockers are resolved from an index built by its own Scan: resolving them
	// with Peek inside the visitor below would nest the store's read lock.
	look := e.scanBlockerIndex()

	var merge, gates, human, decisions, stalled []situationItem
	unclassified := 0
	e.store.Scan(func(st *itemstate.ItemState) {
		if st.IsPR || st.IsClosed {
			return
		}
		if !st.LabelsKnown {
			if s := e.stageByName(st.Status); s != nil && !s.Unmanaged && !s.CleanupWorktree {
				unclassified++
			}
			return
		}
		if scope != nil && !scope(e.baseEvent(st, channelevents.Situation)) {
			return
		}
		res := attention.Classify(e.attentionInput(st, now, threshold, usageLimited, look))
		ref := issueRef(st.Repo, st.Number)
		it := situationItem{repo: st.Repo, number: st.Number, heldFor: -1}
		it.entry = localapi.SituationEntry{
			Issue:               ref,
			Title:               st.Title,
			URL:                 knownString(st.URL),
			State:               string(res.State),
			Code:                res.Code,
			Reason:              res.Summary,
			Rank:                res.Rank,
			Options:             optionInfos(res.Options),
			LatestFabrikComment: latestFabrikComment(st),
		}
		switch {
		case res.State == attention.Suspended:
			it.entry.Pending = pendingInfo(res.Pending, now)
			decisions = append(decisions, it)
		case res.Code == attention.CodeAwaitingMergeDecision:
			it.entry.Merge = situationMerge(st)
			merge = append(merge, it)
		case res.State == attention.NeedsHuman || res.State == attention.Escalated:
			it.entry.Human = &localapi.SituationHuman{ResumeVia: res.ResumeVia}
			human = append(human, it)
		case res.State == attention.Stalled:
			stalled = append(stalled, it)
		case res.State == attention.Waiting || res.State == attention.Idle:
			if g, ok := situationGate(st, res, holds[ref], mapHas(holds, ref), wedged[ref], now); ok {
				it.entry.Gate = g
				if g.HeldSeconds.Valid {
					it.heldFor = time.Duration(g.HeldSeconds.V) * time.Second
				}
				gates = append(gates, it)
			}
		}
	})

	view := localapi.SituationView{UnclassifiedItems: unclassified}
	view.MergeDecisions = boundItems(merge, byRankThenRef)
	view.Decisions = boundItems(decisions, byRankThenRef)
	view.HumanInput = boundItems(human, byRankThenRef)
	view.Stalled = boundItems(stalled, byRankThenRef)
	view.Gates = boundItems(gates, byHeldThenRef)

	// Account sections: whole-account, from Health's own sources.
	health, _ := lb.Health(localapi.HealthParams{})
	view.Health = localapi.SituationHealth{Poll: health.Poll, Workers: health.Workers, Claude: health.Claude}
	parts := append([]localapi.TrainPartition(nil), health.MergeTrain...)
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].Stalled && !parts[j].Stalled })
	for _, p := range parts {
		view.Health.TrainQueued += len(p.Members)
	}
	view.MergeTrain = localapi.SituationSection[localapi.TrainPartition]{Entries: []localapi.TrainPartition{}}
	view.MergeTrain.Entries, view.MergeTrain.Omitted = boundSlice(parts)

	anoms := e.situationAnomalies(health, scope, now)
	view.Anomalies = localapi.SituationSection[localapi.SituationAnomaly]{Entries: []localapi.SituationAnomaly{}}
	view.Anomalies.Entries, view.Anomalies.Omitted = boundSlice(anoms)

	view.NothingNeedsYou = unclassified == 0 && len(anoms) == 0 &&
		len(merge)+len(gates)+len(human)+len(decisions)+len(stalled) == 0
	return view
}

func mapHas[V any](m map[string]V, k string) bool { _, found := m[k]; return found }

// situationMerge is the merge decision's PR and verdicts, from the cache only.
func situationMerge(st *itemstate.ItemState) *localapi.SituationMerge {
	m := &localapi.SituationMerge{PR: localapi.Unknown[int](), Review: localapi.Unknown[string](), CI: localapi.Unknown[string]()}
	lpr := st.LinkedPR
	if lpr == nil || lpr.Number == 0 {
		return m
	}
	m.PR = localapi.Some(lpr.Number)
	m.Review = localapi.Some(reviewSummary(lpr))
	if v := cachedCIVerdict(lpr); v != "unknown" {
		m.CI = localapi.Some(v)
	}
	return m
}

// situationGate names the gate holding an item. A hold the engine recorded gives
// the gate, its reason and its start; otherwise a label wait (awaiting-ci,
// awaiting-review) names the gate and starts at the label's applied time when the
// daemon holds one. A hold start the daemon cannot vouch for is unknown, never
// "now". An item whose hold is already announced as item-wedged is not a gate
// entry (its anomaly carries the gate).
func situationGate(st *itemstate.ItemState, res attention.Result, h wedgeHold, haveHold, wedged bool, now time.Time) (*localapi.SituationGate, bool) {
	if wedged {
		return nil, false
	}
	g := &localapi.SituationGate{HeldSince: localapi.Unknown[time.Time](), HeldSeconds: localapi.Unknown[int64]()}
	switch {
	case haveHold:
		g.Gate, g.Reason = h.gate, h.reason
		g.HeldSince = localapi.KnownTime(h.firstHeld)
		g.HeldSeconds = ageSince(now, h.firstHeld)
	case res.Code == "fabrik:awaiting-ci" || res.Code == "fabrik:awaiting-review":
		if res.Code == "fabrik:awaiting-ci" {
			g.Gate = gateCI
		} else {
			g.Gate = gateReview
		}
		g.Reason = res.Summary
		if at := st.LabelAppliedAt[res.Code]; !at.IsZero() {
			g.HeldSince = localapi.KnownTime(at)
			g.HeldSeconds = ageSince(now, at)
		}
	default:
		return nil, false
	}
	return g, true
}

func byRankThenRef(a, b situationItem) bool {
	if a.entry.Rank != b.entry.Rank {
		return a.entry.Rank < b.entry.Rank
	}
	// Suspended shares needs-human's rank: within it a pause that needs an
	// answer sorts ahead of a decision that merely waits (as the board view does).
	if as, bs := a.entry.State == string(attention.Suspended), b.entry.State == string(attention.Suspended); as != bs {
		return !as
	}
	return byRef(a, b)
}

func byRef(a, b situationItem) bool {
	if a.repo != b.repo {
		return a.repo < b.repo
	}
	return a.number < b.number
}

// byHeldThenRef orders gates longest-held first; an unknown hold time sorts last.
func byHeldThenRef(a, b situationItem) bool {
	if (a.heldFor >= 0) != (b.heldFor >= 0) {
		return a.heldFor >= 0
	}
	if a.heldFor != b.heldFor {
		return a.heldFor > b.heldFor
	}
	return byRef(a, b)
}

// boundItems orders items, then cuts the list at situationSectionMax before any
// rendering, with the exact number cut.
func boundItems(items []situationItem, less func(a, b situationItem) bool) localapi.SituationSection[localapi.SituationEntry] {
	sort.SliceStable(items, func(i, j int) bool { return less(items[i], items[j]) })
	sec := localapi.SituationSection[localapi.SituationEntry]{Entries: []localapi.SituationEntry{}}
	kept, omitted := boundSlice(items)
	for _, it := range kept {
		sec.Entries = append(sec.Entries, it.entry)
	}
	sec.Omitted = omitted
	return sec
}

// boundSlice keeps the first situationSectionMax elements and reports how many
// were cut.
func boundSlice[T any](in []T) ([]T, int) {
	if len(in) == 0 {
		return []T{}, 0 // an explicit empty list on the wire, never null
	}
	if len(in) <= situationSectionMax {
		return in, 0
	}
	return in[:situationSectionMax], len(in) - situationSectionMax
}

// situationAnomalies lists the anomalies active now: a pipeline stall (the poll
// loop, then each stalled merge-train partition, from the same verdicts the
// pipeline-stalled event reads), the open retry-starved and item-wedged episodes
// and the cache divergences inside their window. Item-scoped ones respect scope.
func (e *Engine) situationAnomalies(health *localapi.HealthResult, scope situationScope, now time.Time) []localapi.SituationAnomaly {
	var out []localapi.SituationAnomaly
	if health.Poll.Overdue.Valid && health.Poll.Overdue.V {
		out = append(out, localapi.SituationAnomaly{
			Kind: localapi.AnomalyPipelineStalled, Scope: "poll", Subject: "poll", URL: localapi.Unknown[string](),
			Summary: "the poll loop is overdue (" + health.Poll.OverdueCause + ")",
			Since:   health.Poll.CurrentStartedAt,
		})
	}
	for _, p := range health.MergeTrain {
		if !p.Stalled {
			continue
		}
		out = append(out, localapi.SituationAnomaly{
			Kind: localapi.AnomalyPipelineStalled, Scope: "merge-train", Subject: mergeTrainKey(p.Repo, p.Base), URL: localapi.Unknown[string](),
			Summary: "merge train has members queued and no run started" + idleSuffix(p.IdleReason),
			Since:   p.OldestQueuedSince,
		})
	}

	retries, holds, divergences := e.openAnomalyFacts(now)
	item := func(kind, repo string, n int, summary string, since time.Time) {
		snap, found := e.store.Peek(repo, n)
		if !found {
			return
		}
		st := snap.State()
		if scope != nil && !scope(e.baseEvent(&st, channelevents.Situation)) {
			return
		}
		out = append(out, localapi.SituationAnomaly{
			Kind: kind, Scope: "item", Subject: issueRef(repo, n), Title: st.Title, URL: knownString(st.URL),
			Summary: summary, Since: localapi.KnownTime(since),
		})
	}
	for i := range holds {
		h := &holds[i]
		item(localapi.AnomalyItemWedged, h.repo, h.number, "the "+h.gate+" gate has held its advance or landing with no worker dispatched: "+h.reason, h.firstHeld)
	}
	for i := range retries {
		f := &retries[i]
		since := f.starvedSince
		if since.IsZero() {
			since = f.due
		}
		item(localapi.AnomalyRetryStarved, f.repo, f.number, "the "+f.stage+" retry after "+f.cause+" was due and has not been dispatched: "+f.reason, since)
	}
	for i := range divergences {
		d := &divergences[i]
		item(localapi.AnomalyCacheDivergence, d.repo, d.number, "the engine wrote "+d.what+" ("+d.written+") but a "+d.path+" reports "+d.observed, d.at)
	}
	return out
}

func idleSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

// Situation serves fabrik_situation: the account-wide, unscoped view, with the
// daemon's freshness envelope. A push request queues the same view to the
// caller's subscriber as a `situation` digest (see channel_situation.go).
func (b localAPIBackend) Situation(p localapi.SituationParams) (*localapi.SituationResult, error) {
	res := &localapi.SituationResult{Envelope: b.envelope(b.e.now()), View: b.e.buildSituation(nil)}
	if p.Push {
		res.Pushed, res.PushNote = b.e.requestSituationPush(p.Subscriber, p.Force)
	}
	return res, nil
}
