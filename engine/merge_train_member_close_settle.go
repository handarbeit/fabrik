package engine

import (
	"errors"
	"fmt"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// mergeTrainAwaitingMemberCloseLabel marks a merge-train singleton landing (landSingleton)
// whose member-issue CloseIssue call failed. It durably records the outstanding close (a
// GitHub label survives an engine restart, unlike an itemstate.Store-only marker — there is
// no artifact to safely "redo" here except the call itself) so settleMergeTrainMemberCloses
// can retry it every poll, independent of the item's board column. By the time this call can
// fail, item.Status is already (or will eventually be) "Done" — a materially different
// starting condition than ADR-060's fabrik:awaiting-done, which is written before the
// Done-move. See ADR-061.
const mergeTrainAwaitingMemberCloseLabel = "fabrik:awaiting-member-close"

// mergeTrainMemberCloseRetryStage is a dedicated, non-real stage name used to key the
// existing StageRetryIncremented/StageRetryCleared/Attempts counter for retries of a
// stalled merge-train member-issue close — mirrors noWorkNeededRetryStage. The
// double-underscore wrapping makes it unrepresentable as a real YAML stage `name:`, so it
// can never collide with a configured stage's retry count.
const mergeTrainMemberCloseRetryStage = "__merge_train_member_close__"

// markMergeTrainMemberCloseOutstanding records that landSingleton's member-issue CloseIssue
// call failed, so settleMergeTrainMemberCloses retries it on a later poll. Idempotent — a
// no-op if the marker is already present.
func (e *Engine) markMergeTrainMemberCloseOutstanding(item gh.ProjectItem, owner, repo string) {
	if hasLabel(item, mergeTrainAwaitingMemberCloseLabel) {
		return
	}
	e.addLabel(item, mergeTrainAwaitingMemberCloseLabel)
}

// settleMergeTrainMemberCloses is the per-poll settle scan for the merge-train member-issue
// close retry (ADR-061). It runs unconditionally every poll — independent of merge_train:
// on/off — over the raw board snapshot (not deepFetchCandidates): the item has already
// reached its terminal singleton-landing outcome by the time the marker is written, so the
// ADR-060 dispatch-suppression/terminal-skip machinery built around deepFetchCandidates and
// itemMayNeedWork/itemNeedsWork has nothing to do here and would only add risk (a naive
// port would need the marker added to transientLifecycleLabels to survive the terminal-skip
// interaction — this scan sidesteps that entirely by not depending on either mechanism).
func (e *Engine) settleMergeTrainMemberCloses(board *gh.ProjectBoard) {
	for _, item := range board.Items {
		if !hasLabel(item, mergeTrainAwaitingMemberCloseLabel) || hasLabel(item, "fabrik:paused") {
			continue
		}
		e.settleMergeTrainMemberClose(item)
	}
}

// settleMergeTrainMemberClose retries the outstanding member-issue CloseIssue call for a
// single item. Idempotent: if the issue is already closed (e.g. GitHub's own Closes #N
// auto-close finally landed), it skips the redundant call and just clears the marker.
func (e *Engine) settleMergeTrainMemberClose(item gh.ProjectItem) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	if item.IsClosed {
		e.logf(item.Number, "merge-train", "member issue #%d already closed — clearing awaiting-member-close marker\n", item.Number)
		e.clearMergeTrainMemberCloseMarker(item, owner, repo)
		return
	}

	if err := e.client.CloseIssue(owner, repo, item.Number); err != nil {
		e.logf(item.Number, "merge-train", "retry: could not close member issue #%d: %v\n", item.Number, err)
		e.recordMergeTrainMemberCloseRetry(item)
		return
	}

	if c := e.cache(); c != nil {
		c.ApplyIssueClosed(boardcache.ItemKey(item.Repo, item.Number))
	}
	e.logf(item.Number, "merge-train", "closed member issue #%d (retry)\n", item.Number)
	e.clearMergeTrainMemberCloseMarker(item, owner, repo)
}

// recordMergeTrainMemberCloseRetry increments the in-memory retry counter for a stalled
// merge-train member-issue close, keyed by the dedicated mergeTrainMemberCloseRetryStage
// constant. Escalates via escalateMergeTrainMemberCloseFailure once e.cfg.MaxRetries is
// reached. Mirrors recordNoWorkNeededRetry, including its MaxRetries<=0 (unlimited retries,
// never escalate) guard.
func (e *Engine) recordMergeTrainMemberCloseRetry(item gh.ProjectItem) {
	if e.cfg.MaxRetries <= 0 {
		return
	}
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.StageRetryIncremented{Repo: repoStr, Number: item.Number, StageName: mergeTrainMemberCloseRetryStage})
	var count int
	if snap, err := e.store.Get(repoStr, item.Number); err == nil {
		count = snap.Attempts(mergeTrainMemberCloseRetryStage)
	}
	if count >= e.cfg.MaxRetries {
		e.escalateMergeTrainMemberCloseFailure(item)
	}
}

// escalateMergeTrainMemberCloseFailure is called when the outstanding merge-train
// member-issue close has failed MaxRetries times. It pauses the issue (fabrik:paused),
// removes the awaiting-member-close marker (retry suppression is no longer needed once
// fabrik:paused takes over — groupQueuedByRepo and this scan's own paused-item guard both
// leave paused items alone), and posts an explanatory comment with the manual recovery
// step — mirroring escalateNoWorkNeededFailure.
func (e *Engine) escalateMergeTrainMemberCloseFailure(item gh.ProjectItem) {
	e.logf(item.Number, "escalate", "merge-train member-issue close failed %d time(s) — pausing issue\n", e.cfg.MaxRetries)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	e.addLabel(item, "fabrik:paused")
	e.applyLabelRemove(item, mergeTrainAwaitingMemberCloseLabel, true)

	comment := fmt.Sprintf(
		"🏭 **Fabrik — merge-train member-issue close failed**\n\nThis issue landed successfully via the merge-train singleton path, but closing the issue itself could not be completed after %d attempt(s). The issue has been paused.\n\nManual fix:\n```\ngh issue close %d --repo %s/%s\n```\nThen remove the `fabrik:paused` label.",
		e.cfg.MaxRetries, item.Number, owner, repo,
	)
	e.postItemComment(item, comment, true)

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: mergeTrainMemberCloseRetryStage})
}

// clearMergeTrainMemberCloseMarker removes the awaiting-member-close marker and clears the
// retry counter once the member issue is confirmed closed (by us or by GitHub's own
// Closes #N auto-close).
func (e *Engine) clearMergeTrainMemberCloseMarker(item gh.ProjectItem, owner, repo string) {
	if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, mergeTrainAwaitingMemberCloseLabel); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "could not remove %s marker: %v\n", mergeTrainAwaitingMemberCloseLabel, err)
		return
	} else if err == nil {
		e.syncLabelRemoval(item, mergeTrainAwaitingMemberCloseLabel, true)
	}

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: mergeTrainMemberCloseRetryStage})
}
