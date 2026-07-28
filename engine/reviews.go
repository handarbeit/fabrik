package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// botRepromptedLabel is the idempotency guard for Phase 1 of the bot-reviewer
// escalation ladder and the timing anchor for Phase 2. Fixed label, not per-login,
// because the guard is bot-agnostic: Phase 1 fires once per gate cycle for all
// outstanding bots in one round. Must stay ≤50 chars (GitHub REST API limit).
const botRepromptedLabel = "fabrik:bot-reprompted"

// checkReviewGate inspects item.LinkedPRReviewRequests and item.LinkedPRReviews
// to determine whether the review gate is blocking review reinvoke or stage
// advancement.
//
// This function is only called from the catch-up loop (Phase 1) in poll.go,
// where item.LinkedPRReviewRequests and item.LinkedPRReviews contain fresh
// data from FetchItemDetails. handleStageComplete (Path 1) always applies
// fabrik:awaiting-review directly because reviewer assignment happens after
// MarkPRReady, so data would be stale.
//
// The gate clears under either of these conditions:
//
//  1. No outstanding requested reviewers AND at least one review has been
//     submitted. This handles both the "requested reviewers finished" case
//     and the "bot reviewer self-submitted" case. Bots like Copilot and
//     Gemini do not use the requested-reviewer mechanism — they self-trigger
//     via webhooks when the PR is marked ready and only appear in
//     LinkedPRReviews (not LinkedPRReviewRequests). Waiting on the reviews
//     array is the signal that catches them.
//
// The gate stays closed (returning blocked=true) when:
//
//   - There are outstanding requested reviewers who haven't submitted, OR
//   - No reviews have been submitted yet (even with no requested reviewers —
//     bot reviewers are typically 30s–10m behind PR-ready).
//
// Bot-aware escalation ladder (when all outstanding reviewers are bots and
// item.LinkedPRNumber > 0):
//
//   - Phase 1 (fires at 1× ReviewWaitTimeout from fabrik:awaiting-review): sends
//     a formal re-request (DELETE+POST) and an @mention comment on the PR for
//     each unresponsive bot; applies fabrik:bot-reprompted label; returns
//     (true, false) — still blocked.
//   - Phase 2 (fires at 1× ReviewWaitTimeout from fabrik:bot-reprompted):
//     removes fabrik:bot-reprompted and fabrik:awaiting-review, then returns
//     (false, true) so the caller fires pauseForReviewTimeout with a contextual
//     "re-prompt was already attempted" message.
//
// Mixed or pure-human reviewer paths are unchanged: pause at 1× ReviewWaitTimeout.
//
// Returns (blocked, timedOut):
//   - (true, false)  — gate is blocking; advance should not proceed
//   - (false, false) — gate cleared naturally; advance may proceed
//   - (false, true)  — gate cleared due to timeout; caller should pause the issue
//
// Side effects when blocking:
//   - Logs a message listing why we're waiting.
//   - Adds fabrik:awaiting-review label on first block transition (idempotent).
//
// Side effects when unblocking (naturally or by timeout):
//   - Removes fabrik:awaiting-review label if present (idempotent).
//   - Removes fabrik:bot-reprompted label if present (idempotent).
func (e *Engine) checkReviewGate(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) (blocked, timedOut bool) {
	// Gate is opt-in — only active when wait_for_reviews: true.
	if stage.WaitForReviews == nil || !*stage.WaitForReviews {
		return false, false
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	paused, prNumber := e.handleBrokenReviewLinkage(owner, repo, item)
	if paused {
		// Return (false, false): the gate did not time out — it paused for a different reason.
		// The catch-up loop will not reapply fabrik:awaiting-review.
		return false, false
	}

	reviewRequests, reviews := item.LinkedPRReviewRequests, item.LinkedPRReviews

	// On a base:<branch> repo, closedByPullRequestsReferences (and everything nested
	// inside it — reviewRequests, latestReviews) is structurally empty, so
	// item.LinkedPRReviewRequests/LinkedPRReviews are always empty regardless of the
	// PR's actual review state. Fetch reviews/requests directly via REST, keyed on the
	// PR number handleBrokenReviewLinkage already resolved. See #1046/#1047/#1050.
	if itemHasBaseLabel(item) && prNumber > 0 {
		restReviews, reviewsErr := e.readClient.FetchPRReviews(owner, repo, prNumber)
		restRequests, requestsErr := e.readClient.FetchPRReviewRequests(owner, repo, prNumber)
		if reviewsErr != nil || requestsErr != nil {
			// Conservative: treat a partial failure as no-data rather than trusting
			// whichever call succeeded — a false len(outstanding)==0 read could
			// falsely clear the gate while real outstanding reviewers are unknown.
			if reviewsErr != nil {
				e.logf(item.Number, "warn", "checkReviewGate: FetchPRReviews failed: %v\n", reviewsErr)
			}
			if requestsErr != nil {
				e.logf(item.Number, "warn", "checkReviewGate: FetchPRReviewRequests failed: %v\n", requestsErr)
			}
			reviewRequests, reviews = nil, nil
		} else {
			reviewRequests, reviews = restRequests, restReviews
		}
	}

	outstanding, hasReviews := reviewGateOutstanding(reviewRequests, reviews)

	// Gate clears when all outstanding requested reviewers have responded
	// AND at least one non-DISMISSED review exists. This catches both human
	// reviewers who submit formally and bot reviewers (Copilot, Gemini) who
	// self-submit without ever appearing in reviewRequests.
	if len(outstanding) == 0 && hasReviews {
		e.removeAwaitingReviewLabel(owner, repo, item)
		return false, false
	}

	// Determine if all outstanding reviewers are bots. Used by Phase 1/2 logic.
	allBots := reviewGateAllBots(reviewRequests, outstanding)

	// Find the fabrik:bot-reprompted label (idempotency guard for Phase 1 and
	// timing anchor for Phase 2).
	var reprompted bool
	for _, l := range item.Labels {
		if l == botRepromptedLabel {
			reprompted = true
			break
		}
	}

	// Phase 2 check: if a re-prompt was already sent and another full timeout
	// window has elapsed without response, pause for human.
	if reprompted && allBots {
		if blocked, timedOut, done := e.checkBotPhase2Timeout(owner, repo, item); done {
			return blocked, timedOut
		}
	}

	// Still waiting. Check the fabrik:awaiting-review timeout (Phase 1
	// re-prompt, an already-fired Phase 1 waiting on Phase 2, or a
	// mixed/pure-human/no-PR-number pause).
	if blocked, timedOut, done := e.checkAwaitingReviewTimeout(owner, repo, item, outstanding, allBots, reprompted); done {
		return blocked, timedOut
	}

	if len(outstanding) > 0 {
		e.logf(item.Number, "awaiting-review", "waiting for reviewers: %s\n", strings.Join(outstanding, ", "))
	} else {
		e.logf(item.Number, "awaiting-review", "waiting for initial review submission (no reviewers requested; bot reviewers may still be processing)\n")
	}

	// Apply label on first block transition.
	alreadyWaiting := false
	for _, l := range item.Labels {
		if l == "fabrik:awaiting-review" {
			alreadyWaiting = true
			break
		}
	}
	if !alreadyWaiting {
		e.applyLabelAdd(item, "fabrik:awaiting-review", false)
	}

	return true, false
}

// handleBrokenReviewLinkage detects FR-013's broken-linkage case: the item
// has no linked PR via closingIssuesReferences (LinkedPRNumber == 0) but a PR
// exists on the fabrik/issue-N branch. Without this guard the gate would
// silently loop forever applying fabrik:awaiting-review. When detected, it
// pauses with a clear message (without applying fabrik:awaiting-review) and
// reports paused=true so the caller returns (false, false) directly.
//
// On a base:<branch> repo, closedByPullRequestsReferences is structurally empty
// (GitHub only populates it for PRs targeting the repository default branch), so
// item.LinkedPRNumber == 0 does not by itself indicate broken linkage there. In that
// case linkage is confirmed via FetchPRClosingIssues (a direct PR-body parse) before
// concluding the linkage is actually broken; the default-branch message/behavior below
// this check is unchanged.
//
// Also returns the resolved PR number (0 when no PR was found or the item was
// paused). Since LinkedPRNumber is always 0 on a base:<branch> repo (the same
// structurally-empty GraphQL field), this function's FetchLinkedPR lookup is not a
// one-time linkage-repair path there — it is the steady-state PR-number resolution
// every checkReviewGate call needs, so the number is threaded back to the caller
// rather than re-fetched.
func (e *Engine) handleBrokenReviewLinkage(owner, repo string, item gh.ProjectItem) (paused bool, prNumber int) {
	if item.LinkedPRNumber != 0 {
		return false, item.LinkedPRNumber
	}

	pr, prErr := e.readClient.FetchLinkedPR(owner, repo, item.Number)
	if prErr != nil || pr == nil || pr.Number == 0 || pr.State != "open" || pr.Merged {
		return false, 0
	}

	if itemHasBaseLabel(item) {
		closingIssues, err := e.readClient.FetchPRClosingIssues(owner, repo, pr.Number)
		if err != nil {
			// Transient fetch error: skip verification rather than false-positive pausing.
			e.logf(item.Number, "warn", "handleBrokenReviewLinkage: FetchPRClosingIssues failed: %v\n", err)
			return false, pr.Number
		}
		if slices.Contains(closingIssues, item.Number) {
			// Linkage confirmed via PR body — not broken; let the gate proceed normally.
			return false, pr.Number
		}
		e.logf(item.Number, "review-gate", "broken linkage: PR #%d (base:<branch> repo) exists on branch fabrik/issue-%d but its body lacks a closing keyword\n", pr.Number, item.Number)
		msg := fmt.Sprintf(
			"🏭 **Fabrik — broken PR↔issue linkage**\n\n"+
				"PR #%d exists on branch `fabrik/issue-%d` but its body does not contain a recognized closing keyword. "+
				"This issue targets a non-default base branch, so GitHub's `closingIssuesReferences` is always empty here and "+
				"cannot be used to confirm linkage — the review gate checks the PR body directly instead, but found nothing.\n\n"+
				"Add `Closes #%d` as the first line of the PR body and remove `fabrik:paused` to resume:\n\n"+
				"```bash\n"+
				"gh pr view %d --json body --jq '.body' > /tmp/pr_body.txt && "+
				"printf 'Closes #%d\\n\\n' | cat - /tmp/pr_body.txt | "+
				"gh pr edit %d --body-file -\n"+
				"```",
			pr.Number, item.Number, item.Number,
			pr.Number, item.Number, pr.Number,
		)
		e.pauseIssue(item, msg, pauseOpts{
			labelEcho:  true,
			labelFirst: true,
		})
		return true, 0
	}

	e.logf(item.Number, "review-gate", "broken linkage: PR #%d exists on branch fabrik/issue-%d but is not linked via closing keyword\n", pr.Number, item.Number)
	msg := fmt.Sprintf(
		"🏭 **Fabrik — broken PR↔issue linkage**\n\n"+
			"PR #%d exists on branch `fabrik/issue-%d` but is not linked to this issue via a closing keyword "+
			"(`closingIssuesReferences` is empty). The review gate cannot proceed without this linkage.\n\n"+
			"Add `Closes #%d` as the first line of the PR body and remove `fabrik:paused` to resume:\n\n"+
			"```bash\n"+
			"gh pr view %d --json body --jq '.body' > /tmp/pr_body.txt && "+
			"printf 'Closes #%d\\n\\n' | cat - /tmp/pr_body.txt | "+
			"gh pr edit %d --body-file -\n"+
			"```",
		pr.Number, item.Number, item.Number,
		pr.Number, item.Number, pr.Number,
	)
	e.pauseIssue(item, msg, pauseOpts{
		labelEcho:  true,
		labelFirst: true,
	})
	return true, 0
}

// reviewGateOutstanding computes the outstanding requested reviewers (humans
// or bots using the formal request mechanism — a dismissed review puts the
// reviewer back here; if they're not here, they've finished) and whether at
// least one non-DISMISSED review has been submitted. Takes the review-request
// and review slices directly (rather than a gh.ProjectItem) so the same logic
// serves both the GraphQL-sourced default-branch path (item.LinkedPRReviewRequests/
// LinkedPRReviews) and the REST-sourced base:<branch> path (checkReviewGate) —
// the caller decides where the data comes from.
func reviewGateOutstanding(reviewRequests []gh.ReviewRequest, reviews []gh.PRReview) (outstanding []string, hasReviews bool) {
	for _, rr := range reviewRequests {
		if rr.Login != "" {
			outstanding = append(outstanding, rr.Login)
		}
	}
	for _, r := range reviews {
		if r.State != "DISMISSED" {
			hasReviews = true
			break
		}
	}
	return outstanding, hasReviews
}

// reviewGateBlocksLanding is the landing-decision review gate (#1216). It reports
// whether a wait_for_reviews stage must be held back from its landing decision
// (auto-merge enable, enqueue, direct merge, or advance-to-Queued) because reviewer
// requests are still outstanding.
//
// Why this exists separately from checkReviewGate: the catch-up loop's
// handleReviewGate deliberately no-ops while !pctx.hasComplete (#617), and
// pctx.hasComplete is frozen before the Phase 1 handler chain runs. Because
// reviewGate is ordered ahead of mergeAndCIGates, there is no poll pass in which
// the gate can arm after CI clears and before Phase 2 calls attemptMergeOnValidate
// in that same iteration. Enforcing here — at the single landing-decision choke
// point for both merge_train modes — closes that hole, and also covers the
// wait_for_ci-independent case where handleStageComplete merges before its own
// fabrik:awaiting-review seeding ever runs.
//
// Review state is always re-fetched live rather than read from
// item.LinkedPRReviewRequests/LinkedPRReviews: the two callers of
// attemptMergeOnValidate have different freshness guarantees (handleStageComplete's
// item is the pre-stage snapshot, stale by design because reviewer assignment
// happens inside MarkPRReady), and FR-2 requires the gate to see a reviewer
// requested during the CI-await window.
//
// This check only blocks and labels. It deliberately does not duplicate the
// bot-escalation ladder or the wait timeout — once it blocks, stage:X:complete is
// present on the next poll, so handleReviewGate claims the item with
// hasComplete == true and checkReviewGate owns all escalation from there. That
// handoff is a hard dependency, not a convenience: nothing here bounds how long a
// landing stays held, so every blocking exit relies on checkReviewGate's timers
// firing off the state this function leaves behind, without the item ever
// re-entering this function. See the comment on the final blocking exit below for
// why that holds even in the "no reviewers requested, nothing reviewed yet" case,
// which has no escalation ladder of its own.
//
// Returns false (landing may proceed) when the gate is not opted in, when the item
// demonstrably has no PR (no PR means no reviewer requests; handleBrokenReviewLinkage
// owns the broken-linkage pause), or when the gate has cleared. Returns true (and
// idempotently applies fabrik:awaiting-review) otherwise, including on any fetch
// error — blocking conservatively on unknown state, mirroring checkReviewGate's
// base:<branch> fallback. "No PR" and "could not read the PR" are deliberately
// distinguished: only the former is a safe reason to let a landing through.
func (e *Engine) reviewGateBlocksLanding(item gh.ProjectItem, stage *stages.Stage, owner, repo string) bool {
	// Gate is opt-in — only active when wait_for_reviews: true. Checked before any
	// PR resolution so stages that don't use the gate pay zero extra API calls.
	if stage == nil || stage.WaitForReviews == nil || !*stage.WaitForReviews {
		return false
	}

	prNumber := item.LinkedPRNumber
	if prNumber == 0 {
		// On a base:<branch> repo closedByPullRequestsReferences is structurally
		// empty, so LinkedPRNumber is always 0 there — resolve via REST instead.
		pr, err := e.readClient.FetchLinkedPR(owner, repo, item.Number)
		if err != nil {
			// Conservative, for the same reason as the review-fetch failure below:
			// an unreadable PR is unknown state, not "no PR". On a base:<branch>
			// repo this fallback is the ONLY PR-resolution route, so treating a
			// transient error as "nothing to gate on" would land the item with the
			// gate never evaluated at all. checkReviewGate blocks here too (via
			// handleBrokenReviewLinkage returning prNumber 0, leaving the item's
			// structurally-empty review fields to fail the clearing condition).
			e.logf(item.Number, "warn", "reviewGateBlocksLanding: FetchLinkedPR failed: %v\n", err)
			return e.holdLandingForReview(item, "holding landing decision — linked PR unreadable, review state unknown\n")
		}
		if pr == nil || pr.Number == 0 {
			return false
		}
		// FetchLinkedPR queries state=all, so a stale PR from a previous cycle on
		// the same fabrik/issue-N branch (or one that already merged via a race
		// with a retried landing) can come back here. Gating on such a PR would
		// read the wrong PR's review state — blocking on a dead reviewer request,
		// or worse, clearing because a long-closed PR happens to carry an approval.
		// Neither is a decision about the PR being landed, so treat it exactly as
		// "no open PR to gate on", the same filter and the same resulting prNumber
		// 0 that handleBrokenReviewLinkage applies to its own FetchLinkedPR result.
		// This matters most on a base:<branch> repo, where LinkedPRNumber is always
		// 0 and this fallback is the steady-state resolution route, not an edge case.
		if pr.State != "open" || pr.Merged {
			return false
		}
		// FetchLinkedPR resolves by head-branch name (fabrik/issue-N) alone — it
		// does not verify that the PR body actually closes this issue, unlike
		// handleBrokenReviewLinkage's FetchPRClosingIssues check. So on a
		// base:<branch> repo this can gate against a PR whose linkage is broken.
		// That is deliberately not corrected here: attemptMergeOnValidate's own
		// landing path already resolves the PR the same way (and did so before
		// this gate existed), so the gate evaluates reviews on exactly the PR it
		// would go on to land. More importantly the gate is strictly subtractive
		// — it returns either "block" or "proceed as before", and can never cause
		// a landing that would not otherwise happen — so an unverified match here
		// cannot widen the pre-existing linkage gap, only fail to narrow it.
		// Adding closing-keyword verification belongs with that shared resolution
		// path (and with handleBrokenReviewLinkage, which owns the pause), not in
		// the review gate.
		prNumber = pr.Number
	}

	reviews, reviewsErr := e.readClient.FetchPRReviews(owner, repo, prNumber)
	requests, requestsErr := e.readClient.FetchPRReviewRequests(owner, repo, prNumber)
	fetchFailed := reviewsErr != nil || requestsErr != nil
	if fetchFailed {
		// Conservative: treat a partial failure as no-data rather than trusting
		// whichever call succeeded — a false len(outstanding)==0 read could clear
		// the gate while real outstanding reviewers are unknown.
		if reviewsErr != nil {
			e.logf(item.Number, "warn", "reviewGateBlocksLanding: FetchPRReviews failed: %v\n", reviewsErr)
		}
		if requestsErr != nil {
			e.logf(item.Number, "warn", "reviewGateBlocksLanding: FetchPRReviewRequests failed: %v\n", requestsErr)
		}
		reviews, requests = nil, nil
	}

	// Same clearing condition as checkReviewGate, via the same shared pure
	// function, so the two gate sites can never disagree on "outstanding".
	outstanding, hasReviews := reviewGateOutstanding(requests, reviews)
	if len(outstanding) == 0 && hasReviews {
		// Deliberately does NOT call removeAwaitingReviewLabel here, unlike
		// checkReviewGate's equivalent clearing branch. Removal is checkReviewGate's
		// job: this function only ever runs on a Validate item, and every path that
		// reaches a blocking exit below leaves stage:Validate:complete present, so
		// handleReviewGate claims the item with hasComplete == true on the next poll
		// and clears the label there (naturally or on timeout). Symmetric
		// apply-and-remove here would race that owner for no gain.
		//
		// This is load-bearing for any future landing path: a caller that invokes
		// attemptMergeOnValidate for an item that never re-enters catch-up Phase 1
		// would strand a fabrik:awaiting-review applied here. Such a caller must
		// either route through the catch-up loop or take over removal explicitly.
		return false
	}

	if len(outstanding) > 0 {
		return e.holdLandingForReview(item, "holding landing decision on PR #%d — waiting for reviewers: %s\n",
			prNumber, strings.Join(outstanding, ", "))
	}
	if fetchFailed {
		// Distinct from the "nobody has reviewed yet" message below: both reach
		// this point with outstanding empty and hasReviews false, but only this
		// one means the review state is unknown. Without the distinction an
		// operator watching a GitHub API outage sees "waiting for initial review
		// submission" and has no signal that the gate is blocking on a fetch
		// failure rather than on a genuinely unreviewed PR.
		return e.holdLandingForReview(item,
			"holding landing decision on PR #%d — review state unreadable (fetch failed), blocking conservatively\n", prNumber)
	}
	// Reachable steady state: zero requested reviewers and no review submitted
	// yet (e.g. just after MarkPRReady, before any human or bot has weighed in).
	// This blocks with no escalation of its own, which is safe only because
	// checkReviewGate's timers fire off exactly this state without the item ever
	// re-entering this function: every blocking exit here leaves
	// stage:Validate:complete present, so handleReviewGate claims the item next
	// poll with hasComplete == true. From there reviewGateAllBots returns false
	// (it is false whenever outstanding is empty), so both bot-ladder phases are
	// skipped and checkAwaitingReviewTimeout falls to its mixed/pure-human pause
	// branch, which fires once ReviewWaitTimeout has elapsed since
	// fabrik:awaiting-review was applied — the label holdLandingForReview sets, read
	// via FetchLabelAppliedAt. That path removes the label and pauses for a human,
	// so this is a bounded hold, not a permanent block.
	return e.holdLandingForReview(item, "holding landing decision on PR #%d — waiting for initial review submission\n", prNumber)
}

// holdLandingForReview is reviewGateBlocksLanding's single blocking exit: it logs
// why the landing is being held, applies fabrik:awaiting-review idempotently (the
// label is also the anchor checkReviewGate's timeout reads via FetchLabelAppliedAt,
// so a persistently unreadable PR eventually pauses for a human rather than
// hanging), and reports true. Every block path routes through here so no future
// one forgets the label.
func (e *Engine) holdLandingForReview(item gh.ProjectItem, format string, args ...any) bool {
	e.logf(item.Number, "awaiting-review", format, args...)
	if !hasLabel(item.Labels, "fabrik:awaiting-review") {
		e.applyLabelAdd(item, "fabrik:awaiting-review", true)
	}
	return true
}

// reviewGateAllBots reports whether every outstanding requested reviewer is a
// bot (false when there are no outstanding reviewers at all).
func reviewGateAllBots(reviewRequests []gh.ReviewRequest, outstanding []string) bool {
	allBots := len(outstanding) > 0
	for _, rr := range reviewRequests {
		if rr.Login != "" && !rr.IsBot {
			allBots = false
			break
		}
	}
	return allBots
}

// checkBotPhase2Timeout implements Phase 2 of the bot-reviewer escalation
// ladder. It is only called when a Phase 1 re-prompt was already sent and all
// outstanding reviewers are bots. If a full ReviewWaitTimeout window has
// elapsed since fabrik:bot-reprompted was applied, it removes
// fabrik:bot-reprompted and fabrik:awaiting-review and reports done=true with
// (blocked=false, timedOut=true) so the caller pauses with Phase-2 context.
// done=false means the caller should fall through to the next phase (the
// label timestamp fetch failed, or the timeout window hasn't elapsed yet).
func (e *Engine) checkBotPhase2Timeout(owner, repo string, item gh.ProjectItem) (blocked, timedOut, done bool) {
	repromptedAt, err := e.client.FetchLabelAppliedAt(owner, repo, item.Number, botRepromptedLabel)
	if err != nil {
		e.logf(item.Number, "warn", "could not fetch bot-reprompted label timestamp: %v\n", err)
		return false, false, false
	}
	if repromptedAt.IsZero() {
		return false, false, false
	}
	timeout := e.cfg.ReviewWaitTimeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	if time.Since(repromptedAt) < timeout {
		return false, false, false
	}
	e.logf(item.Number, "review-gate", "phase 2: bot(s) unresponsive after re-prompt — pausing for human\n")
	// Remove fabrik:bot-reprompted and fabrik:awaiting-review.
	// item.Labels is the pre-cleanup snapshot so pauseForReviewTimeout
	// can still detect Phase 2 context from it after we return.
	for _, l := range item.Labels {
		if l == botRepromptedLabel || l == "fabrik:awaiting-review" {
			e.applyLabelRemove(item, l, false)
		}
	}
	return false, true, true
}

// checkAwaitingReviewTimeout implements the fabrik:awaiting-review timeout
// check: once ReviewWaitTimeout has elapsed since the label was applied, it
// either fires Phase 1 of the bot-reviewer escalation ladder (re-prompting
// every outstanding bot reviewer), lets an already-fired Phase 1 continue
// waiting for Phase 2, or pauses for a mixed/pure-human/no-PR-number gate.
// done=true means the caller should return (blocked, timedOut) directly;
// done=false means the label wasn't found, was found but the timeout hasn't
// elapsed yet, or Phase 1 already fired and Phase 2 hasn't timed out yet —
// the caller falls through to the "still waiting" logging/label-apply tail.
func (e *Engine) checkAwaitingReviewTimeout(owner, repo string, item gh.ProjectItem, outstanding []string, allBots, reprompted bool) (blocked, timedOut, done bool) {
	timeout := e.cfg.ReviewWaitTimeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	for _, l := range item.Labels {
		if l != "fabrik:awaiting-review" {
			continue
		}
		appliedAt, err := e.client.FetchLabelAppliedAt(owner, repo, item.Number, "fabrik:awaiting-review")
		if err != nil {
			e.logf(item.Number, "warn", "could not fetch awaiting-review label timestamp: %v\n", err)
			return false, false, false
		}
		if appliedAt.IsZero() || time.Since(appliedAt) < timeout {
			return false, false, false
		}
		// 1× timeout elapsed.
		if allBots && item.LinkedPRNumber > 0 && !reprompted {
			// Phase 1: re-prompt all outstanding bot reviewers.
			var repromptedLogins []string
			for _, rr := range item.LinkedPRReviewRequests {
				if rr.Login == "" {
					continue
				}
				login := rr.Login
				if err := e.client.DeleteReviewRequest(owner, repo, item.LinkedPRNumber, []string{login}); err != nil {
					e.logf(item.Number, "warn", "phase 1: could not delete review request for %s: %v\n", login, err)
				}
				if err := e.client.AddReviewRequest(owner, repo, item.LinkedPRNumber, []string{login}); err != nil {
					e.logf(item.Number, "warn", "phase 1: could not re-add review request for %s: %v\n", login, err)
				}
				msg := fmt.Sprintf("🏭 **Fabrik — review re-prompt**\n\n@%s just checking in — could you take a look at this PR?", botMentionHandle(login))
				// no write-through: excluded — posts to item.LinkedPRNumber (PR comment thread, not issue cache)
				if dbID, err := e.client.AddComment(owner, repo, item.LinkedPRNumber, msg); err != nil {
					e.logf(item.Number, "warn", "phase 1: could not post re-prompt comment for %s: %v\n", login, err)
					// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
				} else if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
					e.logf(item.Number, "warn", "phase 1: could not add 🚀 to re-prompt comment: %v\n", reactErr)
				}
				repromptedLogins = append(repromptedLogins, login)
			}
			e.applyLabelAdd(item, botRepromptedLabel, false)
			e.logf(item.Number, "review-gate", "phase 1: re-prompted bot reviewer(s): %s\n", strings.Join(repromptedLogins, ", "))
			return true, false, true
		}

		// If Phase 1 already fired (reprompted label present) and Phase 2
		// hasn't timed out yet, stay blocked and let Phase 2 handle it.
		if allBots && reprompted {
			return false, false, false
		}

		// Mixed/pure-human or no PR number: existing pause behavior.
		var reason string
		if len(outstanding) > 0 {
			reason = "pending reviewers: " + strings.Join(outstanding, ", ")
		} else {
			reason = "no reviews submitted yet (bots may not have responded)"
		}
		e.logf(item.Number, "warn", "review wait timeout elapsed; pausing issue — %s\n", reason)
		e.removeAwaitingReviewLabel(owner, repo, item)
		return false, true, true
	}
	return false, false, false
}

