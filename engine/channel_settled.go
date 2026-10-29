package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// validate-settled (#1968 R2, ADR-1966-b).
//
// The label stage:Validate:complete is not "gate-complete": under a Validate
// stage with wait_for_ci and wait_for_reviews it is applied first, fabrik:
// awaiting-ci is then removed, and only afterwards does the review gate run and
// (when reviewers are outstanding) apply fabrik:awaiting-review. A predicate
// over labels alone would therefore fire early. So the anchor is the engine's
// own definition of settled: runCatchUpPhase2 is reached for an item only when
// no Phase 1 handler (dependencies, review gate, auto-merge convergence, merge
// and CI gates) claimed it. noteValidateSettled, called at the very top of that
// function — ahead of the autonomy gate, because the headline cruise case
// returns there — merely records "this item reached the settle point"; it
// changes no return value and no decision (R10). The consumer then confirms
// with a cache-only predicate and emits once per episode.

// noteValidateSettled records that item reached the engine's settle point
// (runCatchUpPhase2). See recordValidateSettle.
func (e *Engine) noteValidateSettled(item gh.ProjectItem, stage *stages.Stage) {
	if l := e.captureValidateSettle(item, stage, false); l.ok {
		// A yolo item acts on its own: the landing decision announces it only once
		// the merge or enqueue went through, never ahead of that attempt. Anything
		// that waits for a human is settled right here.
		if l.cand.ev.Meta["next"] == "waiting-for-human" {
			l.finish(true)
		}
	}
}

// noteValidateLanding records that item reached the engine's landing decision
// (attemptMergeOnValidate, after its dependency, review-thread, review and
// feedback gates passed). A yolo item is merged or queued inside that call —
// in the CI-gated flow from checkCIGate's Phase 1 handler, which claims the
// item so Phase 2 is never reached, and in the ungated flow from
// handleStageComplete before the complete label is even applied — so this is
// the settle point for yolo. The gates were just evaluated live by the engine,
// so the cache-only predicate is relaxed to the identity checks.
//
// The event is captured here, before the landing call (a successful queue move
// takes the item off Validate), but it is only announced by the returned
// handle's finish(true), after the merge or enqueue succeeded. A landing that
// fails — a conflict that leads to fabrik:rebase-needed, an enqueue error —
// announces nothing, so the overseer is never told "will auto-merge" for a merge
// that did not happen and the re-settle after the rebase is a fresh episode.
func (e *Engine) noteValidateLanding(item gh.ProjectItem, stage *stages.Stage) settleLanding {
	return e.captureValidateSettle(item, stage, true)
}

// settleCandidate is one settle point captured on the poll goroutine.
type settleCandidate struct {
	ev      channelevents.Event
	relaxed bool
}

// settleLanding is a captured settle point waiting for its landing outcome. The
// zero value (nothing captured, no hub) is a no-op.
type settleLanding struct {
	e    *Engine
	key  string
	cand settleCandidate
	ok   bool
}

// finish announces the captured settle point when landed is true and drops it
// otherwise. A void call: it never alters a decision (R10).
func (l settleLanding) finish(landed bool) {
	if !l.ok || !landed {
		return
	}
	ce := l.e.channelEvents()
	if ce == nil {
		return
	}
	ce.mu.Lock()
	if ce.settleCand == nil {
		ce.settleCand = map[string]settleCandidate{}
	}
	if prev, held := ce.settleCand[l.key]; !held || (prev.relaxed && !l.cand.relaxed) {
		ce.settleCand[l.key] = l.cand
	}
	ce.mu.Unlock()
	ce.signal()
}

