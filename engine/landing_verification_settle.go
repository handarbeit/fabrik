package engine

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// awaitingLandingVerificationLabel marks an item that just reached Done via a
// merge-attributable path (merge-train batch, merge-train singleton, or the
// ordinary auto-merge path) and needs its credited PR's merge state confirmed
// (#1616). Applied at all three Done-transition call sites, immediately after
// their respective advanceToNextStage succeeds. Cleared once
// settleLandingVerification confirms the credited PR merged, or removed (with
// fabrik:paused taking over) once the inconclusive regime exhausts its retry
// budget — see landingVerificationRetryStage.
//
// This is the settle scan's own admission filter, sourced from raw
// board.Items (ADR-1270 precedent), never deepFetchCandidates: the item's own
// stage is already gate-complete (it reached Done) by the time this marker is
// written, so the ordinary dispatch/admission machinery has nothing to do
// here. Old Done items — anything that landed before this feature shipped —
// never had this marker applied, so they are naturally out of scope: no
// backfill, no ambiguity about pre-feature merge-train landings whose member
// PRs are legitimately closed-not-merged.
const awaitingLandingVerificationLabel = "fabrik:awaiting-landing-verification"

// creditedPRLabelPrefix labels record the specific PR credited for a
// merge-train item's Done transition — the integration PR for a batch landing
// (landMergeTrainBatch) or the dedicated singleton landing PR (landSingleton).
// Neither is the item's own linked PR (which stays closed-not-merged in both
// cases), so it can't be rediscovered later via FetchLinkedPR the way the
// ordinary auto-merge path's credited PR can — this label is the durable
// record captured at the Done transition itself, read by
// settleLandingVerification. Not applied by the ordinary auto-merge path,
// whose credited PR is always its own linked PR.
const creditedPRLabelPrefix = "fabrik:credited-pr:"

// landingVerificationFailedLabel is R2's distinguishing label: applied only
// on a confirmed failure (the credited PR exists and is definitively not
// merged), alongside reopening the issue, moving it off Done, and posting an
// explanatory comment. Lets an operator tell this apart from an ordinary
// stage failure at a glance.
const landingVerificationFailedLabel = "fabrik:landing-verification-failed"

// landingVerificationRetryStage is a dedicated, non-real stage name used to
// key the existing StageRetryIncremented/StageRetryCleared/Attempts counter
// for retries of an *inconclusive* landing verification (API error, or no
// crediting reference could be found at all) — mirrors
// nonDefaultBaseCloseRetryStage/advanceAwaitingRetryStage. Double-underscore
// wrapped so it can never collide with a configured stage's own retry count.
//
// Governs only the inconclusive regime. A *confirmed* failure (the credited
// PR definitively did not merge) fires immediately via failLandingVerification
// — never gated by this counter — so AC1's "within one poll cycle" holds
// regardless of MaxRetries.
const landingVerificationRetryStage = "__landing_verification__"

// creditedPRFromLabel scans item's labels for a fabrik:credited-pr:<N> label
// and returns the recorded PR number. If multiple are present (should not
// happen in practice — only one landing call site ever applies this per
// item), the first with a valid positive integer suffix wins. Returns
// (0, false) when no such label is present or none parses.
func creditedPRFromLabel(item gh.ProjectItem) (int, bool) {
	for _, label := range item.Labels {
		if !strings.HasPrefix(label, creditedPRLabelPrefix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(label, creditedPRLabelPrefix))
		if err != nil || n <= 0 {
			continue
		}
		return n, true
	}
	return 0, false
}

// settleLandingVerification is the per-poll settle scan for #1616's post-Done
// landing-verification backstop (R5, AC1-AC4). Runs unconditionally every
// poll over the raw board snapshot (not deepFetchCandidates), mirroring every
// other settle-scan sibling documented in CLAUDE.md's ADR-1270 family — the
// item has already reached Done by the time the marker is written, so the
// ordinary dispatch-suppression/admission machinery has nothing to do here.
func (e *Engine) settleLandingVerification(board *gh.ProjectBoard) {
	for _, item := range board.Items {
		if !hasLabel(item.Labels, awaitingLandingVerificationLabel) || hasLabel(item.Labels, "fabrik:paused") {
			continue
		}
		e.settleLandingVerificationItem(board, item)
	}
}