// removeAwaitingReviewLabel removes fabrik:awaiting-review and the
// fabrik:bot-reprompted label if present on the item (gate-cycle cleanup).
func (e *Engine) removeAwaitingReviewLabel(owner, repo string, item gh.ProjectItem) {
	for _, l := range item.Labels {
		if l == "fabrik:awaiting-review" {
			e.applyLabelRemove(item, "fabrik:awaiting-review", false)
		}
		if l == botRepromptedLabel {
			e.applyLabelRemove(item, l, false)
		}
	}
}

// buildReviewThreadComments returns the inline per-line comments from
// unresolved review threads on the linked PR that have not yet been addressed
// (no 🚀 reaction and not already present in processedSet). These are real
// GitHub comments with real DatabaseIDs, so the 👀/🚀 reaction-based dedup
// mechanism works normally and each thread comment only triggers processing once.
//
// The top-level review body (if any) is not included — only thread comments,
// which are what reviewers use to flag specific code issues. Reviews that
// submit only a top-level body with no inline comments (e.g., bare APPROVED)
// have nothing actionable to address.
//
// The store's ProcessedComments map is checked as defense-in-depth for
// within-session races: if a comment was processed this session but the ROCKET
// reaction hasn't propagated from the API yet, we still skip it.
func (e *Engine) buildReviewThreadComments(item gh.ProjectItem) []gh.Comment {
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	snap, _ := e.store.Get(repoStr, item.Number)
	out := make([]gh.Comment, 0, len(item.LinkedPRReviewThreadComments))
	for _, c := range item.LinkedPRReviewThreadComments {
		if c.HasReaction("ROCKET") {
			continue
		}
		if !snap.CommentProcessed(c.ID).IsZero() {
			continue
		}
		out = append(out, c)
	}
	return out
}

