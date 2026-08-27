package engine

import (
	"fmt"
	"strconv"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// runawayAlertMarkerLabel marks a merge-train member that fireRunawayGuard already paused
// (fabrik:paused + fabrik:awaiting-input applied) but whose explanatory alert comment could
// not be posted (a transient AddComment failure — network error, rate limit, etc.). It
// durably records the outstanding alert so settleRunawayGuardAlertScan can retry it every
// poll, independent of the item's board column or the fireRunawayGuard call that paused it
// ever running again for this member. Unlike the other ADR-060-family markers
// (fabrik:awaiting-member-close, fabrik:awaiting-close), fabrik:paused is already present
// from the very first application of this marker — the pause itself succeeded; only the
// alert delivery is outstanding. See ADR-1533, #1533.
const runawayAlertMarkerLabel = "fabrik:awaiting-runaway-alert"

// runawayAlertRetryStage is a dedicated, non-real stage name used to key the existing
// StageRetryIncremented/StageRetryCleared/Attempts counter for retries of a stalled runaway
// guard alert — mirrors mergeTrainMemberCloseRetryStage/nonDefaultBaseCloseRetryStage. The
// double-underscore wrapping makes it unrepresentable as a real YAML stage `name:`, so it
// can never collide with a configured stage's retry count.
const runawayAlertRetryStage = "__awaiting_runaway_alert__"

// markRunawayAlertOutstanding records that fireRunawayGuard's AddComment call failed for
// item, so settleRunawayGuardAlertScan retries it on a later poll. Idempotent — a no-op if
// the marker is already present.
func (e *Engine) markRunawayAlertOutstanding(item gh.ProjectItem, owner, repo string) {
	if hasLabel(item.Labels, runawayAlertMarkerLabel) {
		return
	}
	e.addLabel(item, runawayAlertMarkerLabel)
}

// mergeTrainKeyForItem reconstructs the composite (repo,base) trainKey fireRunawayGuard
// used when it originally alerted item (#1648). By the time this settle scan runs the
// member is already paused, so unlike a live dispatch there is no partition context handed
// down — the base must be re-resolved from the item itself, mirroring the wm-lookup pattern
// the now-removed nonDefaultBaseExclusion (#1647) used before this issue superseded it.
// Fails if the repo's WorktreeManager isn't registered (should be unreachable in practice:
// fireRunawayGuard's own caller already resolved this item's base successfully to get here
// in the first place) or if baseBranchForItem itself errors.
func (e *Engine) mergeTrainKeyForItem(item gh.ProjectItem) (string, error) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoKey := owner + "/" + repo
	e.mu.Lock()
	wm, ok := e.worktreeManagers[repoKey]
	e.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("no WorktreeManager registered for %s", repoKey)
	}
	base, err := e.baseBranchForItem(item, wm)
	if err != nil {
		return "", fmt.Errorf("resolving base branch for #%d: %w", item.Number, err)
	}
	return mergeTrainKey(repoKey, base), nil
}

// settleRunawayGuardAlertScan is the per-poll settle scan for the runaway-guard alert retry
// (ADR-1533, R1). It runs unconditionally every poll, sourced from the raw board snapshot
// (not deepFetchCandidates or groupQueuedByRepo): a member carrying this marker also
// carries fabrik:paused, which both of those exclude — the item has already reached its
// terminal "paused, awaiting operator" state by the time the marker is written, so the
// ADR-060 dispatch-suppression machinery has nothing to do here and would only hide the very
// items this scan exists to find.
func (e *Engine) settleRunawayGuardAlertScan(board *gh.ProjectBoard) {
	for _, item := range board.Items {
		if !hasLabel(item.Labels, runawayAlertMarkerLabel) {
			continue
		}
		e.settleRunawayGuardAlert(item)
	}
}

// settleRunawayGuardAlert retries the outstanding runaway-guard alert comment for a single
// member. The count/window in the retried message are re-read live via isRunawayTripped/
// effectiveTrialWindow — no storage persists the original firing's exact count, and a live
// re-read is simple and accurate enough for the retry comment's wording (the important facts
// — that the member is paused, and why — don't change based on which count is shown).
//
// Runs under mergeTrainRunawayMu for its entire body, not just the map update after a
// successful post. fireRunawayGuard's own doc comment claims "the whole pause+alert sequence
// is ... a single critical section ... two concurrent calls can never interleave" — that
// invariant only holds if every caller that can post this exact comment for the same member
// participates in the same critical section. A member carrying runawayAlertMarkerLabel is
// excluded from Hook 2's groupQueuedByRepoAndBase (it already has fabrik:paused), but Hook 1's
// current/survivors is the worker's own in-flight member list, not a fresh board read — it
// can still include this member if the worker hasn't ejected it, so a stale Hook 1 call can
// legitimately reprocess a member that already picked up the marker earlier in the same
// worker run. Without holding the lock across the comment post here, that Hook 1 call and
// this settle retry could both observe mergeTrainRunawayAlerted[alertKey] == false and post
// the alert concurrently — a duplicate alert, violating R2/A3.
func (e *Engine) settleRunawayGuardAlert(item gh.ProjectItem) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	trainKey, kerr := e.mergeTrainKeyForItem(item)
	if kerr != nil {
		e.logf(item.Number, "merge-train", "retry: could not resolve base branch to reconstruct runaway alert key: %v — will retry next poll\n", kerr)
		e.recordRunawayAlertRetry(item)
		return
	}
	alertKey := trainKey + "#" + strconv.Itoa(item.Number)

	e.mergeTrainRunawayMu.Lock()
	defer e.mergeTrainRunawayMu.Unlock()

	count, _ := e.isRunawayTripped(trainKey)
	_, window := e.effectiveTrialWindow()

	if recordedCount, ok := e.mergeTrainRunawayAlerted[alertKey]; ok && count <= recordedCount {
		// A racing fireRunawayGuard call already delivered the alert for this member
		// within this episode (e.g. it succeeded after this item picked up the marker
		// but before this settle pass ran) — just clear the now-stale marker. See
		// fireRunawayGuard's doc comment for why the comparison is by trial count, not
		// a plain boolean (#1533 review, finding 2).
		e.clearRunawayAlertMarker(item, owner, repo)
		return
	}

	if _, err := e.postComment(item, runawayGuardAlertMessage(count, trainKey, window), false, true); err != nil {
		e.logf(item.Number, "merge-train", "retry: could not post runaway guard alert: %v\n", err)
		e.recordRunawayAlertRetry(item)
		return
	}

	e.mergeTrainRunawayAlerted[alertKey] = count
	e.logf(item.Number, "merge-train", "posted runaway guard alert (retry)\n")
	e.clearRunawayAlertMarker(item, owner, repo)
}