// settleLandingVerificationItem resolves the credited PR for a single item
// and checks its merge state, per R1's chosen mechanism (merge-state of the
// crediting PR — strategy-agnostic, immune by construction to R3's
// deleted-after-merge and the confirmed rebase-after-landing false positives,
// since it never inspects the item's own branch).
//
// Two failure regimes, never conflated (see landingVerificationRetryStage's
// doc comment): a confirmed failure (the credited PR resolves and is
// definitively not merged) fires immediately via failLandingVerification; an
// inconclusive result (API error, or no crediting reference discoverable at
// all) goes through the bounded retry-then-escalate regime and never reopens
// the issue on ambiguity (AC4).
func (e *Engine) settleLandingVerificationItem(board *gh.ProjectBoard, item gh.ProjectItem) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	creditedPR, ok := creditedPRFromLabel(item)
	if !ok {
		// No fabrik:credited-pr:<N> label — this is either the ordinary
		// auto-merge path (whose credited PR is always its own linked PR, never
		// labeled) or a merge-train label write that failed outright. Either
		// way, FetchLinkedPR is the correct next attempt; a linked PR found here
		// belongs to the item itself, never to a merge-train integration/
		// singleton PR, so this branch is never reached for a merge-train item
		// whose label write actually succeeded.
		pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
		if err != nil {
			e.logf(item.Number, "landing-verification", "could not fetch linked PR: %v — inconclusive, retrying\n", err)
			e.recordLandingVerificationRetry(item)
			return
		}
		if pr == nil || pr.Number == 0 {
			e.logf(item.Number, "landing-verification", "no crediting reference found (no fabrik:credited-pr label, no linked PR) — inconclusive, retrying\n")
			e.recordLandingVerificationRetry(item)
			return
		}
		creditedPR = pr.Number
	}

	merged, err := e.client.FetchPRMerged(owner, repo, creditedPR)
	if err != nil {
		e.logf(item.Number, "landing-verification", "could not confirm merge state of credited PR #%d: %v — inconclusive, retrying\n", creditedPR, err)
		e.recordLandingVerificationRetry(item)
		return
	}
	if merged {
		e.logf(item.Number, "landing-verification", "credited PR #%d confirmed merged — landing verified\n", creditedPR)
		e.clearLandingVerificationMarkers(item, owner, repo)
		return
	}

	e.failLandingVerification(board, item, owner, repo, creditedPR)
}

// recordLandingVerificationRetry increments the inconclusive-regime retry
// counter, escalating to fabrik:paused at MaxRetries via
// escalateLandingVerificationFailure — never reopens the issue on ambiguity
// (R5, AC4).
func (e *Engine) recordLandingVerificationRetry(item gh.ProjectItem) {
	e.recordSettleRetry(item, landingVerificationRetryStage, e.escalateLandingVerificationFailure)
}

// escalateLandingVerificationFailure is called when landing verification has
// been inconclusive MaxRetries times in a row (a transient API failure, or no
// crediting reference could ever be found). Pauses the issue and posts a
// comment that is deliberately worded differently from
// failLandingVerification's confirmed-failure comment: this is ambiguity, not
// a confirmed lost landing, and an operator reading it must not conclude work
// was actually lost.
func (e *Engine) escalateLandingVerificationFailure(item gh.ProjectItem) {
	e.logf(item.Number, "escalate", "landing verification inconclusive %d time(s) — pausing issue\n", e.cfg.MaxRetries)
	e.escalateSettle(item, awaitingLandingVerificationLabel, landingVerificationRetryStage, func(item gh.ProjectItem) {
		comment := fmt.Sprintf(
			"🏭 **Fabrik — landing verification inconclusive**\n\nFabrik could not determine whether this issue's credited PR actually merged, after %d attempt(s). This is **not** a confirmed failure — it means the check itself could not complete (e.g. a transient API error, or no crediting PR reference could be found at all). The issue has **not** been reopened or moved off Done.\n\nManually confirm the credited PR's merge state, then remove the `fabrik:paused` label to resume — or, if you're confident the landing was fine, just remove `%s` and `fabrik:paused`.",
			e.cfg.MaxRetries, awaitingLandingVerificationLabel,
		)
		e.postItemComment(item, comment, true)
	})
}

// clearLandingVerificationMarkers removes the awaiting-landing-verification
// marker, clears its retry counter, and removes any fabrik:credited-pr:<N>
// label — the full set of state this feature writes — once the credited PR
// is confirmed merged (AC2).
func (e *Engine) clearLandingVerificationMarkers(item gh.ProjectItem, owner, repo string) {
	e.clearSettleMarker(item, owner, repo, awaitingLandingVerificationLabel, landingVerificationRetryStage)
	e.removeCreditedPRLabels(item)
}

// removeCreditedPRLabels removes every fabrik:credited-pr:<N> label present on
// item. In practice there is ever at most one (only one landing call site
// ever applies it per item), but this doesn't assume that.
func (e *Engine) removeCreditedPRLabels(item gh.ProjectItem) {
	for _, label := range item.Labels {
		if strings.HasPrefix(label, creditedPRLabelPrefix) {
			e.applyLabelRemove(item, label, true)
		}
	}
}

