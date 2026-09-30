package engine

import (
	"fmt"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// pendingFeedback is what the feedback gate found unprocessed on an item at the
// moment of a decision (#1953 R2).
type pendingFeedback struct {
	// Comments are unprocessed issue / linked-PR comments — exactly
	// findNewComments, so the Fabrik-prefix, 🚀, store-watermark and bot
	// service-notice filters all apply.
	Comments []gh.Comment
	// Threads are unresolved review-thread comments on the current head
	// (currentHeadReviewThreadComments — an isOutdated thread never holds, #1207).
	Threads []gh.Comment
	// Bodies are unaddressed review bodies (buildReviewBodyCommentsFromReviews,
	// #1045 actionability, #1555 durable dedup).
	Bodies []gh.Comment
	// Live is false when any read backing the answer failed, so "nothing found"
	// cannot be trusted.
	Live bool
}

func (p pendingFeedback) empty() bool {
	return len(p.Comments)+len(p.Threads)+len(p.Bodies) == 0
}

// unprocessedFeedback is the single "is anything unprocessed on this item right
// now?" predicate (#1953 R2). It reports what dispatch would go on to act on:
// the same comment, thread and review-body definitions the reinvoke paths use,
// so a hold is never placed for something dispatch would ignore.
//
// alreadyLive says item was itself just read live (attemptMergeOnValidate's
// FetchItemDetails). When false the predicate does that read itself, on a copy —
// a caller's snapshot may be tens of minutes old (handleStageComplete) and a
// frozen phase1Ctx never qualifies. A base:<branch> item's reviews are always
// resolved through a live REST read (its GraphQL review fields are structurally
// empty). Any read error yields Live=false.
func (e *Engine) unprocessedFeedback(item gh.ProjectItem, alreadyLive bool) pendingFeedback {
	pf := pendingFeedback{Live: true}
	if !alreadyLive {
		fresh := item
		if err := e.client.FetchItemDetails(&fresh); err != nil {
			e.logf(item.Number, "warn", "feedback-gate: live re-read failed (%v) — feedback state unknown\n", err)
			pf.Live = false
		} else {
			item = fresh
		}
	}
	reviews, err := e.resolveReviewsForFeedbackChecked(item)
	if err != nil {
		e.logf(item.Number, "warn", "feedback-gate: live review read failed (%v) — feedback state unknown\n", err)
		pf.Live = false
		reviews = item.LinkedPRReviews
	}
	pf.Comments = e.findNewComments(item)
	pf.Threads = e.currentHeadReviewThreadComments(item)
	pf.Bodies = e.buildReviewBodyCommentsFromReviews(item, reviews)
	return pf
}

// feedbackGateBlocks is the gate every advance and landing decision consults
// (#1953 R2/R5/R6): it holds — returns true — when unprocessedFeedback is
// non-empty, or when the live read failed (fail closed: a false hold is
// transient, a false merge or advance is the bug). where names the decision
// being held for the log ("landing decision", "advance", ...).
//
// This supersedes ADR-1862's commentGateBlocksLanding, which covered issue
// comments only; the comment half is unchanged. It never decides what counts as
// actionable — #1045's definition stands — and never changes the reinvoke
// bounds: the held feedback is dispatched through the ordinary paths, under
// MaxReviewCycles, the no-op and frequency comment breakers and the #1555
// marker, so feedback that can never be satisfied ends in a pause, not a loop.
//
// On a hold it records a "feedback-pending" cooldown, mirroring handleReviewGate's
// "review-blocked", so itemMayNeedWork's expiry path re-evaluates the item every
// githubRecheckInterval even when nothing bumps updatedAt.
func (e *Engine) feedbackGateBlocks(item gh.ProjectItem, alreadyLive bool, where string) bool {
	pf := e.unprocessedFeedback(item, alreadyLive)
	if pf.empty() && pf.Live {
		return false
	}
	if pf.empty() {
		e.logf(item.Number, "feedback-gate", "holding %s — live feedback read failed, feedback state unknown\n", where)
	} else {
		e.logf(item.Number, "feedback-gate", "holding %s — %s\n", where, describePendingFeedback(pf))
	}
	e.store.Apply(itemstate.CooldownRecorded{
		Repo:   itemOwnerRepoString(item, e.defaultRepo()),
		Number: item.Number,
		Reason: "feedback-pending",
		Until:  e.now().Add(e.githubRecheckInterval()),
	})
	return true
}

// describePendingFeedback renders the R6 log detail, e.g.
// "2 unprocessed comment(s) (11, 12), 1 unprocessed review body (5356334497)".
func describePendingFeedback(pf pendingFeedback) string {
	var parts []string
	if n := len(pf.Comments); n > 0 {
		parts = append(parts, fmt.Sprintf("%d unprocessed comment(s) (%s)", n, feedbackIDs(pf.Comments)))
	}
	if n := len(pf.Threads); n > 0 {
		parts = append(parts, fmt.Sprintf("%d unresolved review thread comment(s) (%s)", n, feedbackIDs(pf.Threads)))
	}
	if n := len(pf.Bodies); n > 0 {
		noun := "review body"
		if n > 1 {
			noun = "review bodies"
		}
		parts = append(parts, fmt.Sprintf("%d unprocessed %s (%s)", n, noun, feedbackIDs(pf.Bodies)))
	}
	return strings.Join(parts, ", ")
}

// feedbackIDs lists comment identifiers for logging: the numeric database ID
// when there is one, otherwise the ID with a synthetic review-body prefix
// stripped, so a review body reads as its bare review ID.
func feedbackIDs(comments []gh.Comment) string {
	ids := make([]string, 0, len(comments))
	for _, c := range comments {
		if c.DatabaseID != 0 {
			ids = append(ids, fmt.Sprintf("%d", c.DatabaseID))
			continue
		}
		ids = append(ids, strings.TrimPrefix(c.ID, reviewBodyIDPrefix))
	}
	return strings.Join(ids, ", ")
}