// recordRunawayAlertRetry increments the in-memory retry counter for a stalled runaway-guard
// alert, keyed by the dedicated runawayAlertRetryStage constant. Escalates via
// escalateRunawayAlertFailure once e.cfg.MaxRetries is reached. Mirrors
// recordMergeTrainMemberCloseRetry, including its MaxRetries<=0 (unlimited retries, never
// escalate) guard.
func (e *Engine) recordRunawayAlertRetry(item gh.ProjectItem) {
	e.recordSettleRetry(item, runawayAlertRetryStage, e.escalateRunawayAlertFailure)
}

// escalateRunawayAlertFailure is called when the outstanding runaway-guard alert has failed
// to post MaxRetries times. The member is already fabrik:paused (fireRunawayGuard's own
// pause application, unaffected by the comment failure) — reapplying it here is a harmless
// no-op — and it attempts a fallback comment carrying the same explanation the original alert
// would have, so the member is never left paused with zero delivered explanation.
//
// Unlike the sibling settle scans, this deliberately does NOT delegate to the shared
// escalateSettle helper. escalateSettle unconditionally removes the durable marker and
// swallows its postComment closure's error (via postItemComment, whose return value every
// other escalateSettle caller discards) — appropriate when the escalation comment is purely
// informational and the marker's only job was retry-suppression. Here the fallback comment
// *is* the last remaining delivery of the explanation R1 requires; if it also fails (a
// persistent AddComment outage — lost token permission, a secondary rate limit outlasting
// MaxRetries polls), unconditionally removing the marker and marking the member alerted would
// erase the only remaining signal that the alert never landed and permanently suppress every
// further retry this episode — reproducing #1533 itself through the very machinery meant to
// fix it (#1533 review, finding 1). So the marker and the alerted-map entry are only touched
// on confirmed success; on failure the marker stays, and the next settleRunawayGuardAlertScan
// pass retries the primary alert (then, on renewed failure, this fallback) again next poll —
// indefinitely, until a comment actually lands.
//
// The alerted-map write on success is safe here for the same reason as before this rewrite:
// escalateRunawayAlertFailure's only caller is settleRunawayGuardAlert's
// recordRunawayAlertRetry, reached while that function still holds mergeTrainRunawayMu.
func (e *Engine) escalateRunawayAlertFailure(item gh.ProjectItem) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	trainKey, kerr := e.mergeTrainKeyForItem(item)
	if kerr != nil {
		e.logf(item.Number, "escalate", "runaway guard fallback comment: could not resolve base branch to reconstruct alert key: %v — marker stays, will keep retrying\n", kerr)
		return
	}
	alertKey := trainKey + "#" + strconv.Itoa(item.Number)
	count, _ := e.isRunawayTripped(trainKey)
	_, window := e.effectiveTrialWindow()

	// fabrik:paused is unconditional and idempotent, mirroring fireRunawayGuard's own
	// "pause never depends on the comment succeeding" contract.
	e.addLabel(item, "fabrik:paused")

	comment := fmt.Sprintf(
		"🏭 **Fabrik merge-train — runaway guard alert delivery failed**\n\n"+
			"This member was paused by the merge-train runaway guard, but Fabrik could "+
			"not post the explanatory alert comment after %d attempt(s). Posting this "+
			"fallback notice instead so the pause is not left unexplained.\n\n"+
			"%s",
		e.cfg.MaxRetries, runawayGuardAlertMessage(count, trainKey, window),
	)

	if _, err := e.postComment(item, comment, false, true); err != nil {
		e.logf(item.Number, "escalate", "runaway guard fallback comment also failed after %d attempt(s): %v — marker stays, will keep retrying\n", e.cfg.MaxRetries, err)
		return
	}

	e.logf(item.Number, "escalate", "runaway guard alert failed to post %d time(s) — posted fallback comment\n", e.cfg.MaxRetries)
	e.mergeTrainRunawayAlerted[alertKey] = count
	e.clearRunawayAlertMarker(item, owner, repo)

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: runawayAlertRetryStage})
}

// clearRunawayAlertMarker removes the awaiting-runaway-alert marker and clears the retry
// counter once the alert comment is confirmed posted (by settleRunawayGuardAlert or by a
// racing fireRunawayGuard call succeeding for the same member first).
func (e *Engine) clearRunawayAlertMarker(item gh.ProjectItem, owner, repo string) {
	e.clearSettleMarker(item, owner, repo, runawayAlertMarkerLabel, runawayAlertRetryStage)
}
