package engine

import (
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// effectiveMaxNoOpCommentCycles returns the success-agnostic no-op comment
// cycle limit, applying zero-means-default semantics (default 5) — mirrors
// effectiveCommentBreakerWindow's pattern (#1089).
func (e *Engine) effectiveMaxNoOpCommentCycles() int {
	if e.cfg.MaxNoOpCommentCycles > 0 {
		return e.cfg.MaxNoOpCommentCycles
	}
	return 5
}

// checkNoOpCommentCycle records this comment-processing cycle's outcome
// against the success-agnostic circuit breaker (#1555, sibling of
// #1089/#1382/#1413/#1414): reset the per-stage counter on genuine progress
// (R2 — a commit landed, the issue body updated, or FABRIK_STAGE_COMPLETE was
// emitted), otherwise increment it (R1) — regardless of whether the
// invocation itself exited successfully, which is the entire defect this
// counter exists to close. Called exactly once per processComments cycle, at
// whichever of its five mutually exclusive exit points was reached, mirroring
// checkCommentBreaker's call-site shape.
//
// Returns true when this cycle's increment tripped the limit, in which case
// the issue has already been paused (tripNoOpCommentCycleBreaker) and the
// caller must skip evaluating checkCommentBreaker for this cycle (R5) — at
// most one pause comment is posted per cycle across the two breakers.
func (e *Engine) checkNoOpCommentCycle(item gh.ProjectItem, stage *stages.Stage, progressed bool, lastAuthor string) bool {
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	if progressed {
		e.store.Apply(itemstate.NoOpCommentCycleReset{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		return false
	}
	e.store.Apply(itemstate.NoOpCommentCycleIncremented{Repo: repoStr, Number: item.Number, StageName: stage.Name})

	snap, err := e.store.Get(repoStr, item.Number)
	if err != nil {
		return false
	}
	count := snap.NoOpCommentCycles(stage.Name)
	limit := e.effectiveMaxNoOpCommentCycles()
	if count < limit {
		return false
	}
	e.tripNoOpCommentCycleBreaker(item, stage, count, lastAuthor)
	return true
}

// tripNoOpCommentCycleBreaker pauses item, posts an explanatory comment
// naming the stage, the consecutive no-progress invocation count, and the
// last comment's author (R4), and emits a structural TUI event. Reuses
// fabrik:paused + fabrik:awaiting-input via the shared pauseIssue helper
// (engine/mutate.go) so the pause is honorable per ADR-069 — a subsequent bot
// comment cannot lift it.
func (e *Engine) tripNoOpCommentCycleBreaker(item gh.ProjectItem, stage *stages.Stage, count int, lastAuthor string) {
	displayAuthor := lastAuthor
	if displayAuthor == "" {
		displayAuthor = "unknown"
	}

	e.logf(item.Number, "noop-comment-breaker", "success-agnostic circuit breaker tripped: %d consecutive comment-processing invocation(s) for stage %q produced no forward progress — pausing\n", count, stage.Name)

	msg := fmt.Sprintf(
		"🏭 **Fabrik — no-op comment-processing circuit breaker tripped**\n\n"+
			"This issue underwent **%d consecutive comment-processing invocation(s)** for stage "+
			"**%s** with **no forward progress** (no new commit, no issue-body update, and no "+
			"`FABRIK_STAGE_COMPLETE`) — regardless of whether each individual invocation itself "+
			"exited successfully. This is the success-agnostic sibling of the existing "+
			"failure-keyed comment circuit breaker (#1089/#1382): a comment that keeps being "+
			"redelivered and \"successfully\" re-processed as a no-op would otherwise loop "+
			"indefinitely without ever failing or exceeding the windowed burst-loop breaker.\n\n"+
			"Last comment author: **%s**.\n\n"+
			"Fabrik has paused this issue for human review. Check whether the same comment is "+
			"being redelivered (a processed-marker not sticking) or whether the stage itself is "+
			"stuck in a self-inflicted loop.\n\n"+
			"**To resume:** investigate the root cause, then remove the `fabrik:paused` label.",
		count, stage.Name, displayAuthor,
	)
	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})

	e.emitStructural(tui.CommentBreakerTrippedEvent{
		IssueNumber:       item.Number,
		Repo:              itemOwnerRepoString(item, e.defaultRepo()),
		Title:             item.Title,
		StageName:         item.Status,
		InvocationCount:   count,
		LastCommentAuthor: lastAuthor,
	})
}