// pauseForReviewTimeout pauses the issue when the review wait timeout elapses.
// It applies fabrik:paused + fabrik:awaiting-input and posts an explanatory comment.
// If item.Labels contains the fabrik:bot-reprompted label (the pre-cleanup snapshot
// captured before checkReviewGate removed it), Phase 2 context is detected and a
// more specific "after re-prompt" message is posted.
func (e *Engine) pauseForReviewTimeout(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "review-timeout", "review wait timeout elapsed — pausing for human intervention\n")

	// Build pending-reviewer list with bot/human tags for the pause comment.
	var reviewerParts []string
	for _, rr := range item.LinkedPRReviewRequests {
		if rr.Login == "" {
			continue
		}
		tag := "human"
		if rr.IsBot {
			tag = "bot"
		}
		reviewerParts = append(reviewerParts, fmt.Sprintf("`%s` (%s)", rr.Login, tag))
	}

	// Detect Phase 2 context: checkReviewGate removed the label from GitHub but
	// item.Labels is the pre-cleanup snapshot, so the label is still present here.
	// Derive bot logins from LinkedPRReviewRequests (bots haven't responded, so
	// they're still in the requests list).
	var repromptedLogins []string
	for _, l := range item.Labels {
		if l == botRepromptedLabel {
			for _, rr := range item.LinkedPRReviewRequests {
				if rr.IsBot && rr.Login != "" {
					repromptedLogins = append(repromptedLogins, rr.Login)
				}
			}
			break
		}
	}

	var msg string
	if len(repromptedLogins) > 0 {
		// Phase 2 message: re-prompt was already sent but bot didn't respond.
		botList := strings.Join(repromptedLogins, ", ")
		prRef := ""
		if item.LinkedPRNumber > 0 {
			prRef = fmt.Sprintf("PR #%d", item.LinkedPRNumber)
		} else {
			prRef = "the linked PR"
		}
		msg = fmt.Sprintf(
			"🏭 **Fabrik — review wait timeout (after bot re-prompt)**\n\n"+
				"The review gate for stage **%s** timed out waiting for %s (bot). "+
				"A re-prompt was sent, but no review was submitted in the additional waiting window.\n\n"+
				"Fabrik has paused this issue. To resume, either:\n"+
				"- (a) post a review on %s yourself,\n"+
				"- (b) remove `wait_for_reviews: true` from the %s stage YAML if bot reviews are unreliable on this repo,\n"+
				"- (c) merge %s manually, or\n"+
				"- (d) remove `fabrik:paused` to let the engine cycle through another re-prompt + wait.",
			stage.Name, botList, prRef, stage.Name, prRef,
		)
	} else {
		// Standard timeout message with named reviewers.
		pendingLine := ""
		if len(reviewerParts) > 0 {
			pendingLine = "\n\nPending reviewers: " + strings.Join(reviewerParts, ", ")
		}
		msg = fmt.Sprintf(
			"🏭 **Fabrik — review wait timeout**\n\nThe review gate for stage **%s** timed out waiting for outstanding reviewers.%s\n\n"+
				"Fabrik has paused this issue. Please check the PR for pending reviews, address any issues, and then remove the `fabrik:paused` label to resume.",
			stage.Name, pendingLine,
		)
	}

	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})
}