// captureValidateSettle captures the event as it stands when the cache-only
// predicate holds right now. The event is built here, on the calling
// goroutine, rather than later on the consumer: a yolo item can be merged and
// moved to Done within the very same pass, and an evaluation deferred past
// that would find it no longer at Validate and silently drop the headline
// event. Everything here is a store Peek plus pure CPU — no GitHub call, no
// engine decision touched (R9, R10). A void, non-blocking call, safe with no
// hub running.
func (e *Engine) captureValidateSettle(item gh.ProjectItem, stage *stages.Stage, atLanding bool) settleLanding {
	if stage == nil || stage.Name != "Validate" {
		return settleLanding{}
	}
	ce := e.channelEvents()
	if ce == nil {
		return settleLanding{}
	}
	defer func() {
		if r := recover(); r != nil {
			e.logf(item.Number, "channel", "validate-settled hook panic recovered: %v\n", r)
		}
	}()
	repo := itemOwnerRepoString(item, e.defaultRepo())
	snap, ok := e.store.Peek(repo, item.Number)
	if !ok {
		return settleLanding{}
	}
	st := snap.State()
	if !e.validateSettledSnap(snap, &st, atLanding) {
		return settleLanding{}
	}
	ev := e.validateSettledEvent(snap, &st, stage)
	return settleLanding{e: e, key: issueRef(repo, item.Number), cand: settleCandidate{ev: ev, relaxed: atLanding}, ok: true}
}

// cachedFeedback is the cache-only count of unprocessed feedback on an item.
type cachedFeedback struct {
	Comments, Threads, Bodies int
}

func (f cachedFeedback) any() bool { return f.Comments+f.Threads+f.Bodies > 0 }

// cachedPendingFeedback answers the feedback gate's question — is anything
// unprocessed on this item? — from the store alone. It applies the gate's own
// definitions through the shared pure cores (filterNewComments,
// filterReviewThreadComments + dropOutdatedThreadComments, reviewBodyCandidates)
// so the two cannot drift; the only difference is that the durable
// review-ids-addressed marker (#1555) is looked up in the cached comments
// rather than fetched live. TestCachedFeedbackMatchesLiveGate pins the
// agreement.
func (e *Engine) cachedPendingFeedback(snap itemstate.Snapshot, st *itemstate.ItemState) cachedFeedback {
	return e.cachedPendingFeedbackOf(st)
}

// cachedPendingFeedbackOf is cachedPendingFeedback over the item state alone
// (the processed-comment map is part of it), so attentionInput can ask it from
// inside a Store.Scan visitor, where no Snapshot is at hand.
func (e *Engine) cachedPendingFeedbackOf(st *itemstate.ItemState) cachedFeedback {
	processed := func(id string) bool { return !st.StageState.ProcessedComments[id].IsZero() }
	var f cachedFeedback
	f.Comments = len(filterNewComments(st.Comments, processed))
	if lpr := st.LinkedPR; lpr != nil {
		f.Threads = len(dropOutdatedThreadComments(filterReviewThreadComments(lpr.ThreadComments, processed)))
		cands := reviewBodyCandidates(lpr.Reviews, processed, e.cleanReviewSkipper(st.Status, st.Number, st.Labels))
		if len(cands) > 0 {
			durable := e.cachedAddressedReviewIDs(st)
			for _, c := range cands {
				if !durable[c.review.DatabaseID] {
					f.Bodies++
				}
			}
		}
	}
	return f
}

// cachedAddressedReviewIDs is durablyAddressedReviewIDs over the cached
// comments: only Fabrik's own comments count, so a stranger's comment naming a
// review ID cannot suppress its feedback.
func (e *Engine) cachedAddressedReviewIDs(st *itemstate.ItemState) map[int]bool {
	addressed := map[int]bool{}
	self := e.selfLogin()
	for _, c := range st.Comments {
		if c.Author != self {
			continue
		}
		for _, id := range parseReviewIDsAddressedMarker(c.Body) {
			addressed[id] = true
		}
	}
	return addressed
}

