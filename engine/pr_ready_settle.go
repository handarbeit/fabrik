package engine

import (
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
)

// prReadyAwaitingLabel marks an item whose markPRReady call (engine/pr.go)
// failed after its own 3-attempt in-process retry was exhausted, or hit a
// non-transient error — a genuinely-created draft PR that never transitioned
// to ready-for-review. It durably records the outstanding call (a GitHub
// label survives an engine restart, unlike an itemstate.Store-only marker —
// there is no artifact to safely "redo" here except the MarkPRReady call
// itself) so settlePRReadyScan can retry it on a later poll, independent of
// the item's board column or stage. Modeled on the fabrik:awaiting-close
// settle pattern (ADR-1097).
//
// Unlike most of its awaiting-* siblings, this marker is NOT terminal-only:
// it can be applied to an item that is still mid-pipeline (e.g. a
// mark_pr_ready_on_complete stage other than the last one), and the item
// keeps advancing through subsequent stages normally while the marker is
// outstanding — nothing about a later stage's dispatch depends on the PR
// being ready. It is therefore not added to transientLifecycleLabels and
// does not suppress dispatch admission. See ADR-1582.
const prReadyAwaitingLabel = "fabrik:awaiting-pr-ready"

// prReadyRetryStage is a dedicated, non-real stage name used to key the
// existing StageRetryIncremented/StageRetryCleared/Attempts counter for
// retries of a stalled MarkPRReady call — mirrors nonDefaultBaseCloseRetryStage/
// landingVerificationRetryStage. The double-underscore wrapping makes it
// unrepresentable as a real YAML stage `name:`, so it can never collide with a
// configured stage's own retry count.
const prReadyRetryStage = "__awaiting_pr_ready__"

// markPRReadyOutstanding records that markPRReady's client.MarkPRReady call
// failed (either non-transient, or after its in-process retry budget was
// exhausted), so settlePRReadyScan retries it on a later poll. Idempotent —
// a no-op if the marker is already present.
func (e *Engine) markPRReadyOutstanding(item gh.ProjectItem, owner, repo string) {
	if hasLabel(item.Labels, prReadyAwaitingLabel) {
		return
	}
	e.addLabel(item, prReadyAwaitingLabel)
}

// settlePRReadyScan is the per-poll settle scan for the durable Mark-PR-Ready
// retry (ADR-1582). It runs unconditionally every poll over the raw board
// snapshot (not deepFetchCandidates), independent of itemMayNeedWork/
// itemNeedsWork dispatch — see prReadyAwaitingLabel's doc comment for why
// this marker cannot assume the item has reached a terminal board state the
// way most of its awaiting-* siblings can.
func (e *Engine) settlePRReadyScan(board *gh.ProjectBoard) {
	for _, item := range board.Items {
		if !hasLabel(item.Labels, prReadyAwaitingLabel) || hasLabel(item.Labels, "fabrik:paused") {
			continue
		}
		e.settlePRReady(item)
	}
}

