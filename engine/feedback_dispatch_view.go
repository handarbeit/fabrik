package engine

import (
	"fmt"
	"sort"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// feedbackGapRepairBudget bounds how many times the gate repairs the dispatcher's
// input for one exact gap (#2160). Two covers GitHub's eventual consistency
// resolving in a poll or two; past it the gate keeps holding (fail-closed) and
// says so loudly instead of refetching for ever.
const feedbackGapRepairBudget = 2

// feedbackKey identifies a comment for the gate/dispatcher comparison: the node
// or synthetic ID when there is one (a review body is "review-body:<id>"),
// otherwise the numeric database ID.
func feedbackKey(c gh.Comment) string {
	if c.ID != "" {
		return c.ID
	}
	return fmt.Sprintf("%d", c.DatabaseID)
}

// dispatcherVisibleIDs lists the review bodies and thread comments item's own
// (board cache sourced) review data offers Phase 1's handleReviewGate before the
// processed / durably-addressed filters — the same eligibility
// (reviewBodyCandidates) the dispatcher's builders start from. The filters are
// not repeated: they read the same store watermark and the same durable marker
// the gate already applied, so an ID the gate holds on passes them on the
// dispatcher's side too, and re-running them would cost another comment read per
// hold (durablyAddressedReviewIDs).
func dispatcherVisibleIDs(item gh.ProjectItem) []gh.Comment {
	var out []gh.Comment
	for _, c := range reviewBodyCandidates(item.LinkedPRReviews, func(string) bool { return false }) {
		out = append(out, gh.Comment{ID: c.id})
	}
	return append(out, item.LinkedPRReviewThreadComments...)
}

// feedbackGap returns the review bodies and threads in held that view lacks,
// compared by comment ID. A non-empty gap means the gate holds on feedback the
// dispatcher cannot see (#2160).
func feedbackGap(held, view []gh.Comment) []gh.Comment {
	seen := make(map[string]struct{}, len(view))
	for _, c := range view {
		seen[feedbackKey(c)] = struct{}{}
	}
	var gap []gh.Comment
	for _, c := range held {
		if _, ok := seen[feedbackKey(c)]; !ok {
			gap = append(gap, c)
		}
	}
	return gap
}

// dispatcherGap computes the held feedback the dispatcher's view lacks. The view
// is read through e.readClient on a copy of the caller's original item — exactly
// the source the next admission reads — so a stale board cache shows up as a gap
// and a fresh one does not. Only review bodies and threads are compared: they
// are what handleReviewGate derives from cached review data. A base:<branch> or
// missing-link item resolves its reviews over REST on both sides, so the two
// cannot disagree and nothing is read. A read error leaves the gap empty; it
// never loosens the hold.
func (e *Engine) dispatcherGap(orig gh.ProjectItem, pf pendingFeedback) []gh.Comment {
	held := append(append([]gh.Comment{}, pf.Threads...), pf.Bodies...)
	if len(held) == 0 {
		return nil
	}
	view := orig
	if itemHasBaseLabel(orig) {
		return nil
	}
	if err := e.readClient.FetchItemDetails(&view); err != nil {
		e.logf(orig.Number, "warn", "feedback-gate: dispatcher-view read failed (%v) — skipping gap check\n", err)
		return nil
	}
	if reviewDataRidesMissingLink(view) {
		return nil
	}
	return feedbackGap(held, dispatcherVisibleIDs(view))
}

// repairDispatcherBlindness handles a gate hold on feedback the dispatcher does
// not see (#2160, ADR 2160). It never dispatches: it invalidates the item's deep
// cache so the next admission performs a real fetch, and records a short
// admission cooldown so that admission is the very next poll. Phase 1's ordinary,
// already-bounded review-reinvoke path (dispatchWithCycleLimit) then dispatches.
// Bounded per exact gap by feedbackGapRepairBudget.
func (e *Engine) repairDispatcherBlindness(item gh.ProjectItem, gap []gh.Comment) {
	ids := make([]string, 0, len(gap))
	for _, c := range gap {
		ids = append(ids, feedbackKey(c))
	}
	sort.Strings(ids)
	repo := itemOwnerRepoString(item, e.defaultRepo())
	key := fmt.Sprintf("%s#%d:%s", repo, item.Number, strings.Join(ids, ","))

	e.feedbackGapMu.Lock()
	if e.feedbackGaps == nil {
		e.feedbackGaps = make(map[string]int)
	}
	n := e.feedbackGaps[key]
	if n >= feedbackGapRepairBudget {
		e.feedbackGapMu.Unlock()
		return
	}
	e.feedbackGaps[key] = n + 1
	e.feedbackGapMu.Unlock()

	list := feedbackIDs(gap)
	if n+1 == feedbackGapRepairBudget {
		e.logf(item.Number, "feedback-gate", "dispatcher cannot see held feedback (%s) — refetching (last repair; the gate keeps holding if this does not resolve it)\n", list)
	} else {
		e.logf(item.Number, "feedback-gate", "dispatcher cannot see held feedback (%s) — invalidating cached item so the next poll refetches\n", list)
	}
	e.store.Apply(itemstate.DeepFetchInvalidated{Repo: repo, Number: item.Number})
	e.store.Apply(itemstate.CooldownRecorded{
		Repo:   repo,
		Number: item.Number,
		Reason: "feedback-dispatcher-gap",
		Until:  e.now().Add(e.mergeGateRecheckInterval()),
	})
}