// validateSettledSnap reports whether the item currently satisfies the
// cache-only validate-settled predicate. It is only consulted after the engine
// itself reached a settle point, so it checks the remaining cache-visible
// conditions rather than re-deriving the gates. atLanding relaxes it to the
// identity checks (Validate, open, an open PR, not paused or blocked): the
// landing decision just evaluated every gate live, and the item still has its
// worker and may not yet carry the complete label.
func (e *Engine) validateSettledSnap(snap itemstate.Snapshot, st *itemstate.ItemState, atLanding bool) bool {
	if st.IsPR || st.IsClosed || st.Status != "Validate" {
		return false
	}
	for _, l := range []string{"fabrik:paused", "fabrik:blocked", "fabrik:rebase-needed", "fabrik:revalidate", "fabrik:awaiting-landing-verification"} {
		if hasLabelStr(st.Labels, l) {
			return false
		}
	}
	lpr := st.LinkedPR
	if lpr == nil || lpr.Number == 0 || lpr.Merged || lpr.State == "closed" {
		return false
	}
	if atLanding {
		return true
	}
	if st.Worker != nil || !hasLabelStr(st.Labels, "stage:Validate:complete") {
		return false
	}
	for _, l := range []string{"fabrik:awaiting-ci", "fabrik:awaiting-review", "fabrik:bot-reprompted"} {
		if hasLabelStr(st.Labels, l) {
			return false
		}
	}
	for _, d := range st.BlockedBy {
		if !strings.EqualFold(d.State, "CLOSED") {
			return false
		}
	}
	return !e.cachedPendingFeedback(snap, st).any()
}

// validateSettledEpisodeEnded reports whether the conditions that end a settle
// episode hold: the complete label is gone (fabrik:revalidate removes it), the
// item left Validate or was closed, a revalidate was requested, or the PR head
// moved after Validate completed. A reopen is a close followed by an open, so
// closing already ended the episode.
func validateSettledEpisodeEnded(st *itemstate.ItemState) bool {
	if st.IsClosed || st.Status != "Validate" || !hasLabelStr(st.Labels, "stage:Validate:complete") ||
		hasLabelStr(st.Labels, "fabrik:revalidate") {
		return true
	}
	// A landing that bounced (the PR stopped merging cleanly) ends the episode:
	// the re-settle after the rebase is announced afresh.
	if hasLabelStr(st.Labels, "fabrik:rebase-needed") {
		return true
	}
	if lpr := st.LinkedPR; lpr != nil && lpr.ValidateCompletedSHA != "" && lpr.HeadSHA != "" &&
		lpr.ValidateCompletedSHA != lpr.HeadSHA {
		return true
	}
	return false
}

func vsKeys(ref string) (started, closed string) { return "vs:" + ref, "vsclosed:" + ref }

// vsRelaxedKey marks an episode opened at the landing decision, before the
// complete label existed: such an episode must not be closed merely because the
// label is not (yet) there.
func vsRelaxedKey(ref string) string { return "vsrelaxed:" + ref }

// validateWithdrawalReason says why the engine took a validate-settled item back
// (#2120 R-unsettle-1): the PR needs a rebase, its head moved so CI must run
// again, a worker was dispatched, or new feedback arrived. "" when the item
// merely moved on, was closed or was sent back through Validate on purpose
// (fabrik:revalidate) — a human or engine decision, not a withdrawal.
func (e *Engine) validateWithdrawalReason(st *itemstate.ItemState, rebaseSeen bool) string {
	if st.IsClosed || st.Status != "Validate" || hasLabelStr(st.Labels, "fabrik:revalidate") {
		return ""
	}
	switch {
	case hasLabelStr(st.Labels, "fabrik:rebase-needed") || rebaseSeen:
		return reasonRebaseNeeded
	case func() bool {
		lpr := st.LinkedPR
		return lpr != nil && lpr.ValidateCompletedSHA != "" && lpr.HeadSHA != "" && lpr.ValidateCompletedSHA != lpr.HeadSHA
	}():
		return reasonCIOnNewHead
	case st.Worker != nil || hasReworkingLabel(st.Labels):
		return reasonWorkerDispatched
	case e.cachedPendingFeedbackOf(st).any():
		return reasonNewFeedback
	}
	return ""
}