// failLandingVerification handles R2's confirmed-failure action: the credited
// PR was resolved and definitively did not merge. Fires immediately — never
// gated by the retry counter (AC1's "within one poll cycle"). Reopens the
// issue (if closed), moves the item's board Status back to Validate (a fixed
// target across all three landing paths — Validate already knows how to
// attempt/re-attempt landing), applies the distinguishing label, removes the
// awaiting-verification marker and any credited-PR label, and posts a comment
// naming the branch, the resolved base, and the credited PR that was checked
// and found not merged.
//
// Every step is best-effort and independently logged — a failure in one
// (e.g. ReopenIssue erroring) must not prevent the others from running, since
// each is itself the most important signal for an operator investigating a
// specific failure mode.
func (e *Engine) failLandingVerification(board *gh.ProjectBoard, item gh.ProjectItem, owner, repo string, creditedPR int) {
	e.logf(item.Number, "landing-verification", "credited PR #%d not merged — reopening and moving off Done\n", creditedPR)

	if item.IsClosed {
		if err := e.client.ReopenIssue(owner, repo, item.Number); err != nil {
			e.logf(item.Number, "warn", "landing-verification: could not reopen issue: %v\n", err)
		} else {
			e.logf(item.Number, "landing-verification", "reopened issue #%d\n", item.Number)
			// No cache write-through here — boardcache has no ApplyIssueReopened
			// counterpart to ApplyIssueClosed. A stale cached IsClosed=true
			// self-corrects on the next Reconcile or issues.reopened webhook; this
			// item is about to be visited by a human anyway (the failure comment
			// below), so the brief staleness window is low-stakes.
		}
	}

	if err := e.moveItemToValidate(board, item); err != nil {
		e.logf(item.Number, "warn", "landing-verification: could not move #%d off Done: %v\n", item.Number, err)
	}

	e.addLabel(item, landingVerificationFailedLabel)
	e.applyLabelRemove(item, awaitingLandingVerificationLabel, true)
	e.removeCreditedPRLabels(item)

	branch, base := e.resolveLandingVerificationBranchInfo(item)
	branchClause := "its branch"
	if branch != "" {
		branchClause = fmt.Sprintf("branch `%s`", branch)
	}
	baseClause := "its base branch"
	if base != "" {
		baseClause = fmt.Sprintf("base branch `%s`", base)
	}
	comment := fmt.Sprintf(
		"🏭 **Fabrik — landing verification failed**\n\nThis issue reached Done crediting PR #%d for the merge, but PR #%d was checked and found **not merged**. %s does not appear to be landed on %s.\n\nThe issue has been reopened, moved back to Validate, and labeled `%s` so it isn't silently treated as shipped. This is a backstop, not a diagnosis — investigate what happened to PR #%d (closed without merging? never actually created the trial branch it was supposed to?) before re-attempting the merge.",
		creditedPR, creditedPR, branchClause, baseClause, landingVerificationFailedLabel, creditedPR,
	)
	e.postItemComment(item, comment, true)

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: landingVerificationRetryStage})
}

// resolveLandingVerificationBranchInfo best-effort resolves the item's own
// branch name and its resolved base branch (honouring base:<branch> — R4),
// purely for naming them in the failure comment. Never blocks or influences
// the merge/no-merge decision itself, which is base-independent under
// mechanism 1 (see the Implementation Approach in the Plan stage comment).
// Degrades gracefully — mirroring closeIssueIfNonDefaultBase's existing
// precedent — when no WorktreeManager is registered for the item's repo.
func (e *Engine) resolveLandingVerificationBranchInfo(item gh.ProjectItem) (branch, base string) {
	branch = fmt.Sprintf("fabrik/issue-%d", item.Number)

	key := item.Repo
	if key == "" {
		key = e.defaultRepo()
	}
	e.mu.Lock()
	wm, ok := e.worktreeManagers[key]
	e.mu.Unlock()
	if !ok {
		e.logf(item.Number, "warn", "landing-verification: no WorktreeManager registered for %s — comment will not name the base branch\n", key)
		return branch, ""
	}

	resolvedBase, err := e.baseBranchForItem(item, wm)
	if err != nil {
		e.logf(item.Number, "warn", "landing-verification: could not resolve base branch for #%d: %v\n", item.Number, err)
		return branch, ""
	}
	return branch, resolvedBase
}

// moveItemToValidate moves item's board Status back to the fixed "Validate"
// target — the "move it off Done" action R2 requires. Unlike
// advanceToNextStage, which only ever moves *forward* to the next configured
// stage, this moves *backward* to a caller-chosen target; there is no
// existing generic helper for that, so this mirrors advanceToNextStage's own
// option-lookup/mutation/write-through internals but targets a fixed name.
//
// "Validate" is a literal, hardcoded stage name — the same convention used
// throughout this package (e.g. pr_terminal_advance.go, poll_settle.go) —
// not resolved via stages.NextStage or any Order-relative lookup, since there
// is no "current stage" to be relative to here (the item is in Done).
func (e *Engine) moveItemToValidate(board *gh.ProjectBoard, item gh.ProjectItem) error {
	if e.statusField == nil {
		return fmt.Errorf("status field metadata not available")
	}

	const targetStatus = "Validate"
	optionID, ok := e.statusField.Options[targetStatus]
	if !ok {
		return fmt.Errorf("no status option %q found on project board (available: %v)",
			targetStatus, mapKeys(e.statusField.Options))
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	err := e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID)
	if err == nil {
		if c := e.cache(); c != nil {
			c.UpdateItemStatus(boardcache.ItemKey(owner+"/"+repo, item.Number), targetStatus)
		}
		e.store.Apply(itemstate.SelfWriteObserved{Repo: owner + "/" + repo, Number: item.Number})
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "edited", item.ItemID)
		}
	}
	return err
}