// settlePRReady retries the outstanding MarkPRReady call for a single item.
// It re-resolves the PR live via FetchLinkedPR every pass — a GitHub label
// carries no payload, and this scan runs on a completely independent poll
// cadence from when the marker was written — rather than trusting a stashed
// PR number. Idempotent short-circuits, mirroring settleNonDefaultBaseClose's
// item.IsClosed check: no PR found, or the PR is closed/merged, or the PR is
// already not-draft (self-healed by a later stage's own markPRReady call, or
// a human clicking "Ready for review" manually) all clear the marker without
// calling MarkPRReady again. Only a genuinely-still-draft, open PR triggers a
// real retry call — safe regardless, since GitHub's markPullRequestReadyForReview
// mutation is a documented no-op success on an already-ready PR.
func (e *Engine) settlePRReady(item gh.ProjectItem) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "pr-ready", "retry: could not fetch linked PR: %v\n", err)
		e.recordPRReadyRetry(item)
		return
	}
	if pr == nil || pr.Number == 0 {
		e.logf(item.Number, "pr-ready", "no linked PR found — clearing awaiting-pr-ready marker\n")
		e.clearPRReadyMarker(item, owner, repo)
		return
	}
	if pr.Merged || pr.State == "closed" {
		e.logf(item.Number, "pr-ready", "PR #%d closed/merged — clearing awaiting-pr-ready marker\n", pr.Number)
		e.clearPRReadyMarker(item, owner, repo)
		return
	}
	if !pr.Draft {
		e.logf(item.Number, "pr-ready", "PR #%d already not draft — clearing awaiting-pr-ready marker\n", pr.Number)
		e.clearPRReadyMarker(item, owner, repo)
		return
	}

	if err := e.client.MarkPRReady(owner, repo, pr.Number); err != nil {
		e.logf(item.Number, "pr-ready", "retry: could not mark PR #%d ready: %v\n", pr.Number, err)
		e.recordPRReadyRetry(item)
		return
	}

	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEcho("pull_request", "ready_for_review", fmt.Sprintf("%s/%s#pr%d", owner, repo, pr.Number))
	}
	e.logf(item.Number, "pr-ready", "marked PR #%d ready-for-review (retry)\n", pr.Number)
	e.clearPRReadyMarker(item, owner, repo)
}

// recordPRReadyRetry increments the in-memory retry counter for a stalled
// Mark-PR-Ready retry, keyed by the dedicated prReadyRetryStage constant.
// Escalates via escalatePRReadyFailure once e.cfg.MaxRetries is reached.
// Mirrors recordNonDefaultBaseCloseRetry, including its MaxRetries<=0
// (unlimited retries, never escalate) guard.
func (e *Engine) recordPRReadyRetry(item gh.ProjectItem) {
	e.recordSettleRetry(item, prReadyRetryStage, e.escalatePRReadyFailure)
}

// escalatePRReadyFailure is called when the outstanding MarkPRReady call has
// failed MaxRetries times. It pauses the issue (fabrik:paused), removes the
// awaiting-pr-ready marker (retry suppression is no longer needed once
// fabrik:paused takes over — this scan's own paused-item guard leaves paused
// items alone), and posts an explanatory comment naming the draft PR (when
// known) with the manual recovery step — mirroring
// escalateNonDefaultBaseCloseFailure.
func (e *Engine) escalatePRReadyFailure(item gh.ProjectItem) {
	e.logf(item.Number, "escalate", "mark-pr-ready failed %d time(s) — pausing issue\n", e.cfg.MaxRetries)

	e.escalateSettle(item, prReadyAwaitingLabel, prReadyRetryStage, func(item gh.ProjectItem) {
		prClause := "its draft PR"
		manualFix := "Find the draft PR and mark it ready manually:\n```\ngh pr ready <PR-number>\n```"
		if item.LinkedPRNumber != 0 {
			prClause = fmt.Sprintf("PR #%d", item.LinkedPRNumber)
			manualFix = fmt.Sprintf("Mark it ready manually:\n```\ngh pr ready %d\n```", item.LinkedPRNumber)
		}
		comment := fmt.Sprintf(
			"🏭 **Fabrik — mark-pr-ready failed**\n\n%s could not be transitioned from draft to ready-for-review after %d attempt(s). The issue has been paused.\n\n%s\nThen remove the `fabrik:paused` label.",
			prClause, e.cfg.MaxRetries, manualFix,
		)
		e.postItemComment(item, comment, true)
	})
}

// clearPRReadyMarker removes the awaiting-pr-ready marker and clears the
// retry counter once MarkPRReady is confirmed to have succeeded (or is no
// longer applicable — no PR found, or the PR is closed/merged).
func (e *Engine) clearPRReadyMarker(item gh.ProjectItem, owner, repo string) {
	e.clearSettleMarker(item, owner, repo, prReadyAwaitingLabel, prReadyRetryStage)
}