// closeSettleEpisode closes an open episode whose ending conditions hold, and
// announces one settlement-withdrawn event when the reason is that the engine
// took the item back. Consumer goroutine only.
func (ce *channelEvents) closeSettleEpisode(ref string, st *itemstate.ItemState) {
	started, closed := vsKeys(ref)
	n := ce.hub.Counter(started)
	if n <= ce.hub.Counter(closed) {
		return
	}
	relaxed := ce.hub.Counter(vsRelaxedKey(ref)) == n
	ended := validateSettledEpisodeEnded(st)
	if relaxed && !hasLabelStr(st.Labels, "stage:Validate:complete") &&
		!st.IsClosed && st.Status == "Validate" && !hasLabelStr(st.Labels, "fabrik:revalidate") &&
		!hasLabelStr(st.Labels, "fabrik:rebase-needed") {
		ended = false
	}
	m := ce.memo[ref]
	if m == nil {
		m = &itemMemo{} // closing an episode never creates the item's memo (derive owns the first-sight baseline)
	}
	reason := ce.e.validateWithdrawalReason(st, m.rebaseSeen)
	// A worker dispatch or new feedback also withdraw a settlement that the
	// older end conditions do not cover. An episode opened at the landing
	// decision (yolo) still has its worker, so it is exempt.
	if !ended && !relaxed && (reason == reasonWorkerDispatched || reason == reasonNewFeedback) {
		ended = true
	}
	// A change of autonomy while the item stays settled (cruise to yolo, a label
	// removed) is a decision about the merge, not a withdrawal: it ends the
	// episode so the next settle point announces the item again with its cause
	// (#2169 R3a). No settlement-withdrawn follows, because the item is still
	// settled.
	if !ended && !relaxed && ce.autonomyChangedSinceSettle(ref, n, st) {
		ended = true
	}
	if !ended {
		return
	}
	ce.hub.SetCounter(closed, n)
	if reason != "" && !ce.e.chanTest.withdrawalOff.Load() {
		ev := ce.e.withdrawnEvent(st, string(channelevents.ValidateSettled), reason)
		ev.DedupKey = fmt.Sprintf("swvs:%s:%d", ref, n)
		ce.enqueueDerived([]channelevents.Event{ce.withComment(m, st, ev)})
	}
}

// settleCandidate handles one settle point the engine reported: close a
// finished episode, then emit the captured validate-settled event if no episode
// is open. The episode counter and the dedup key persist in the hub, so a
// restart neither re-emits an announced episode nor loses an unannounced one.
func (ce *channelEvents) settleCandidate(ref string, cand settleCandidate) {
	ev := cand.ev
	repo, n, ok := parseIssueRef(ref)
	if !ok {
		return
	}
	// The item may already have moved on (a yolo merge in the same pass); a
	// missing or advanced snapshot only matters for closing a previous episode.
	if snap, ok := ce.e.store.Peek(repo, n); ok {
		st := snap.State()
		ce.closeSettleEpisode(ref, &st)
	}
	started, closed := vsKeys(ref)
	if ce.hub.Counter(started) > ce.hub.Counter(closed) {
		return // an episode is already open: announced once
	}
	ep := ce.hub.BumpCounter(started)
	if cand.relaxed {
		ce.hub.SetCounter(vsRelaxedKey(ref), ep)
	}
	ev.DedupKey = started + ":" + strconv.Itoa(ep)
	ce.stampSettleCause(ref, &ev, ep)
	m := ce.memoFor(ref)
	var st itemstate.ItemState
	if snap, ok := ce.e.store.Peek(repo, n); ok {
		st = snap.State()
	}
	ce.enqueueDerived([]channelevents.Event{ce.withComment(m, &st, ev)})
}

// autonomyMode names the item's autonomy as the engine's own checks resolve
// it: the raw cruise label wins over yolo (cruise is the conservative label).
func (e *Engine) autonomyMode(st *itemstate.ItemState) string {
	switch {
	case hasLabelStr(st.Labels, "fabrik:cruise"):
		return "cruise"
	case e.cfg.Yolo || hasLabelStr(st.Labels, "fabrik:yolo"):
		return "yolo"
	}
	return "none"
}

// validateSettledNext says what the engine does next, mirroring
// runCatchUpPhase2 and attemptMergeOnValidate: only yolo (and never alongside
// cruise) acts; with the merge train on it queues the PR, otherwise it merges
// (or has already enabled auto-merge). Everything else waits for a human.
func (e *Engine) validateSettledNext(st *itemstate.ItemState) string {
	if e.autonomyMode(st) != "yolo" {
		return "waiting-for-human"
	}
	if hasLabelStr(st.Labels, "fabrik:auto-merge-enabled") {
		return "auto-merge"
	}
	if e.cfg.MergeTrain == "on" {
		return "merge-train"
	}
	return "auto-merge"
}

