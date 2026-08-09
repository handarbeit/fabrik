package engine

import (
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// awaitingAdvanceLabel marks an item whose terminal-advance call
// (advanceToNextStage, invoked from advanceValidateTerminalItem's merged-PR
// path and advanceConvergedPRToDone) failed to move the project board Status
// forward — most commonly because the target stage's Status option does not
// exist on the board (#1422). Unlike fabrik:awaiting-done (ADR-060), which is
// applied unconditionally before the Done-move is attempted, this label is
// applied only in the failure branch: advanceToNextStage is the last
// mutation at both call sites, so by the time it can fail, every other side
// effect (gate-label clearing, completion-label filling,
// fabrik:auto-merge-enabled removal) has already landed — the same shape as
// fabrik:awaiting-close (ADR-1097). Durable (a GitHub label, not an
// itemstate.Store-only marker) so a stranded item survives an engine restart
// and is picked up by settleAwaitingAdvanceScan without operator
// intervention beyond fixing the underlying board misconfiguration.
const awaitingAdvanceLabel = "fabrik:awaiting-advance"

// advanceAwaitingRetryStage is a dedicated, non-real stage name used to key
// the existing StageRetryIncremented/StageRetryCleared/Attempts counter for
// retries of a stalled terminal advance — mirrors nonDefaultBaseCloseRetryStage.
// Double-underscore-wrapped so it can never collide with a configured
// stage's own retry count.
const advanceAwaitingRetryStage = "__awaiting_advance__"

// awaitingAdvanceStuckOrReset guards advanceValidateTerminalItem's merged-PR
// branch against the loop Pruefer flagged in PR #1469 review: that branch
// unconditionally strips fabrik:paused/fabrik:awaiting-input and retries the
// terminal advance every single poll, regardless of *why* fabrik:paused is
// present — including a pause escalateAwaitingAdvanceFailure itself just
// applied. Without this guard, the very next poll strips the pause right
// back off, retries the still-failing advance, fails, and re-escalates —
// posting a fresh pair of comments and re-pausing every poll, forever,
// instead of settling into a stable paused state.
//
// Detects the specific "still escalated, not yet resolved by an operator"
// state via the advanceAwaitingRetryStage attempt counter (nothing else
// touches it) rather than fabrik:paused alone — a merged item can
// legitimately carry fabrik:paused for an unrelated, stale reason (e.g. a
// review/rebase-cycle-limit pause the merge itself supersedes), which this
// self-heal path is deliberately designed to clear and retry through. A
// blanket "skip whenever paused" guard would regress that pre-existing,
// tested behavior.
//
//   - Paused AND counter >= MaxRetries: still escalated, unresolved by an
//     operator — returns true so the caller skips the whole branch untouched
//     (no relabeling, no retry, no comment).
//   - Not paused AND counter >= MaxRetries: an operator has removed
//     fabrik:paused since the last escalation — delegates to
//     awaitingAdvanceResetIfUnpaused for a fresh MaxRetries budget, then
//     returns false so the caller proceeds normally.
//   - Otherwise (counter below MaxRetries, or MaxRetries <= 0 meaning
//     unlimited/never-escalate): returns false — nothing to do.
func (e *Engine) awaitingAdvanceStuckOrReset(item gh.ProjectItem) bool {
	if e.cfg.MaxRetries <= 0 {
		return false
	}
	if !hasLabel(item.Labels, "fabrik:paused") {
		e.awaitingAdvanceResetIfUnpaused(item)
		return false
	}
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	snap, err := e.store.Get(repoStr, item.Number)
	if err != nil {
		// Fail open (proceed with self-heal), matching every other
		// e.store.Get error-handling site in this package — but log it: a
		// persistent store failure here would otherwise silently reintroduce
		// the exact unpause/retry/re-escalate loop this guard exists to
		// prevent (Pruefer, PR #1469 review), with no visible signal.
		e.logf(item.Number, "warn", "awaiting-advance: could not read retry state: %v — proceeding without the stuck guard\n", err)
		return false
	}
	return snap.Attempts(advanceAwaitingRetryStage) >= e.cfg.MaxRetries
}

// awaitingAdvanceResetIfUnpaused resets the advanceAwaitingRetryStage retry
// counter when it is already at or above MaxRetries but item is not (or is
// no longer) carrying fabrik:paused — the signal that an operator has
// removed the pause since the last escalation. Mirrors clearFailedStage's
// StageRetryCleared, scoped to this synthetic retry key. No-op otherwise
// (including MaxRetries <= 0, unlimited/never-escalate).
//
// Shared by awaitingAdvanceStuckOrReset (advanceValidateTerminalItem's call
// site, which also needs the "still paused, still stuck" half above) and
// settleAwaitingAdvanceScan directly (whose own pre-filter already excludes
// paused items, so only this reset half is ever relevant there). Without
// this, a settle-scan retry issued after a manual unpause would immediately
// re-escalate on its very first failure instead of getting a fresh budget —
// Pruefer's second PR #1469 review finding, which also applies to this
// scan's own retries, not just the direct call sites.
func (e *Engine) awaitingAdvanceResetIfUnpaused(item gh.ProjectItem) {
	if e.cfg.MaxRetries <= 0 {
		return
	}
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	snap, err := e.store.Get(repoStr, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "awaiting-advance: could not read retry state: %v — leaving retry counter as-is\n", err)
		return
	}
	if snap.Attempts(advanceAwaitingRetryStage) >= e.cfg.MaxRetries {
		e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: advanceAwaitingRetryStage})
	}
}