// dispatchReviewReinvoke re-invokes the stage agent via processComments with
// synthetic review comments. A thin wrapper over dispatchReinvoke, supplying
// only review's pre-dispatch emptiness precheck and its comment builder —
// the shared goroutine scaffold (WorkerEntered/semaphore/processComments)
// lives in reinvoke.go.
func (e *Engine) dispatchReviewReinvoke(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.dispatchReinvoke(ctx, board, item, stage, reinvokeOpts{
		tag: "review-reinvoke",
		// precheck runs synchronously before WorkerEntered/goroutine dispatch —
		// buildReviewThreadComments needs no workDir, so there's no reason to
		// incur ensureRepoReady/WorkerEntered churn for a same-poll no-op.
		precheck: func() bool {
			return len(e.buildReviewThreadComments(item)) > 0
		},
		build: func(workDir string) []gh.Comment {
			return e.buildReviewThreadComments(item)
		},
	})
}

// pauseForReviewCycleLimit pauses the issue when the maximum review re-invocation
// cycle count is reached. It applies fabrik:paused + fabrik:awaiting-input and
// posts an explanatory comment.
func (e *Engine) pauseForReviewCycleLimit(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, cycleCount, maxCycles int) {
	e.logf(item.Number, "review-cycles", "review cycle limit %d reached — pausing for human intervention\n", maxCycles)

	msg := fmt.Sprintf(
		"🏭 **Fabrik — review cycle limit reached**\n\nThe stage **%s** has been re-invoked to address PR review feedback %d time(s), "+
			"which has reached the maximum configured limit (`FABRIK_MAX_REVIEW_CYCLES=%d`).\n\n"+
			"This usually means a reviewer (bot or human) is repeatedly requesting changes after each fix. "+
			"Fabrik has paused this issue for human review. Once the review situation is resolved, "+
			"remove the `fabrik:paused` label to resume.",
		stage.Name, cycleCount, maxCycles,
	)
	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})
}

// botMentionHandle maps copilot-* logins to "copilot" — GitHub's canonical mention surface for the reviewer bot.
func botMentionHandle(login string) string {
	if strings.HasPrefix(strings.ToLower(login), "copilot") {
		return "copilot"
	}
	return login
}
