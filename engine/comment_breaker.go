package engine

import (
	"fmt"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/tui"
)

// effectiveCommentBreakerWindow returns the comment-processing circuit
// breaker's threshold (N) and rolling window (T), applying zero-means-default
// semantics: N=10, T=30min (#1089). Mirrors effectiveTrialWindow's pattern
// (ADR-059 D8).
func (e *Engine) effectiveCommentBreakerWindow() (int, time.Duration) {
	n := e.cfg.MaxCommentCyclesPerWindow
	if n <= 0 {
		n = 10
	}
	t := e.cfg.CommentCycleWindow
	if t <= 0 {
		t = 30 * time.Minute
	}
	return n, t
}

// recordCommentBreakerInvocation records a comment-processing invocation for
// item's circuit-breaker counter, attributing it to author (the triggering
// comment's author — surfaced in the trip comment if the breaker fires).
func (e *Engine) recordCommentBreakerInvocation(item gh.ProjectItem, author string) {
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.CommentBreakerInvocationRecorded{
		Repo:   repoStr,
		Number: item.Number,
		At:     time.Now(),
		Author: author,
	})
}

// commentBreakerCount returns the number of comment-processing invocations
// recorded for item within the current rolling window. Window math (pruning)
// lives here, not in itemstate — the Store holds raw timestamps only,
// mirroring the mergeTrainTrials runaway-guard precedent (ADR-059 D8).
func (e *Engine) commentBreakerCount(item gh.ProjectItem) int {
	_, window := e.effectiveCommentBreakerWindow()
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	snap, err := e.store.Get(repoStr, item.Number)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-window)
	count := 0
	for _, at := range snap.CommentBreakerInvocationsAt() {
		if !at.Before(cutoff) {
			count++
		}
	}
	return count
}

// resetCommentBreaker clears item's comment-breaker invocation counter after
// forward progress is observed: a stage:*:complete transition, a new commit
// on the branch, a PR state change, an issue-body update, or a manual human
// unpause (#1089).
func (e *Engine) resetCommentBreaker(item gh.ProjectItem) {
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.CommentBreakerReset{Repo: repoStr, Number: item.Number})
}

// checkCommentBreaker evaluates whether item's comment-processing circuit
// breaker has tripped (commentBreakerCount >= threshold N within window T)
// and, if so, pauses the issue and suppresses further dispatch. Called at the
// tail of processComments, after the invocation for this cycle has already
// been recorded and any progress reset for this cycle has already been applied.
func (e *Engine) checkCommentBreaker(item gh.ProjectItem) {
	n, window := e.effectiveCommentBreakerWindow()
	count := e.commentBreakerCount(item)
	if count < n {
		return
	}
	e.tripCommentBreaker(item, count, window)
}

// tripCommentBreaker pauses item, posts an explanatory comment naming the
// loop (invocation count, window, last comment author, resume instructions),
// and emits a structural TUI event. Reuses fabrik:paused + fabrik:awaiting-input
// via the shared pauseIssue helper (engine/mutate.go) so the pause is
// honorable per ADR-069 — a subsequent bot comment cannot lift it.
func (e *Engine) tripCommentBreaker(item gh.ProjectItem, count int, window time.Duration) {
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	snap, _ := e.store.Get(repoStr, item.Number)
	lastAuthor := snap.CommentBreakerLastAuthor()
	displayAuthor := lastAuthor
	if displayAuthor == "" {
		displayAuthor = "unknown"
	}

	e.logf(item.Number, "comment-breaker", "circuit breaker tripped: %d comment-processing invocation(s) within %s with no forward progress — pausing\n", count, window)

	msg := fmt.Sprintf(
		"🏭 **Fabrik — comment-processing circuit breaker tripped**\n\n"+
			"This issue underwent **%d comment-processing invocations** within the last %s "+
			"with **no forward progress** (no stage completion, new commit, or PR state change).\n\n"+
			"Last comment author: **%s**.\n\n"+
			"This is a defense-in-depth backstop against a self-sustaining comment loop "+
			"(see incident #1083) — it should rarely if ever fire. Fabrik has paused this issue "+
			"for human review.\n\n"+
			"**To resume:** investigate why comment-processing wasn't making progress, then remove "+
			"the `fabrik:paused` label.",
		count, window, displayAuthor,
	)
	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})

	e.emitStructural(tui.CommentBreakerTrippedEvent{
		IssueNumber:       item.Number,
		Repo:              repoStr,
		Title:             item.Title,
		StageName:         item.Status,
		InvocationCount:   count,
		Window:            window,
		LastCommentAuthor: lastAuthor,
	})
}