// recordAdvanceOutcome wraps advanceToNextStage so both terminal-advance call
// sites (advanceValidateTerminalItem's merged-PR path, and
// advanceConvergedPRToDone) get identical failure escalation and success
// recovery for free (R1-R3, R6). On failure it applies awaitingAdvanceLabel
// and posts a one-time explanatory comment (gated on the label's own prior
// absence — R5), then records a settle-retry pass; on success it clears any
// outstanding marker left by a prior failed pass. Callers keep their own
// existing warn log line unchanged — this only adds the durable
// label/comment/retry side effects around the same call.
func (e *Engine) recordAdvanceOutcome(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) error {
	err := e.advanceToNextStage(board, item, stage)
	if err != nil {
		e.markAdvanceFailureOutstanding(item, stage, err)
		return err
	}
	if hasLabel(item.Labels, awaitingAdvanceLabel) {
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		e.clearAwaitingAdvanceMarker(item, owner, repo)
	}
	return nil
}

// markAdvanceFailureOutstanding records that a terminal advance failed: posts
// the one-time comment naming the failing stage and the underlying error
// (which, for the "no status option" case advanceToNextStage itself returns,
// already names the missing option and the options that do exist — R1, no
// new typed-error plumbing needed), gated on the label's own absence so
// repeated failures in the same episode never produce more than one comment
// (R5). Every failing pass — first or repeat — counts toward the bounded
// retry budget via recordSettleRetry (R3).
func (e *Engine) markAdvanceFailureOutstanding(item gh.ProjectItem, stage *stages.Stage, aerr error) {
	if !hasLabel(item.Labels, awaitingAdvanceLabel) {
		comment := fmt.Sprintf(
			"🏭 **Fabrik — terminal advance stuck**\n\nStage **%s** is complete and could not move the project-board Status forward: %v\n\nThis issue is stranded — its own board Status will not advance and, because dependency edges clear on close (not on merge), any issue that lists this one as a blocker will keep waiting on work that has already shipped. Fabrik will keep retrying automatically every poll; adding the missing Status option (or otherwise fixing the board) is enough to unstick it — no engine restart or manual re-dispatch needed.",
			stage.Name, aerr,
		)
		e.postItemComment(item, comment, false)
		e.addLabel(item, awaitingAdvanceLabel)
	}
	e.recordSettleRetry(item, advanceAwaitingRetryStage, e.escalateAwaitingAdvanceFailure)
}

