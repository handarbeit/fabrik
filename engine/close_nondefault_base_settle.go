package engine

import (
	"fmt"
	"strings"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
)

// nonDefaultBaseAwaitingCloseLabel marks an issue whose explicit
// closeIssueIfNonDefaultBase CloseIssue call failed (ADR-1096). It durably
// records the outstanding close (a GitHub label survives an engine restart,
// unlike an itemstate.Store-only marker — there is no artifact to safely
// "redo" here except the call itself) so settleNonDefaultBaseCloses can
// retry it every poll, independent of the item's board column. By the time
// this call can fail, the board has already advanced to Done — a materially
// different starting condition than ADR-060's fabrik:awaiting-done, which is
// written before the Done-move. See ADR-1097.
const nonDefaultBaseAwaitingCloseLabel = "fabrik:awaiting-close"

// nonDefaultBaseCloseRetryStage is a dedicated, non-real stage name used to key the
// existing StageRetryIncremented/StageRetryCleared/Attempts counter for retries of a
// stalled non-default-base explicit close — mirrors mergeTrainMemberCloseRetryStage. The
// double-underscore wrapping makes it unrepresentable as a real YAML stage `name:`, so it
// can never collide with a configured stage's retry count.
const nonDefaultBaseCloseRetryStage = "__non_default_base_close__"

// markNonDefaultBaseCloseOutstanding records that closeIssueIfNonDefaultBase's explicit
// CloseIssue call failed, so settleNonDefaultBaseCloses retries it on a later poll.
// Idempotent — a no-op if the marker is already present.
func (e *Engine) markNonDefaultBaseCloseOutstanding(item gh.ProjectItem, owner, repo string) {
	if hasLabel(item.Labels, nonDefaultBaseAwaitingCloseLabel) {
		return
	}
	e.addLabel(item, nonDefaultBaseAwaitingCloseLabel)
}

// settleNonDefaultBaseCloses is the per-poll settle scan for the non-default-base explicit
// close retry (ADR-1097). It runs unconditionally every poll over the raw board snapshot
// (not deepFetchCandidates): the item has already reached Done by the time the marker is
// written, so the ADR-060 dispatch-suppression/terminal-skip machinery built around
// deepFetchCandidates and itemMayNeedWork/itemNeedsWork has nothing to do here and would
// only add risk (a naive port would need the marker added to transientLifecycleLabels to
// survive the terminal-skip interaction — this scan sidesteps that entirely by not
// depending on either mechanism).
func (e *Engine) settleNonDefaultBaseCloses(board *gh.ProjectBoard) {
	for _, item := range board.Items {
		if !hasLabel(item.Labels, nonDefaultBaseAwaitingCloseLabel) || hasLabel(item.Labels, "fabrik:paused") {
			continue
		}
		e.settleNonDefaultBaseClose(item)
	}
}

// settleNonDefaultBaseClose retries the outstanding explicit CloseIssue call for a single
// item. Idempotent: if the issue is already closed (e.g. GitHub's own Closes #N auto-close
// finally landed, or a human closed it manually), it skips the redundant call and just
// clears the marker.
func (e *Engine) settleNonDefaultBaseClose(item gh.ProjectItem) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	if item.IsClosed {
		e.logf(item.Number, "pr-terminal", "issue #%d already closed — clearing awaiting-close marker\n", item.Number)
		e.clearNonDefaultBaseCloseMarker(item, owner, repo)
		return
	}

	if err := e.client.CloseIssue(owner, repo, item.Number); err != nil {
		e.logf(item.Number, "pr-terminal", "retry: could not close #%d: %v\n", item.Number, err)
		e.recordNonDefaultBaseCloseRetry(item)
		return
	}

	if c := e.cache(); c != nil {
		c.ApplyIssueClosed(boardcache.ItemKey(owner+"/"+repo, item.Number))
	}
	if itemOnDefaultBase(item) {
		// The default-base backstop (#1962, guardDefaultBaseAutoClose): GitHub's
		// Closes #N auto-close should have closed this issue and did not. Say so
		// on the issue, naming the merged PR — resolved by branch, since GitHub's
		// closing-keyword link is exactly what may be missing.
		prClause := "its merged PR"
		if pr, perr := e.client.FetchLinkedPR(owner, repo, item.Number); perr == nil && pr != nil && pr.Number != 0 {
			prClause = fmt.Sprintf("PR #%d", pr.Number)
		}
		e.logf(item.Number, "pr-terminal", "GitHub auto-close did not fire for #%d — closed explicitly (default base, %s)\n", item.Number, prClause)
		e.postItemComment(item, fmt.Sprintf(
			"🏭 **Fabrik — closed after merge**\n\n%s merged into the default branch, but GitHub's `Closes #%d` auto-close did not fire. Fabrik closed this issue explicitly.",
			strings.ToUpper(prClause[:1])+prClause[1:], item.Number), false)
	} else {
		e.logf(item.Number, "pr-terminal", "closed #%d (retry)\n", item.Number)
	}
	e.clearNonDefaultBaseCloseMarker(item, owner, repo)
}