// cachedCIVerdict classifies the cached check runs of the PR head with the
// engine's own classifier. Never a live read.
//
// A PR with no cached check runs reports "unknown", not "none": the cache fills
// from check_run webhooks and gate reads, so absence here does not prove the head
// has no checks. The engine's own gate state is reported separately (ci_gate).
func cachedCIVerdict(lpr *itemstate.LinkedPRState) string {
	if lpr == nil || len(lpr.CheckRuns) == 0 {
		return "unknown"
	}
	status, _, _ := gh.ClassifyCheckRuns(lpr.CheckRuns)
	switch status {
	case gh.CheckRunsPending:
		return "pending"
	case gh.CheckRunsFailed, gh.CheckRunsNonPassing:
		// #2296: a cancelled/stale/unrecognised latest run has no result and is
		// never green; the worker's ci-status.md and the gates call it red too.
		return "red"
	}
	if allCheckRunsPassed(lpr.CheckRuns) {
		return "green"
	}
	return "passed-with-skips"
}

// reviewSummary lists each reviewer's latest non-dismissed review.
func reviewSummary(lpr *itemstate.LinkedPRState) string {
	if lpr == nil {
		return "none"
	}
	latest := map[string]string{}
	var order []string
	for _, r := range lpr.Reviews {
		if r.State == "DISMISSED" || r.State == "PENDING" {
			continue
		}
		if _, seen := latest[r.Author]; !seen {
			order = append(order, r.Author)
		}
		latest[r.Author] = r.State
	}
	if len(order) == 0 {
		return "none"
	}
	sort.Strings(order)
	parts := make([]string, 0, len(order))
	for _, a := range order {
		parts = append(parts, a+": "+latest[a])
	}
	return strings.Join(parts, ", ")
}

func (e *Engine) validateSettledEvent(snap itemstate.Snapshot, st *itemstate.ItemState, stage *stages.Stage) channelevents.Event {
	lpr := st.LinkedPR
	ev := e.baseEvent(st, channelevents.ValidateSettled)
	next := e.validateSettledNext(st)
	ci := cachedCIVerdict(lpr)
	reviews := reviewSummary(lpr)
	unresolved := 0
	if lpr != nil {
		unresolved = len(dropOutdatedThreadComments(lpr.ThreadComments))
	}
	ev.Meta["next"] = next
	if next == "waiting-for-human" {
		// Decision-bearing: say how to act (R-options-1).
		if o := optionsJSON(attention.MergeDecisionOptions(ev.PR)); o != "" {
			ev.Meta["options"] = o
		}
	}
	ev.Meta["ci"] = ci
	if stage != nil && stage.WaitForCI != nil && *stage.WaitForCI {
		// Reaching the settle point means the engine's own CI gate cleared.
		ev.Meta["ci_gate"] = "cleared"
	} else {
		ev.Meta["ci_gate"] = "not-gated"
	}
	ev.Meta["reviews"] = reviews
	// Per-reviewer verdicts at the PR head (#2127 R5): a COMMENTED review reads the
	// same for a clean pass and for findings, these fields tell them apart.
	if j := reviewersJSON(e.reviewerVerdicts(st)); j != "" {
		ev.Meta["reviewers"] = j
	}
	// A green that follows a flake re-run on this head says so (#2127 R7).
	if lpr != nil && ci == "green" {
		if names := e.flakeRerunChecks(st.Repo, lpr.Number, lpr.HeadSHA); len(names) > 0 {
			ev.Meta["ci_rerun"] = strings.Join(names, ", ")
		}
	}
	ev.Meta["unresolved_threads"] = strconv.Itoa(unresolved)
	ev.Meta["autonomy"] = e.autonomyMode(st)
	if lpr != nil && lpr.HeadSHA != "" {
		ev.Meta["head_sha"] = lpr.HeadSHA
	}
	action := map[string]string{
		"waiting-for-human": "waiting for a human to decide",
		"auto-merge":        "the engine will auto-merge",
		"merge-train":       "the engine will queue it into the merge train",
	}[next]
	ev.Content = fmt.Sprintf("%s settled at Validate: PR #%d CI %s, reviews %s; %s",
		issueRef(st.Repo, st.Number), ev.PR, ci, reviews, action)
	return ev
}

