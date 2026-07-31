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
// Returns (blocked, timedOut, terminated):
//   - (true, false, false)  — gate is blocking; advance should not proceed
//   - (false, false, false) — gate cleared naturally; advance may proceed
//   - (false, true, false)  — gate cleared due to timeout; caller should pause the issue
//   - (false, false, true)  — processing already terminated via a direct pauseIssue call
//     (handleBrokenReviewLinkage). The caller MUST claim the item (do not treat this as
//     "gate cleared") so Phase 2 does not advance an item that was just paused in this
//     same pass. See ADR-1223.
//
// Side effects when blocking:
//   - Logs a message listing why we're waiting.
//   - Adds fabrik:awaiting-review label on first block transition (idempotent).
//
// Side effects when unblocking (naturally or by timeout):
//   - Removes fabrik:awaiting-review label if present (idempotent).
//   - Removes fabrik:bot-reprompted label if present (idempotent).
func (e *Engine) checkReviewGate(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) (blocked, timedOut, terminated bool) {
	// Gate is opt-in — only active when wait_for_reviews: true.
	if stage.WaitForReviews == nil || !*stage.WaitForReviews {
		return false, false, false
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	paused, prNumber := e.handleBrokenReviewLinkage(owner, repo, item)
	if paused {
		// handleBrokenReviewLinkage already paused the item directly — report
		// terminated=true so the caller claims it instead of reading this as
		// "gate cleared naturally".
		return false, false, true
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
	//
	// In authoritative mode this is additive, not a replacement: the same
	// len(outstanding)==0 && hasReviews condition still gates entry, and
	// reviewGateAuthorityVerdict is only consulted from inside it — a PR with
	// zero reviews stays blocked by the outer condition regardless of mode.
	var authorityReason string
	if len(outstanding) == 0 && hasReviews {
		if e.effectiveReviewAuthority(item, stage) != "authoritative" {
			e.removeAwaitingReviewLabel(owner, repo, item)
			return false, false, false
		}
		if prNumber <= 0 {
			// No PR number resolved — can't fetch a verdict. Block
			// conservatively rather than silently falling back to advisory
			// clearing.
			authorityReason = "review verdict unreadable — no PR number resolved"
		} else if reviewDecision, err := e.readClient.FetchPRReviewDecision(owner, repo, prNumber); err != nil {
			e.logf(item.Number, "warn", "checkReviewGate: FetchPRReviewDecision failed: %v\n", err)
			authorityReason = "review verdict unreadable (fetch failed), blocking conservatively"
		} else if satisfied, reason := reviewGateAuthorityVerdict(reviewDecision, reviews); satisfied {
			e.removeAwaitingReviewLabel(owner, repo, item)
			return false, false, false
		} else {
			authorityReason = reason
		}
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
			return blocked, timedOut, false
		}
	}

	// Still waiting. Check the fabrik:awaiting-review timeout (Phase 1
	// re-prompt, an already-fired Phase 1 waiting on Phase 2, or a
	// mixed/pure-human/no-PR-number pause).
	if blocked, timedOut, done := e.checkAwaitingReviewTimeout(owner, repo, item, outstanding, allBots, reprompted, authorityReason); done {
		return blocked, timedOut, false
	}

	if authorityReason != "" {
		e.logf(item.Number, "awaiting-review", "authoritative gate still blocking: %s\n", authorityReason)
	} else if len(outstanding) > 0 {
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

	return true, false, false
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

// reviewAuthorityRank lists review-authority modes from least to most
// restrictive. Iterating from the end returns the more restrictive mode
// (authoritative > advisory) — mirrors effortLevelRank's "prefer the
// higher-ranked value" convention, not extractModelOverride's first-wins
// convention. See #1261.
var reviewAuthorityRank = []string{"advisory", "authoritative"}

// extractReviewAuthorityOverride scans item labels for "review-authority:<mode>"
// labels and returns the resolved mode. If multiple recognized labels are
// present, it resolves to the more restrictive one (authoritative wins) and
// logs a warning listing all found labels — mirroring extractEffortOverride's
// "pick deterministically, don't arbitrate" convention rather than
// extractModelOverride's "first wins" convention, per #1261.
//
// A label whose suffix is not exactly "advisory" or "authoritative" (typo,
// casing, unknown value) is ignored with a logged warning — never a hard
// failure, never a silent escalation to authoritative.
//
// Returns "" when no valid review-authority: label is found (stage config governs).
func (e *Engine) extractReviewAuthorityOverride(issueNumber int, labels []string) string {
	const prefix = "review-authority:"
	found := make(map[string]bool)
	var malformed []string
	for _, label := range labels {
		if !strings.HasPrefix(label, prefix) {
			continue
		}
		mode := strings.TrimPrefix(label, prefix)
		if mode == "advisory" || mode == "authoritative" {
			found[mode] = true
		} else {
			malformed = append(malformed, label)
		}
	}
	if len(malformed) > 0 {
		e.logf(issueNumber, "warn", "unrecognized review-authority: label(s) %s (must be advisory or authoritative); ignoring\n",
			strings.Join(malformed, ", "))
	}
	if len(found) == 0 {
		return ""
	}
	if len(found) > 1 {
		all := make([]string, 0, len(found))
		for m := range found {
			all = append(all, "review-authority:"+m)
		}
		e.logf(issueNumber, "warn", "multiple review-authority: labels found (%s); using more restrictive (authoritative)\n", strings.Join(all, ", "))
	}
	// Return the more restrictive mode present.
	for i := len(reviewAuthorityRank) - 1; i >= 0; i-- {
		if found[reviewAuthorityRank[i]] {
			return reviewAuthorityRank[i]
		}
	}
	return ""
}

// effectiveReviewAuthority resolves the effective review_authority mode for
// item, applying the per-issue "review-authority:<mode>" label override (#1261)
// on top of the stage's YAML-configured value. checkReviewGate,
// reviewGateBlocksLanding, and pauseForReviewTimeout's message-only check all
// consult this instead of reading stage.ReviewAuthority directly, so the two
// gates (and the pause message describing them) can never disagree about
// which mode is in effect for a given issue.
//
// Precedence: no review-authority: label on the issue → stage.ReviewAuthority
// governs. Exactly one recognized label → it overrides the stage config for
// this issue only. Both labels present → resolves to "authoritative" (logged
// warning). Malformed/unknown label → ignored (logged warning), falls back to
// stage config.
//
// Returns "", "advisory", or "authoritative" — every existing call site
// already compares with !=/== "authoritative" only, so "" and "advisory"
// remain interchangeable exactly as stage.ReviewAuthority == "" is today.
func (e *Engine) effectiveReviewAuthority(item gh.ProjectItem, stage *stages.Stage) string {
	if override := e.extractReviewAuthorityOverride(item.Number, item.Labels); override != "" {
		return override
	}
	return stage.ReviewAuthority
}

// reviewGateAuthorityVerdict is the additive check `review_authority:
// authoritative` mode applies inside the existing "outstanding == 0 &&
// hasReviews" clearing branch of both checkReviewGate and
// reviewGateBlocksLanding — it never runs in "advisory" mode (the caller gates
// the call itself) and never widens what advisory already clears, only
// narrows it further. See ADR-1250.
//
// When reviewDecision is one of GitHub's real branch-protection-review-requirement
// values (APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED — computed server-side,
// including CODEOWNERS and required-approval-count rules), that value is
// authoritative: satisfied only on APPROVED. When reviewDecision is empty (no
// branch-protection review requirement configured on this repo), authoritative
// mode must not become a silent no-op — it falls back to Fabrik's own
// computation: satisfied unless any non-DISMISSED review in reviews is in
// CHANGES_REQUESTED state. Any other non-empty value (an undocumented future
// GitHub enum addition, or a schema change) blocks conservatively instead of
// silently falling through to the no-branch-protection fallback — GitHub did
// report a real verdict here, just not one this function recognizes, so
// treating it as "no requirement configured" could wrongly satisfy the gate.
func reviewGateAuthorityVerdict(reviewDecision string, reviews []gh.PRReview) (satisfied bool, reason string) {
	switch reviewDecision {
	case "":
		// No branch-protection review requirement configured — fall through to
		// Fabrik's own computation below.
	case "APPROVED":
		return true, "reviewDecision=APPROVED"
	case "CHANGES_REQUESTED":
		return false, "reviewDecision=CHANGES_REQUESTED"
	case "REVIEW_REQUIRED":
		// Covers both "zero reviews submitted yet against a branch-protection
		// requirement" and "some, but not enough, approvals" (e.g. a
		// required-approval-count of 2 with only 1 approval so far) — GitHub
		// reports the same REVIEW_REQUIRED value for both. This function is
		// only reached once hasReviews is already true (checkReviewGate/
		// reviewGateBlocksLanding gate on outstanding==0 && hasReviews before
		// calling in), so in practice this always represents the
		// not-enough-approvals case, not the zero-reviews case — but the
		// block-conservatively behavior is correct for either.
		return false, "reviewDecision=REVIEW_REQUIRED"
	default:
		return false, fmt.Sprintf("unrecognized reviewDecision=%q, blocking conservatively", reviewDecision)
	}

	// No branch-protection review requirement configured — fall back to
	// Fabrik's own outstanding-reviewer + no-CHANGES_REQUESTED computation.
	// DISMISSED reviews are excluded — a dismissed CHANGES_REQUESTED review is
	// no longer an active verdict, mirroring reviewGateOutstanding's hasReviews
	// computation.
	for _, r := range reviews {
		if r.State == "CHANGES_REQUESTED" {
			return false, fmt.Sprintf("no branch-protection review requirement; %s requested changes", r.Author)
		}
	}
	return true, "no branch-protection review requirement; no outstanding CHANGES_REQUESTED review"
}

// reviewGateBlocksLanding is the landing-decision review gate (#1216). It reports
// whether a wait_for_reviews stage must be held back from its landing decision
// (auto-merge enable, enqueue, direct merge, or advance-to-Queued) because reviewer
// requests are still outstanding.
//
// review_authority: authoritative (ADR-1250) additionally gates the same
// clearing branch on reviewGateAuthorityVerdict — no outstanding
// CHANGES_REQUESTED and required approvals satisfied. Because this sits ahead
// of attemptMergeOnValidate's yolo/cruise merge logic, `yolo`/`cruise` never
// bypass it: they only control merge *timing* once the authoritative gate is
// itself satisfied, never the gate's clearing condition.
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
		// Authoritative mode: additive check, same reviewGateAuthorityVerdict
		// pure function checkReviewGate uses, so the advance gate and the
		// landing gate can never disagree on what "satisfied" means. A fetch
		// error blocks conservatively — same rationale as the FetchPRReviews/
		// FetchPRReviewRequests failure handling just above; a transient
		// GraphQL error must never silently fall back to advisory clearing.
		if e.effectiveReviewAuthority(item, stage) == "authoritative" {
			reviewDecision, decisionErr := e.readClient.FetchPRReviewDecision(owner, repo, prNumber)
			if decisionErr != nil {
				e.logf(item.Number, "warn", "reviewGateBlocksLanding: FetchPRReviewDecision failed: %v\n", decisionErr)
				return e.holdLandingForReview(item,
					"holding landing decision on PR #%d — review verdict unreadable (fetch failed), blocking conservatively\n", prNumber)
			}
			if satisfied, reason := reviewGateAuthorityVerdict(reviewDecision, reviews); !satisfied {
				return e.holdLandingForReview(item,
					"holding landing decision on PR #%d — authoritative gate still blocking: %s\n", prNumber, reason)
			}
		}
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
//
// authorityReason is non-empty only when the caller's authoritative-mode
// verdict check blocked (see reviewGateAuthorityVerdict) — i.e. outstanding is
// empty and reviews exist, but the verdict itself isn't satisfied. When set,
// it is used verbatim as the pause reason instead of the generic
// "pending reviewers"/"no reviews submitted yet" messages, which would
// otherwise misleadingly suggest nobody has reviewed at all.
// reviewTimeoutReason builds the log-only reason string for the pause
// branch of checkAwaitingReviewTimeout. authorityReason, when non-empty,
// takes precedence: the gate is blocking on an authoritative verdict, not a
// plain review count. Otherwise, when reviewers are still outstanding, name
// them. When neither applies, no reviewer was ever requested — state the
// three things the engine actually knows (no reviewer requested, no review
// received, can't determine if one is coming) instead of speculating about
// bots that may not exist on this repo.
func reviewTimeoutReason(outstanding []string, authorityReason string) string {
	if authorityReason != "" {
		return "authoritative gate blocking: " + authorityReason
	}
	if len(outstanding) > 0 {
		return "pending reviewers: " + strings.Join(outstanding, ", ")
	}
	return "no reviewers were requested, no review has been received, and Fabrik cannot determine whether one is coming"
}

func (e *Engine) checkAwaitingReviewTimeout(owner, repo string, item gh.ProjectItem, outstanding []string, allBots, reprompted bool, authorityReason string) (blocked, timedOut, done bool) {
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
		reason := reviewTimeoutReason(outstanding, authorityReason)
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

// currentHeadReviewThreadComments narrows buildReviewThreadComments to
// comments whose parent thread GitHub itself has not marked isOutdated — i.e.
// threads still anchored to the PR's current head SHA. A thread opened
// against a commit that has since been superseded by a new push must not
// block the yolo-merge guards at attemptMergeOnValidate and
// handleReviewGate's auto-merge-disable path (#1207); GitHub computes
// isOutdated from whether the commented lines still exist unchanged in the
// current diff, so no separate SHA-comparison logic is needed here.
func (e *Engine) currentHeadReviewThreadComments(item gh.ProjectItem) []gh.Comment {
	all := e.buildReviewThreadComments(item)
	out := make([]gh.Comment, 0, len(all))
	for _, c := range all {
		if c.IsOutdated {
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
		// Standard timeout: branches below on whether any reviewer was ever
		// requested (pendingLine == "" means none was) AND whether any review
		// has actually been submitted. pendingLine alone is not sufficient:
		// once a requested reviewer submits a formal review (e.g.
		// CHANGES_REQUESTED), GitHub drops them from the requested-reviewers
		// list, so pendingLine goes back to "" even though a review exists.
		// That combination is reachable in authoritative mode, where the gate
		// can still be blocking on that review's verdict — see #1268 review
		// thread.
		pendingLine := ""
		if len(reviewerParts) > 0 {
			pendingLine = "\n\nPending reviewers: " + strings.Join(reviewerParts, ", ")
		}
		_, hasReviews := reviewGateOutstanding(item.LinkedPRReviewRequests, item.LinkedPRReviews)
		// Authoritative-mode context: live-fetch the verdict and reuse the same
		// reviewGateAuthorityVerdict pure function both gate sites consult, so
		// this message can never disagree with why the gate is actually
		// blocking — unlike a narrower heuristic (e.g. scanning only for a
		// CHANGES_REQUESTED review), this also surfaces REVIEW_REQUIRED and
		// fetch-failure cases, not just an active CHANGES_REQUESTED review.
		// item.LinkedPRNumber (and LinkedPRReviews with it) is always 0/empty
		// for a base:<branch> item — closedByPullRequestsReferences is
		// structurally empty there. Resolve both via the same REST fallback
		// checkReviewGate/reviewGateBlocksLanding use, so this line is still
		// populated on a base:<branch> repo instead of silently omitted.
		authorityLine := ""
		if e.effectiveReviewAuthority(item, stage) == "authoritative" {
			owner, repo := itemOwnerRepo(item, e.defaultRepo())
			prNumber := item.LinkedPRNumber
			reviews := item.LinkedPRReviews
			if prNumber == 0 {
				if pr, err := e.readClient.FetchLinkedPR(owner, repo, item.Number); err == nil && pr != nil && pr.Number != 0 {
					prNumber = pr.Number
					if restReviews, err := e.readClient.FetchPRReviews(owner, repo, prNumber); err == nil {
						reviews = restReviews
						_, hasReviews = reviewGateOutstanding(nil, reviews)
					}
				}
			}
			if prNumber > 0 {
				if reviewDecision, err := e.readClient.FetchPRReviewDecision(owner, repo, prNumber); err != nil {
					authorityLine = fmt.Sprintf(
						"\n\nReview authority is `authoritative` for this stage — the review verdict could not be "+
							"read (%v); the gate is blocking conservatively until it can be.",
						err,
					)
				} else if satisfied, reason := reviewGateAuthorityVerdict(reviewDecision, reviews); !satisfied {
					authorityLine = fmt.Sprintf(
						"\n\nReview authority is `authoritative` for this stage — %s, "+
							"and the gate will not clear until that is resolved.",
						reason,
					)
				}
			}
		}
		if pendingLine == "" && !hasReviews {
			// No reviewer was ever requested on this PR, and no review has
			// been submitted either — waiting longer cannot satisfy the gate.
			// Say so plainly instead of claiming a wait on "outstanding
			// reviewers" that don't exist, and offer the same remedies as
			// Phase 2. The !hasReviews check matters: in authoritative mode a
			// reviewer who submitted a formal review is no longer "pending"
			// (pendingLine goes back to ""), but a review did happen — that
			// case must fall through to the other branch below instead of
			// falsely claiming none was submitted.
			prRef := "the linked PR"
			if item.LinkedPRNumber > 0 {
				prRef = fmt.Sprintf("PR #%d", item.LinkedPRNumber)
			}
			msg = fmt.Sprintf(
				"🏭 **Fabrik — review wait timeout**\n\n"+
					"The review gate for stage **%s** timed out. No reviewer was ever requested on %s, and no review "+
					"has been submitted — Fabrik cannot determine whether one is ever coming.%s\n\n"+
					"Fabrik has paused this issue. To resume, either:\n"+
					"- (a) post a review on %s yourself — a `COMMENTED` self-review from the PR author satisfies "+
					"the gate, even though GitHub forbids self-approval,\n"+
					"- (b) set `wait_for_reviews: false` in the %s stage YAML if this repo has no reviewer,\n"+
					"- (c) merge %s manually, or\n"+
					"- (d) remove `fabrik:paused` to let the engine wait again.",
				stage.Name, prRef, authorityLine, prRef, stage.Name, prRef,
			)
		} else {
			// Standard timeout message with named reviewers — unchanged.
			msg = fmt.Sprintf(
				"🏭 **Fabrik — review wait timeout**\n\nThe review gate for stage **%s** timed out waiting for outstanding reviewers.%s%s\n\n"+
					"Fabrik has paused this issue. Please check the PR for pending reviews, address any issues, and then remove the `fabrik:paused` label to resume.",
				stage.Name, pendingLine, authorityLine,
			)
		}
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