// itemOnDefaultBase reports whether the item targets the repository default
// branch as far as the settle scan can tell without a worktree: no base:
// label. (A base: label naming the default branch is treated as non-default
// here, which only changes the wording of the log line, never the close.)
func itemOnDefaultBase(item gh.ProjectItem) bool {
	return !itemHasBaseLabel(item)
}

// recordNonDefaultBaseCloseRetry increments the in-memory retry counter for a stalled
// non-default-base explicit close, keyed by the dedicated nonDefaultBaseCloseRetryStage
// constant. Escalates via escalateNonDefaultBaseCloseFailure once e.cfg.MaxRetries is
// reached. Mirrors recordMergeTrainMemberCloseRetry, including its MaxRetries<=0
// (unlimited retries, never escalate) guard.
func (e *Engine) recordNonDefaultBaseCloseRetry(item gh.ProjectItem) {
	e.recordSettleRetry(item, nonDefaultBaseCloseRetryStage, e.escalateNonDefaultBaseCloseFailure)
}

// escalateNonDefaultBaseCloseFailure is called when the outstanding non-default-base
// explicit close has failed MaxRetries times. It pauses the issue (fabrik:paused), removes
// the awaiting-close marker (retry suppression is no longer needed once fabrik:paused takes
// over — this scan's own paused-item guard leaves paused items alone), and posts an
// explanatory comment naming the merged PR (when known) with the manual recovery step —
// mirroring escalateMergeTrainMemberCloseFailure.
func (e *Engine) escalateNonDefaultBaseCloseFailure(item gh.ProjectItem) {
	e.logf(item.Number, "escalate", "non-default-base explicit close failed %d time(s) — pausing issue\n", e.cfg.MaxRetries)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	e.escalateSettle(item, nonDefaultBaseAwaitingCloseLabel, nonDefaultBaseCloseRetryStage, func(item gh.ProjectItem) {
		baseKind, baseWhere := "non-default-base", "onto a non-default base branch"
		if itemOnDefaultBase(item) {
			baseKind, baseWhere = "post-merge", "into the default branch and GitHub's auto-close did not fire"
		}
		prClause := ""
		if item.LinkedPRNumber != 0 {
			prClause = fmt.Sprintf(" (merged via PR #%d)", item.LinkedPRNumber)
		}
		comment := fmt.Sprintf(
			"🏭 **Fabrik — %s explicit close failed**\n\nThis issue's linked PR merged%s %s, but closing the issue explicitly could not be completed after %d attempt(s). The issue has been paused.\n\nManual fix:\n```\ngh issue close %d --repo %s/%s\n```\nThen remove the `fabrik:paused` label.",
			baseKind, prClause, baseWhere, e.cfg.MaxRetries, item.Number, owner, repo,
		)
		e.postItemComment(item, comment, true)
	})
}

// clearNonDefaultBaseCloseMarker removes the awaiting-close marker and clears the retry
// counter once the issue is confirmed closed (by us or by any other actor).
func (e *Engine) clearNonDefaultBaseCloseMarker(item gh.ProjectItem, owner, repo string) {
	e.clearSettleMarker(item, owner, repo, nonDefaultBaseAwaitingCloseLabel, nonDefaultBaseCloseRetryStage)
}