// The settleCauseOff seam (channel_settings.go) neutralises the re-settle cause
// (tests only): no cause, no actor, and an autonomy change no longer ends the
// episode, which is how an item behaved before #2169.

// autonomyActor is who last wrote the item's autonomy label, as attributed by
// the observer; added says whether that write was an addition.
type autonomyActor struct {
	attr  labelAttribution
	added bool
}

// noteAutonomyWrite remembers the attribution of a fabrik:cruise / fabrik:yolo
// delta (observer goroutine). An added label wins over a removed one: a cruise to
// yolo swap is two deltas and the actor of the new mode is the decision.
func (ce *channelEvents) noteAutonomyWrite(key string, d itemstate.LabelDelta, attr labelAttribution) {
	if d.Label != "fabrik:cruise" && d.Label != "fabrik:yolo" {
		return
	}
	ce.dmu.Lock()
	defer ce.dmu.Unlock()
	if ce.autonomy == nil {
		ce.autonomy = map[string]autonomyActor{}
	}
	if prev, ok := ce.autonomy[key]; ok && prev.added && !d.Added {
		return
	}
	ce.autonomy[key] = autonomyActor{attr: attr, added: d.Added}
}

// takeAutonomyWrite returns and forgets the remembered autonomy actor.
func (ce *channelEvents) takeAutonomyWrite(key string) (autonomyActor, bool) {
	ce.dmu.Lock()
	defer ce.dmu.Unlock()
	a, ok := ce.autonomy[key]
	delete(ce.autonomy, key)
	return a, ok
}

// autonomyChangedSinceSettle reports whether the open episode n was announced
// under a different autonomy than the item has now, while it is otherwise still
// settled.
func (ce *channelEvents) autonomyChangedSinceSettle(ref string, n int, st *itemstate.ItemState) bool {
	if ce.e.chanTest.settleCauseOff.Load() || ce.ledger == nil {
		return false
	}
	rec, ok := ce.ledger.Settle(ref)
	if !ok || rec.Episode != n || rec.Autonomy == "" {
		return false
	}
	if st.IsClosed || st.Status != "Validate" || !hasLabelStr(st.Labels, "stage:Validate:complete") {
		return false
	}
	return rec.Autonomy != ce.e.autonomyMode(st)
}

// stampSettleCause records the announced settle in the ledger and, when this is
// not the first one, says why the item settled again: revalidated, new-head,
// autonomy-changed (with the autonomy writer's attribution) or other, in that
// precedence. The first settle carries no cause: after a lost ledger the daemon
// cannot honestly claim "initial" (ADR 2169).
func (ce *channelEvents) stampSettleCause(ref string, ev *channelevents.Event, episode int) {
	if ce.ledger == nil {
		return
	}
	prev, havePrev := ce.ledger.Settle(ref)
	actor, haveActor := ce.takeAutonomyWrite(ref)
	head, autonomy := ev.Meta["head_sha"], ev.Meta["autonomy"]
	ce.ledger.SetSettle(ref, channelevents.SettleRecord{Episode: episode, HeadSHA: head, Autonomy: autonomy})
	if !havePrev || ce.e.chanTest.settleCauseOff.Load() {
		return
	}
	cause := channelevents.CauseOther
	switch {
	case prev.Revalidated:
		cause = channelevents.CauseRevalidated
	case prev.HeadSHA != "" && head != "" && prev.HeadSHA != head:
		cause = channelevents.CauseNewHead
	case prev.Autonomy != "" && prev.Autonomy != autonomy:
		cause = channelevents.CauseAutonomyChanged
	}
	ev.Meta[channelevents.MetaCause] = cause
	if cause == channelevents.CauseAutonomyChanged {
		attr := labelAttribution{Actor: "unknown"}
		if haveActor {
			attr = actor.attr
		}
		by := attributionMeta(ev, attr)
		ev.Content += fmt.Sprintf(" (settled again: autonomy changed from %s to %s by %s)", prev.Autonomy, autonomy, by)
		return
	}
	ev.Content += fmt.Sprintf(" (settled again: %s)", cause)
}
