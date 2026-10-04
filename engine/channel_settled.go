package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
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

// noteValidateSettled records that item reached the engine's settle point and,
// when the cache-only predicate holds right now, captures the event as it
// stands. The event is built here, on the poll goroutine, rather than later on
// the consumer: a yolo item can be merged and moved to Done within the very same
// pass, and an evaluation deferred past that would find it no longer at
// Validate and silently drop the headline event. Everything here is a store
// Peek plus pure CPU — no GitHub call, no engine decision touched (R9, R10).
// A void, non-blocking call, safe with no hub running.
func (e *Engine) noteValidateSettled(item gh.ProjectItem, stage *stages.Stage) {
	if stage == nil || stage.Name != "Validate" {
		return
	}
	ce := e.channelEvents()
	if ce == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			e.logf(item.Number, "channel", "validate-settled hook panic recovered: %v\n", r)
		}
	}()
	repo := itemOwnerRepoString(item, e.defaultRepo())
	snap, ok := e.store.Peek(repo, item.Number)
	if !ok {
		return
	}
	st := snap.State()
	if !e.validateSettledSnap(snap, &st) {
		return
	}
	ev := e.validateSettledEvent(snap, &st)
	key := issueRef(repo, item.Number)
	ce.mu.Lock()
	if ce.settleCand == nil {
		ce.settleCand = map[string]channelevents.Event{}
	}
	ce.settleCand[key] = ev
	ce.mu.Unlock()
	ce.signal()
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
	processed := func(id string) bool { return !snap.CommentProcessed(id).IsZero() }
	var f cachedFeedback
	f.Comments = len(filterNewComments(st.Comments, processed))
	if lpr := st.LinkedPR; lpr != nil {
		f.Threads = len(dropOutdatedThreadComments(filterReviewThreadComments(lpr.ThreadComments, processed)))
		cands := reviewBodyCandidates(lpr.Reviews, processed)
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
// itself reached the settle point, so it checks the remaining cache-visible
// conditions rather than re-deriving the gates.
func (e *Engine) validateSettledSnap(snap itemstate.Snapshot, st *itemstate.ItemState) bool {
	if st.IsPR || st.IsClosed || st.Status != "Validate" || st.Worker != nil {
		return false
	}
	if !hasLabelStr(st.Labels, "stage:Validate:complete") {
		return false
	}
	for _, l := range []string{
		"fabrik:awaiting-ci", "fabrik:awaiting-review", "fabrik:bot-reprompted",
		"fabrik:paused", "fabrik:blocked", "fabrik:rebase-needed", "fabrik:revalidate",
		"fabrik:awaiting-landing-verification",
	} {
		if hasLabelStr(st.Labels, l) {
			return false
		}
	}
	for _, d := range st.BlockedBy {
		if !strings.EqualFold(d.State, "CLOSED") {
			return false
		}
	}
	lpr := st.LinkedPR
	if lpr == nil || lpr.Number == 0 || lpr.Merged || lpr.State == "closed" {
		return false
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
	if lpr := st.LinkedPR; lpr != nil && lpr.ValidateCompletedSHA != "" && lpr.HeadSHA != "" &&
		lpr.ValidateCompletedSHA != lpr.HeadSHA {
		return true
	}
	return false
}

func vsKeys(ref string) (started, closed string) { return "vs:" + ref, "vsclosed:" + ref }

// closeSettleEpisode closes an open episode whose ending conditions hold.
// Consumer goroutine only.
func (ce *channelEvents) closeSettleEpisode(ref string, st *itemstate.ItemState) {
	started, closed := vsKeys(ref)
	n := ce.hub.Counter(started)
	if n > ce.hub.Counter(closed) && validateSettledEpisodeEnded(st) {
		ce.hub.SetCounter(closed, n)
	}
}

// settleCandidate handles one settle point the engine reported: close a
// finished episode, then emit the captured validate-settled event if no episode
// is open. The episode counter and the dedup key persist in the hub, so a
// restart neither re-emits an announced episode nor loses an unannounced one.
func (ce *channelEvents) settleCandidate(ref string, ev channelevents.Event) {
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
	ev.DedupKey = started + ":" + strconv.Itoa(ep)
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
func cachedCIVerdict(lpr *itemstate.LinkedPRState) string {
	if lpr == nil || len(lpr.CheckRuns) == 0 {
		return "none"
	}
	status, _, _ := gh.ClassifyCheckRuns(lpr.CheckRuns)
	switch status {
	case gh.CheckRunsPending:
		return "pending"
	case gh.CheckRunsFailed:
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

func (e *Engine) validateSettledEvent(snap itemstate.Snapshot, st *itemstate.ItemState) channelevents.Event {
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
	ev.Meta["ci"] = ci
	ev.Meta["reviews"] = reviews
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
