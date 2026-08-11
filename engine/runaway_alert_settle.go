package engine

import (
	"fmt"
	"strconv"

	gh "github.com/handarbeit/fabrik/github"
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
// excluded from Hook 2's groupQueuedByRepo (it already has fabrik:paused), but Hook 1's
// current/survivors is the worker's own in-flight member list, not a fresh board read — it
// can still include this member if the worker hasn't ejected it, so a stale Hook 1 call can
// legitimately reprocess a member that already picked up the marker earlier in the same
// worker run. Without holding the lock across the comment post here, that Hook 1 call and
// this settle retry could both observe mergeTrainRunawayAlerted[alertKey] == false and post
// the alert concurrently — a duplicate alert, violating R2/A3.
func (e *Engine) settleRunawayGuardAlert(item gh.ProjectItem) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoKey := owner + "/" + repo
	alertKey := repoKey + "#" + strconv.Itoa(item.Number)

	e.mergeTrainRunawayMu.Lock()
	defer e.mergeTrainRunawayMu.Unlock()

	if e.mergeTrainRunawayAlerted[alertKey] {
		// A racing fireRunawayGuard call already delivered the alert for this member
		// within this episode (e.g. it succeeded after this item picked up the marker
		// but before this settle pass ran) — just clear the now-stale marker.
		e.clearRunawayAlertMarker(item, owner, repo)
		return
	}

	count, _ := e.isRunawayTripped(repoKey)
	_, window := e.effectiveTrialWindow()

	if _, err := e.postComment(item, runawayGuardAlertMessage(count, repoKey, window), false, true); err != nil {
		e.logf(item.Number, "merge-train", "retry: could not post runaway guard alert: %v\n", err)
		e.recordRunawayAlertRetry(item)
		return
	}

	e.mergeTrainRunawayAlerted[alertKey] = true
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
// pause application, unaffected by the comment failure) — escalateSettle's own
// fabrik:paused add is therefore a no-op here — but it removes the awaiting-runaway-alert
// marker (retry suppression is no longer needed once the fallback comment below has
// been posted) and posts a fallback comment carrying the same explanation the original
// alert would have, so the member is never left paused with zero delivered explanation.
//
// Also marks the member alerted in mergeTrainRunawayAlerted (#1533 review): this is the
// only caller of escalateRunawayAlertFailure, and it is always reached from
// settleRunawayGuardAlert's recordRunawayAlertRetry call while that function still holds
// mergeTrainRunawayMu — so writing to the map here is safe, and skipping it would leave a
// member that only ever received the fallback comment indistinguishable, map-wise, from one
// that was never alerted at all. Without this, a stale Hook 1 call still holding this member
// in its own in-flight items slice (fireRunawayGuard's own doc comment notes current/
// survivors isn't re-derived from a fresh board read) would find the map entry false and
// post a second, duplicate alert on top of the fallback one — violating R2/A3.
func (e *Engine) escalateRunawayAlertFailure(item gh.ProjectItem) {
	e.logf(item.Number, "escalate", "runaway guard alert failed to post %d time(s) — posting fallback comment\n", e.cfg.MaxRetries)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoKey := owner + "/" + repo
	alertKey := repoKey + "#" + strconv.Itoa(item.Number)
	count, _ := e.isRunawayTripped(repoKey)
	_, window := e.effectiveTrialWindow()

	e.escalateSettle(item, runawayAlertMarkerLabel, runawayAlertRetryStage, func(item gh.ProjectItem) {
		comment := fmt.Sprintf(
			"🏭 **Fabrik merge-train — runaway guard alert delivery failed**\n\n"+
				"This member was paused by the merge-train runaway guard, but Fabrik could "+
				"not post the explanatory alert comment after %d attempt(s). Posting this "+
				"fallback notice instead so the pause is not left unexplained.\n\n"+
				"%s",
			e.cfg.MaxRetries, runawayGuardAlertMessage(count, repoKey, window),
		)
		e.postItemComment(item, comment, false)
	})

	e.mergeTrainRunawayAlerted[alertKey] = true
}

// clearRunawayAlertMarker removes the awaiting-runaway-alert marker and clears the retry
// counter once the alert comment is confirmed posted (by settleRunawayGuardAlert or by a
// racing fireRunawayGuard call succeeding for the same member first).
func (e *Engine) clearRunawayAlertMarker(item gh.ProjectItem, owner, repo string) {
	e.clearSettleMarker(item, owner, repo, runawayAlertMarkerLabel, runawayAlertRetryStage)
}