// escalateAwaitingAdvanceFailure is called when the outstanding terminal
// advance has failed e.cfg.MaxRetries times (R3). Pauses the issue and posts
// an explanatory comment, mirroring escalateNonDefaultBaseCloseFailure's
// shape — but deliberately does NOT reuse the shared escalateSettle helper,
// and deliberately does NOT remove awaitingAdvanceLabel the way every other
// settle scan's escalation does.
//
// escalateSettle's doc comment justifies removing the marker on escalation
// with "dispatch/retry suppression is no longer needed once fabrik:paused
// takes over" — true for its other six callers only because each of THEIR
// direct call sites unconditionally re-creates the marker on its very next
// relevant poll regardless of pause state (e.g. closeIssueIfNonDefaultBase,
// ADR-1097). Ours does not: advanceConvergedPRToDone (merge_gate.go) fires
// at most once per episode — removing fabrik:auto-merge-enabled up front
// structurally prevents checkAutoMergeConvergence from ever re-entering it
// (see awaitingAdvanceStuckOrReset's doc comment). For an item whose *only*
// driver is settleAwaitingAdvanceScan, removing the marker here would
// permanently strand it: nothing would ever re-add it, even after an
// operator fixes the board and removes fabrik:paused (Pruefer, PR #1469
// review). Keeping the marker in place is harmless while paused —
// settleAwaitingAdvanceScan's own guard already skips any item carrying
// fabrik:paused — and is exactly what lets that scan pick the item back up,
// with a fresh retry budget (awaitingAdvanceResetIfUnpaused), the moment an
// operator removes the pause and nothing else.
func (e *Engine) escalateAwaitingAdvanceFailure(item gh.ProjectItem) {
	e.logf(item.Number, "escalate", "terminal advance failed %d time(s) — pausing issue\n", e.cfg.MaxRetries)

	e.addLabel(item, "fabrik:paused")
	comment := fmt.Sprintf(
		"🏭 **Fabrik — terminal advance failed repeatedly**\n\nFabrik could not move this issue's project-board Status forward after %d attempt(s). The issue has been paused.\n\nCheck that every stage name in your stage config has a matching Status option on the project board, then remove the `fabrik:paused` label to resume.",
		e.cfg.MaxRetries,
	)
	e.postItemComment(item, comment, true)

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: advanceAwaitingRetryStage})
}

// clearAwaitingAdvanceMarker removes the awaiting-advance marker and clears
// the retry counter once a terminal advance succeeds.
func (e *Engine) clearAwaitingAdvanceMarker(item gh.ProjectItem, owner, repo string) {
	e.clearSettleMarker(item, owner, repo, awaitingAdvanceLabel, advanceAwaitingRetryStage)
}

// settleAwaitingAdvanceScan is the per-poll settle scan for a stranded
// terminal advance (R2). Runs over the raw board snapshot (board.Items), not
// deepFetchCandidates — a stranded item's stage is already complete, and
// itemMayNeedWork/selectDeepFetchCandidates does not reliably admit it (this
// is exactly the fragility Research identified for advanceConvergedPRToDone's
// path: it self-heals only as long as nothing else in the admission pipeline
// claims the item first).
//
// Shares the poll's advancedItems map with
// runValidatePRTerminalAdvance/settleClosedValidateAdvance: for a closed item
// stuck at Validate, that pair already retries advanceToNextStage
// unconditionally every poll and will have already set advancedItems[iKey]
// before this scan runs later in the same poll pass, so this scan is a
// harmless, correctly-skipped no-op there — it is the exclusive retry-owner
// only for the two genuine gaps (an open item admission-gated out of
// runValidatePRTerminalAdvance's deepFetchCandidates source, and
// advanceConvergedPRToDone's path, which is reachable only via the Phase 1
// catch-up handler chain and is otherwise unowned when that chain doesn't
// reach it on a given poll).
func (e *Engine) settleAwaitingAdvanceScan(board *gh.ProjectBoard, advancedItems map[string]bool) {
	for _, item := range board.Items {
		if !hasLabel(item.Labels, awaitingAdvanceLabel) || hasLabel(item.Labels, "fabrik:paused") {
			continue
		}
		iKey := issueKey(item, e.defaultRepo())
		if advancedItems[iKey] {
			continue
		}
		stage := stages.FindStage(e.cfg.Stages, item.Status)
		if stage == nil {
			e.logf(item.Number, "warn", "awaiting-advance: no stage configured for board status %q — skipping\n", item.Status)
			continue
		}
		// Reset a stale post-escalation counter before retrying: an item can
		// reach here still carrying awaitingAdvanceLabel with Attempts already
		// at MaxRetries if an operator removed fabrik:paused after a prior
		// escalation — escalateAwaitingAdvanceFailure deliberately leaves this
		// marker in place (see its doc comment) so this scan can still see the
		// item, but without this reset the retry below would immediately
		// re-escalate instead of getting a fresh budget.
		e.awaitingAdvanceResetIfUnpaused(item)
		if err := e.recordAdvanceOutcome(board, item, stage); err != nil {
			e.logf(item.Number, "awaiting-advance", "retry: could not advance: %v\n", err)
		} else {
			e.logf(item.Number, "awaiting-advance", "advance succeeded (retry)\n")
		}
		advancedItems[iKey] = true
	}
}
