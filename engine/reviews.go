package engine

import (
	"context"
	"fmt"
	"slices"
	"strconv"
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

// reviewBodyIDPrefix marks a synthetic gh.Comment derived from a PR review's
// top-level body (Finding 4, #1375) rather than a real inline thread comment.
// GitHub's REST reactions API has no endpoint for a pulls/.../reviews/{id}
// object itself, so a review body has no ROCKET-reaction dedup backstop the
// way a real thread comment does (acknowledgeComments/finalizeComments skip
// reaction calls for DatabaseID==0 synthetics) — snap.CommentProcessed keyed
// on this ID is the *only* idempotency guard (R7). The prefix is structurally
// distinct from GraphQL node IDs (thread comments use those verbatim as ID),
// so a synthetic body ID can never collide with a real comment ID. Also
// doubles as the isReviewReinvoke/buildThreadEntries discriminator for a
// review-reinvoke batch that mixes thread comments and review bodies.
const reviewBodyIDPrefix = "review-body:"

// reviewBodyCommentID derives the stable synthetic ID for review's top-level
// body, keyed on PRReview.DatabaseID (the numeric PR review ID, already
// fetched — see github/types.go). Stable across polls so snap.CommentProcessed
// dedup (R7) actually works: the same review always produces the same ID.
func reviewBodyCommentID(review gh.PRReview) string {
	return reviewBodyIDPrefix + strconv.Itoa(review.DatabaseID)
}

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
	var fetchFailed bool
	if itemHasBaseLabel(item) && prNumber > 0 {
		restReviews, reviewsErr := e.readClient.FetchPRReviews(owner, repo, prNumber)
		restRequests, requestsErr := e.readClient.FetchPRReviewRequests(owner, repo, prNumber)
		fetchFailed = reviewsErr != nil || requestsErr != nil
		if fetchFailed {
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

	// FR-2 fast path: nothing requested, nothing reviewed yet, and the stage
	// explicitly declared expected_reviewers: [] (no unrequested reviewer is
	// expected). Placed after handleBrokenReviewLinkage/REST-fallback PR
	// resolution above, so it never fires on unconfirmed PR linkage. Recorded
	// in the trail (not silent) per FR-2 — this is the only "gate cleared"
	// exit that gets its own distinct log line, since it is the one clearing
	// path an operator might not expect. Gated on !fetchFailed, mirroring
	// reviewGateBlocksLanding's identical guard: a REST fetch failure already
	// zeroed reviewRequests/reviews above, which would otherwise look
	// identical to "genuinely nothing requested or reviewed" — advancing on
	// unknown review state would be exactly the fail-open regression
	// FR-6/FR-5 guard against. Also gated on prNumber > 0: handleBrokenReviewLinkage
	// returns (paused=false, prNumber=0) whenever FetchLinkedPR errors, finds no PR
	// on the branch, or finds one that isn't open/is merged (see
	// TestCheckReviewGate_BrokenLinkage_NoPRFound_FallsThrough) — a real, reachable
	// state distinct from "a PR exists and genuinely has nothing requested or
	// reviewed". Without this guard, an item declaring expected_reviewers: [] would
	// fast-advance past a not-yet-resolved PR, the same fail-open shape the
	// !fetchFailed guard exists to prevent, just reached via a different route.
	// reviewGateBlocksLanding does not need the equivalent guard explicitly here:
	// its own "no PR" branch (:583-585) already returns before prNumber is ever used
	// to fetch reviews, so it can never reach its fast-path call with prNumber == 0.
	expectedReviewers := e.effectiveExpectedReviewers(item, stage)
	if !fetchFailed && prNumber > 0 && reviewGateFastAdvance(outstanding, hasReviews, expectedReviewers) {
		e.logf(item.Number, "awaiting-review", "expected_reviewers declared none expected and nothing was requested — advancing immediately\n")
		e.removeAwaitingReviewLabel(owner, repo, item)
		return false, false, false
	}

	var declaredReviewers []string
	if expectedReviewers != nil {
		declaredReviewers = *expectedReviewers
	}
	// On a fetch failure, reviews is nil — "no matching review found" would be
	// indistinguishable from "review state unknown", so every declared
	// reviewer would read as outstanding purely from stale/unknown data. That
	// can flip reviewGateAllBots to true and fire a spurious Phase 1 @mention
	// re-prompt for a bot that may already have reviewed. Skip the
	// computation entirely on fetchFailed (declaredOutstanding stays nil) —
	// the gate still blocks via the generic "waiting for initial review
	// submission" path below, it just doesn't manufacture a false ladder
	// trigger from a transient API error.
	var declaredOutstanding []string
	if !fetchFailed {
		declaredOutstanding = declaredReviewersOutstanding(declaredReviewers, reviews)
	}

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

	// Determine if all outstanding reviewers are bots (or, when nothing was
	// formally requested, whether a declared reviewer is still outstanding).
	// Used by Phase 1/2 logic.
	//
	// Finding 2 (#1375): additionally require authorityReason == "". A
	// declared expected_reviewers bot must never preempt a human escalation —
	// authorityReason is only ever non-empty once outstanding is already empty
	// (a requested human has responded but with a verdict authoritative mode
	// does not accept), so this can never change behavior for the "human
	// hasn't responded yet" case (outstanding non-empty already forces
	// allBots=false via the loop below); it only closes the gap where a human
	// *has* responded with a blocking verdict but a declared bot is still
	// outstanding — without this, that declared bot's re-prompt ladder would
	// defer the human escalation by a full ReviewWaitTimeout window.
	allBots := reviewGateAllBots(reviewRequests, outstanding, declaredOutstanding) && authorityReason == ""

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
	if blocked, timedOut, done := e.checkAwaitingReviewTimeout(owner, repo, item, outstanding, declaredOutstanding, allBots, reprompted, authorityReason); done {
		return blocked, timedOut, false
	}

	if authorityReason != "" {
		e.logf(item.Number, "awaiting-review", "authoritative gate still blocking: %s\n", authorityReason)
	} else if len(outstanding) > 0 {
		e.logf(item.Number, "awaiting-review", "waiting for reviewers: %s\n", strings.Join(outstanding, ", "))
	} else if len(declaredOutstanding) > 0 {
		e.logf(item.Number, "awaiting-review", "waiting for declared reviewer(s): %s\n", strings.Join(declaredOutstanding, ", "))
	} else {
		e.logf(item.Number, "awaiting-review", "waiting for initial review submission (no reviewers requested, none declared via expected_reviewers)\n")
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

// reviewGateFastAdvance reports whether the FR-2 fast path fires: nothing is
// outstanding (no requested reviewer left to respond) and no review has been
// submitted yet, and the stage explicitly declared expected_reviewers: []
// (no unrequested reviewer is expected on this PR). A reviewer actually
// requested on the PR (non-empty outstanding) or an already-submitted review
// always takes precedence over the declaration — expected_reviewers only
// narrows waiting for *unrequested* reviewers; it never bypasses
// wait_for_reviews for a reviewer GitHub is genuinely tracking. An undeclared
// stage (expected == nil) never fast-advances, preserving FR-5's default.
//
// Deliberately independent of review_authority: this path only ever fires
// when hasReviews is false, and authoritative mode's verdict-weighing
// (reviewGateAuthorityVerdict) only ever runs once hasReviews is true — there
// is no verdict to weigh yet. Gating this on review_authority would make an
// authoritative stage wait out the full timeout every cycle for a review
// the stage's own declaration says will never come, recreating the exact
// stall (#1080) this feature exists to eliminate. See
// docs/USER_GUIDE.md#declaring-expected-reviewers.
func reviewGateFastAdvance(outstanding []string, hasReviews bool, expected *[]string) bool {
	if len(outstanding) > 0 || hasReviews {
		return false
	}
	return expected != nil && len(*expected) == 0
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

// expectedReviewersSyntheticName is the fixed identity the
// "expected-reviewers:declared" per-issue override (#1304) resolves to. It is
// a testing/e2e hook, not a general "declare a real reviewer" feature — it
// never actually posts a review on a real PR. Applying it to a production
// issue causes the gate to run out the full re-prompt ladder before pausing.
// Must match tests/e2e/expected_reviewers_test.go's expectedReviewersSyntheticName
// exactly; production code cannot import test files, so the value is
// independently duplicated here, mirroring how "advisory"/"authoritative" are
// already duplicated between this file and the e2e suite.
const expectedReviewersSyntheticName = "e2e-synthetic-declared-reviewer"

// expectedReviewersOverrideRank lists expected-reviewers override modes from
// least to most restrictive. "declared" imposes strictly more waiting
// obligation than "none"'s immediate fast-advance (reviewGateFastAdvance), so
// it is the more restrictive mode — same "prefer to keep waiting" direction
// reviewAuthorityRank encodes for authoritative > advisory. See #1304.
var expectedReviewersOverrideRank = []string{"none", "declared"}

// extractExpectedReviewersOverride scans item labels for
// "expected-reviewers:<mode>" labels and returns the resolved *[]string
// override, or nil when no valid label is found (stage config governs).
// Mirrors extractReviewAuthorityOverride's shape (#1261): if multiple
// recognized labels are present, it resolves to the more restrictive one
// ("declared" wins) and logs a warning listing all found labels — the same
// "pick deterministically, don't arbitrate" convention. A label whose suffix
// is not exactly "none" or "declared" (typo, casing, unknown value) is
// ignored with a logged warning — never a hard failure, never a silent
// escalation to "declared".
//
// Unlike extractReviewAuthorityOverride's "" empty-string sentinel for "not
// found" (string can't distinguish unset from a valid ""), nil is
// unambiguous here because neither valid override value (&[]string{} or
// &[]string{expectedReviewersSyntheticName}) is ever nil.
func (e *Engine) extractExpectedReviewersOverride(issueNumber int, labels []string) *[]string {
	const prefix = "expected-reviewers:"
	found := make(map[string]bool)
	var malformed []string
	for _, label := range labels {
		if !strings.HasPrefix(label, prefix) {
			continue
		}
		mode := strings.TrimPrefix(label, prefix)
		if mode == "none" || mode == "declared" {
			found[mode] = true
		} else {
			malformed = append(malformed, label)
		}
	}
	if len(malformed) > 0 {
		e.logf(issueNumber, "warn", "unrecognized expected-reviewers: label(s) %s (must be none or declared); ignoring\n",
			strings.Join(malformed, ", "))
	}
	if len(found) == 0 {
		return nil
	}
	if len(found) > 1 {
		all := make([]string, 0, len(found))
		for m := range found {
			all = append(all, "expected-reviewers:"+m)
		}
		e.logf(issueNumber, "warn", "multiple expected-reviewers: labels found (%s); using more restrictive (declared)\n", strings.Join(all, ", "))
	}
	// Return the more restrictive mode present.
	for i := len(expectedReviewersOverrideRank) - 1; i >= 0; i-- {
		mode := expectedReviewersOverrideRank[i]
		if !found[mode] {
			continue
		}
		if mode == "none" {
			return &[]string{}
		}
		return &[]string{expectedReviewersSyntheticName}
	}
	return nil
}

// effectiveExpectedReviewers resolves the effective expected_reviewers value
// for item, applying the per-issue "expected-reviewers:<mode>" label override
// (#1304) on top of the stage's YAML-configured value. checkReviewGate,
// reviewGateBlocksLanding, and pauseForReviewTimeout's message-only check all
// consult this instead of reading stage.ExpectedReviewers directly, so the
// two gates (and the pause message describing them) can never disagree about
// which value is in effect for a given issue.
//
// Precedence: no expected-reviewers: label on the issue → stage.ExpectedReviewers
// governs unchanged (nil stays nil — FR-5 default preserved). Exactly one
// recognized label → it overrides the stage config for this issue only. Both
// labels present → resolves to &[]string{expectedReviewersSyntheticName}
// (logged warning). Malformed/unknown label → ignored (logged warning), falls
// back to stage config.
func (e *Engine) effectiveExpectedReviewers(item gh.ProjectItem, stage *stages.Stage) *[]string {
	if override := e.extractExpectedReviewersOverride(item.Number, item.Labels); override != nil {
		return override
	}
	return stage.ExpectedReviewers
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

	// FR-2 fast path, same shared pure function checkReviewGate uses so the
	// advance gate and the landing gate can never disagree on when it fires
	// (including its deliberate independence from review_authority — see
	// reviewGateFastAdvance's doc comment). Gated on !fetchFailed: a fetch
	// failure already zeroed reviews/requests above, which would otherwise
	// look identical to "genuinely nothing requested or reviewed" —
	// advancing a landing on unknown review state would be exactly the
	// fail-open regression FR-6/FR-5 guard against.
	if !fetchFailed && reviewGateFastAdvance(outstanding, hasReviews, e.effectiveExpectedReviewers(item, stage)) {
		e.logf(item.Number, "awaiting-review", "landing decision on PR #%d — expected_reviewers declared none expected and nothing was requested — proceeding immediately\n", prNumber)
		return false
	}

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

// reviewGateAllBots reports whether the bot-escalation ladder (Phase 1/2)
// should be reachable: either every outstanding *requested* reviewer is a
// bot, or — when nothing was formally requested — at least one *declared*
// expected_reviewers identity is still unmatched (declaredOutstanding). The
// declared-reviewer branch only activates when outstanding is empty: a
// reviewer GitHub is actually tracking always takes precedence over a
// declaration, so this never changes behavior for a formally requested
// reviewer (declaredOutstanding is ignored whenever outstanding is
// non-empty).
func reviewGateAllBots(reviewRequests []gh.ReviewRequest, outstanding, declaredOutstanding []string) bool {
	if len(outstanding) == 0 {
		return len(declaredOutstanding) > 0
	}
	allBots := true
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
	repromptedAt, err := e.labelAppliedAt(item, owner, repo, botRepromptedLabel)
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

// reviewTimeoutReason builds the log-only reason string for the pause
// branch of checkAwaitingReviewTimeout. authorityReason, when non-empty,
// takes precedence: the gate is blocking on an authoritative verdict, not a
// plain review count. Otherwise, when reviewers are still outstanding, name
// them. Next, a declared-but-unrequested reviewer (expected_reviewers, #1283)
// that hasn't responded is named. When none of those apply, no reviewer was
// ever requested or declared — state the three things the engine actually
// knows (no reviewer requested, no review received, can't determine if one
// is coming) instead of speculating about bots that may not exist on this
// repo.
func reviewTimeoutReason(outstanding, declaredOutstanding []string, authorityReason string) string {
	if authorityReason != "" {
		return "authoritative gate blocking: " + authorityReason
	}
	if len(outstanding) > 0 {
		return "pending reviewers: " + strings.Join(outstanding, ", ")
	}
	if len(declaredOutstanding) > 0 {
		return "declared reviewer(s) not yet responded: " + strings.Join(declaredOutstanding, ", ")
	}
	return "no reviewers were requested, no review has been received, and Fabrik cannot determine whether one is coming"
}

// checkAwaitingReviewTimeout implements the fabrik:awaiting-review timeout
// check: once ReviewWaitTimeout has elapsed since the label was applied, it
// either fires Phase 1 of the bot-reviewer escalation ladder (re-prompting
// every outstanding bot reviewer — both formally requested ones, via a
// DELETE+POST request mutation, and declared-but-unrequested ones, via a
// direct @mention comment with no request to mutate), lets an already-fired
// Phase 1 continue waiting for Phase 2, or pauses for a mixed/pure-human/
// no-PR-number gate. done=true means the caller should return (blocked,
// timedOut) directly; done=false means the label wasn't found, was found but
// the timeout hasn't elapsed yet, or Phase 1 already fired and Phase 2
// hasn't timed out yet — the caller falls through to the "still waiting"
// logging/label-apply tail.
//
// declaredOutstanding lists the declared expected_reviewers identities that
// have not yet been matched to a review (see declaredReviewersOutstanding).
// It only drives Phase 1 behavior when allBots is true via the
// declared-reviewer branch of reviewGateAllBots (i.e. outstanding is empty)
// — see that function's doc comment for why a formally requested reviewer
// always takes precedence.
//
// authorityReason is non-empty only when the caller's authoritative-mode
// verdict check blocked (see reviewGateAuthorityVerdict) — i.e. outstanding is
// empty and reviews exist, but the verdict itself isn't satisfied. When set,
// it is used verbatim as the pause reason instead of the generic
// "pending reviewers"/"declared reviewer(s) not yet responded"/"no reviewers
// were requested" messages built by reviewTimeoutReason, which would
// otherwise misleadingly suggest nobody has reviewed at all.
func (e *Engine) checkAwaitingReviewTimeout(owner, repo string, item gh.ProjectItem, outstanding, declaredOutstanding []string, allBots, reprompted bool, authorityReason string) (blocked, timedOut, done bool) {
	timeout := e.cfg.ReviewWaitTimeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	for _, l := range item.Labels {
		if l != "fabrik:awaiting-review" {
			continue
		}
		appliedAt, err := e.labelAppliedAt(item, owner, repo, "fabrik:awaiting-review")
		if err != nil {
			e.logf(item.Number, "warn", "could not fetch awaiting-review label timestamp: %v\n", err)
			return false, false, false
		}
		if appliedAt.IsZero() || time.Since(appliedAt) < timeout {
			return false, false, false
		}
		// 1× timeout elapsed.
		if allBots && item.LinkedPRNumber > 0 && !reprompted {
			// Phase 1: re-prompt all outstanding bot reviewers. Iterates
			// outstanding (already correctly resolved by checkReviewGate:
			// GraphQL-sourced item.LinkedPRReviewRequests on a default-branch
			// repo, REST-sourced on a base:<branch> repo where
			// item.LinkedPRReviewRequests is structurally empty — see the
			// comment at the top of checkReviewGate) rather than re-deriving
			// from item.LinkedPRReviewRequests directly, which would silently
			// no-op this entire mutation loop (and the declared-reviewer
			// dedup below, which also depends on requestedLogins) on a
			// base:<branch> repo even though allBots/outstanding correctly
			// identified an outstanding formally-requested bot.
			var repromptedLogins []string
			requestedLogins := outstanding
			for _, login := range outstanding {
				if login == "" {
					continue
				}
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
			// Declared-but-unrequested reviewers (FR-3/declaration semantics
			// point 2): there is no GitHub review request to delete/re-add, so
			// that mutation is skipped entirely for this path — post the
			// @mention comment directly. The mention text is run through
			// botMentionHandle, same as the formally-requested path above
			// (:854) — a declared identity is FR-8-validated to be a plausible
			// bare handle, but "plausible" isn't "resolves as an @mention":
			// e.g. a declared "copilot-pull-request-reviewer" is a valid App
			// slug that passes validateExpectedReviewers and correctly matches
			// a live "copilot-pull-request-reviewer[bot]" review author via
			// reviewerIdentityMatches' copilot-prefix collapse, but GitHub only
			// resolves the mention as "@copilot". Skip any name already
			// re-prompted above via the formal request path, so a reviewer
			// that happens to be both requested and declared isn't mentioned
			// twice in one cycle. Matched via reviewerIdentityMatches (not raw
			// string equality) — a declared "copilot" and a requested login
			// "copilot-pull-request-reviewer[bot]" refer to the same reviewer,
			// the same login-form mismatch reviewerIdentityMatches exists to
			// reconcile everywhere else in this file.
			for _, name := range declaredOutstanding {
				alreadyPrompted := false
				for _, login := range requestedLogins {
					if reviewerIdentityMatches(name, login) {
						alreadyPrompted = true
						break
					}
				}
				if alreadyPrompted {
					continue
				}
				msg := fmt.Sprintf("🏭 **Fabrik — review re-prompt**\n\n@%s just checking in — could you take a look at this PR?", botMentionHandle(name))
				if dbID, err := e.client.AddComment(owner, repo, item.LinkedPRNumber, msg); err != nil {
					e.logf(item.Number, "warn", "phase 1: could not post re-prompt comment for declared reviewer %s: %v\n", name, err)
				} else if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
					e.logf(item.Number, "warn", "phase 1: could not add 🚀 to re-prompt comment: %v\n", reactErr)
				}
				repromptedLogins = append(repromptedLogins, name)
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
		reason := reviewTimeoutReason(outstanding, declaredOutstanding, authorityReason)
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

// buildReviewBodyComments returns synthetic gh.Comments derived from the
// top-level body of each unaddressed PR review (Finding 4, #1375) —
// buildReviewThreadComments only ever looks at inline thread comments, which
// GitHub does not require, and misses the guaranteed part of a
// CHANGES_REQUESTED/COMMENTED review: its body (see reviewGateAuthorityVerdict's
// doc comment and GitHub's REST "Create a review for a pull request" contract).
//
// A review is skipped when:
//   - DatabaseID == 0 (not yet fetched/unavailable — no stable ID to key
//     dedup on)
//   - State == "APPROVED" (a body here is typically "LGTM", not something to
//     act on, and GitHub only *requires* a body for REQUEST_CHANGES/COMMENT)
//   - State == "DISMISSED" (no longer an active verdict — mirrors
//     reviewGateOutstanding's hasReviews computation)
//   - Body == "" (nothing to act on)
//   - already recorded via snap.CommentProcessed(reviewBodyCommentID(r)) — the
//     only dedup mechanism available, since no GitHub reaction endpoint exists
//     for a top-level review body (see reviewBodyIDPrefix's doc comment, R7)
//
// The returned gh.Comment carries DatabaseID: 0 and no ReviewThreadID — the
// pre-existing "synthetic comment" escape hatch in acknowledgeComments/
// finalizeComments (both already gate on DatabaseID == 0) skips reaction calls
// for it automatically.
//
// base:<branch> repos (Pruefer review finding, #1375): closedByPullRequestsReferences
// — and everything nested inside it, including latestReviews — is structurally
// empty there (GitHub only populates it for PRs targeting the repository
// default branch), so item.LinkedPRReviews is always empty regardless of the
// PR's actual review state. Mirrors checkReviewGate's own REST-fallback branch
// (#1046/#1047/#1050): resolves the PR number from item.LinkedPRNumber (always
// populated on a default-branch item) or, when zero, a plain FetchLinkedPR call
// — no pause side effects here, since handleReviewGate only ever reaches this
// call after checkReviewGate has already run handleBrokenReviewLinkage this
// same poll without returning terminated=true, so linkage is already known
// good (or genuinely absent). buildReviewThreadComments has the identical
// base:<branch> gap and remains unfixed — an existing, explicitly documented
// out-of-scope limitation (docs/state-machine.md's Review Gate section,
// "Out of scope: LinkedPRReviewThreadComments") that predates this issue and
// is not addressed here; only the review-body half (this function) gets the
// REST fallback, which is sufficient to make Finding 1/AC6's target
// scenario — a CHANGES_REQUESTED review with a body and zero inline
// comments — reachable on a base:<branch> repo too.
func (e *Engine) buildReviewBodyComments(item gh.ProjectItem) []gh.Comment {
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	snap, _ := e.store.Get(repoStr, item.Number)

	reviews := item.LinkedPRReviews
	if itemHasBaseLabel(item) {
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		prNumber := item.LinkedPRNumber
		if prNumber == 0 {
			pr, err := e.readClient.FetchLinkedPR(owner, repo, item.Number)
			if err != nil || pr == nil || pr.Number == 0 {
				return nil
			}
			prNumber = pr.Number
		}
		restReviews, err := e.readClient.FetchPRReviews(owner, repo, prNumber)
		if err != nil {
			e.logf(item.Number, "warn", "buildReviewBodyComments: FetchPRReviews failed: %v\n", err)
			return nil
		}
		reviews = restReviews
	}

	out := make([]gh.Comment, 0, len(reviews))
	for _, r := range reviews {
		if r.DatabaseID == 0 {
			continue
		}
		if r.State == "APPROVED" || r.State == "DISMISSED" {
			continue
		}
		if r.Body == "" {
			continue
		}
		id := reviewBodyCommentID(r)
		if !snap.CommentProcessed(id).IsZero() {
			continue
		}
		// Prefer the review's actual submission time (r.SubmittedAt, now
		// fetched via both the GraphQL and REST paths) so a mixed batch of
		// thread comments and a review body sorts/reads correctly by
		// chronology in the reinvoke prompt (engine/claude.go renders
		// c.CreatedAt verbatim). Fall back to time.Now() only when it's
		// genuinely unavailable (unparseable or a data source that doesn't
		// carry it) — never leave the comment with a zero timestamp.
		createdAt := r.SubmittedAt
		if createdAt.IsZero() {
			createdAt = time.Now()
		}
		out = append(out, gh.Comment{
			ID:        id,
			Author:    r.Author,
			Body:      r.Body,
			CreatedAt: createdAt,
		})
	}
	return out
}

// buildReviewFeedbackComments returns the full set of actionable review
// feedback for a review-reinvoke dispatch: unresolved inline thread comments
// (buildReviewThreadComments) plus unaddressed review bodies
// (buildReviewBodyComments, Finding 4). This is the set dispatchReviewReinvoke
// and handleReviewGate's reinvoke-before-block ordering (Finding 1) actually
// dispatch on — buildReviewThreadComments alone is retained standalone because
// its dedup/shape is also exercised independently by existing tests and the
// #1207 auto-merge-disable guard's isOutdated-aware narrowing
// (currentHeadReviewThreadComments).
func (e *Engine) buildReviewFeedbackComments(item gh.ProjectItem) []gh.Comment {
	out := e.buildReviewThreadComments(item)
	out = append(out, e.buildReviewBodyComments(item)...)
	return out
}

// currentHeadReviewFeedbackComments narrows buildReviewFeedbackComments to
// comments safe to trigger the #1207 auto-merge-disable guard: current-head
// thread comments (currentHeadReviewThreadComments) plus all review-body
// comments.
//
// R8 decision: a review body has no thread and therefore no GitHub-computed
// isOutdated signal — CommitID (the SHA a review was submitted against) is
// only ever populated via the REST path (FetchPRReviews), not the GraphQL
// latestReviews selection the default-branch path uses, so comparing it
// against the PR's current head SHA would require a schema change. Every
// review-body comment is therefore treated as always-current-head — an
// explicit, documented simplification (see adrs/1375-review-authority-reinvoke-not-pause.md),
// not an oversight. This is over-conservative, not unsafe: at worst it holds
// auto-merge for a review body that actually targets a stale commit, which is
// consistent with this file's established "block on unknown state" pattern.
func (e *Engine) currentHeadReviewFeedbackComments(item gh.ProjectItem) []gh.Comment {
	out := e.currentHeadReviewThreadComments(item)
	out = append(out, e.buildReviewBodyComments(item)...)
	return out
}

// pauseForReviewTimeout pauses the issue when the review wait timeout elapses.
// It applies fabrik:paused + fabrik:awaiting-input and posts an explanatory comment.
// If item.Labels contains the fabrik:bot-reprompted label (the pre-cleanup snapshot
// captured before checkReviewGate removed it), Phase 2 context is detected and a
// more specific "after re-prompt" message is posted.
// resolvedReviewData returns item.LinkedPRNumber/LinkedPRReviews when already
// populated, falling back to a live REST fetch otherwise. item.LinkedPRNumber
// (and LinkedPRReviews with it) is always 0/empty for a base:<branch> item —
// closedByPullRequestsReferences is structurally empty there — so this is the
// steady-state resolution path on such a repo, not an edge case. Shared by
// pauseForReviewTimeout's authoritative-mode and expected_reviewers status
// messaging so the two can never disagree about which reviews exist.
func (e *Engine) resolvedReviewData(item gh.ProjectItem) (prNumber int, reviews []gh.PRReview) {
	prNumber = item.LinkedPRNumber
	reviews = item.LinkedPRReviews
	if prNumber == 0 {
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		if pr, err := e.readClient.FetchLinkedPR(owner, repo, item.Number); err == nil && pr != nil && pr.Number != 0 {
			prNumber = pr.Number
			if restReviews, err := e.readClient.FetchPRReviews(owner, repo, prNumber); err == nil {
				reviews = restReviews
			}
		}
	}
	return prNumber, reviews
}

func (e *Engine) pauseForReviewTimeout(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "review-timeout", "review wait timeout elapsed — pausing for human intervention\n")

	// FR-4: when the stage declared one or more expected_reviewers, report
	// per-reviewer status ("Pruefer reviewed; Gemini did not") so a partial
	// response is diagnosable — unconditional on which branch below fires
	// (Phase 2 / mixed / generic), since a declared reviewer's status matters
	// regardless of why the pause happened. declaredOutstandingNames (the
	// subset that never responded) is reused below to fold declared-but-
	// unrequested reviewers into the Phase 2 "after re-prompt" detection,
	// which otherwise only sees LinkedPRReviewRequests (always empty for a
	// declared-only reviewer).
	var expectedReviewersLine string
	var declaredOutstandingNames []string
	expectedReviewers := e.effectiveExpectedReviewers(item, stage)
	if expectedReviewers != nil && len(*expectedReviewers) > 0 {
		_, resolvedReviews := e.resolvedReviewData(item)
		declared := *expectedReviewers
		declaredOutstandingNames = declaredReviewersOutstanding(declared, resolvedReviews)
		outstanding := make(map[string]bool, len(declaredOutstandingNames))
		for _, name := range declaredOutstandingNames {
			outstanding[name] = true
		}
		statuses := make([]string, 0, len(declared))
		for _, name := range declared {
			if outstanding[name] {
				statuses = append(statuses, fmt.Sprintf("`%s` did not respond", name))
			} else {
				statuses = append(statuses, fmt.Sprintf("`%s` reviewed", name))
			}
		}
		expectedReviewersLine = "\n\nExpected reviewers: " + strings.Join(statuses, "; ")
	}

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
	// they're still in the requests list) — plus any declared-but-unrequested
	// reviewer still outstanding (declaredOutstandingNames), which never
	// appears in LinkedPRReviewRequests at all.
	var repromptedLogins []string
	for _, l := range item.Labels {
		if l == botRepromptedLabel {
			for _, rr := range item.LinkedPRReviewRequests {
				if rr.IsBot && rr.Login != "" {
					repromptedLogins = append(repromptedLogins, rr.Login)
				}
			}
			repromptedLogins = append(repromptedLogins, declaredOutstandingNames...)
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
				"A re-prompt was sent, but no review was submitted in the additional waiting window.%s\n\n"+
				"Fabrik has paused this issue. To resume, either:\n"+
				"- (a) post a review on %s yourself,\n"+
				"- (b) remove `wait_for_reviews: true` from the %s stage YAML if bot reviews are unreliable on this repo,\n"+
				"- (c) merge %s manually, or\n"+
				"- (d) remove `fabrik:paused` to let the engine cycle through another re-prompt + wait.",
			stage.Name, botList, expectedReviewersLine, prRef, stage.Name, prRef,
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
					} else {
						// Conservative, matching checkReviewGate's failure handling:
						// a transient fetch failure must not leave hasReviews at its
						// stale item.LinkedPRReviews reading (always empty on this
						// base:<branch> path) and fall into the no-reviewer-requested
						// branch below, which would assert "no review has been
						// submitted" — a claim we can no longer support. Treat review
						// state as unknown-but-possibly-present instead.
						e.logf(item.Number, "warn", "pauseForReviewTimeout: FetchPRReviews failed: %v\n", err)
						hasReviews = true
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
		if pendingLine == "" && !hasReviews && len(declaredOutstandingNames) == 0 {
			// No reviewer was ever requested on this PR, none was declared
			// (expected_reviewers, #1283) either, and no review has been
			// submitted — waiting longer cannot satisfy the gate. Say so
			// plainly instead of claiming a wait on "outstanding reviewers"
			// that don't exist, and offer the same remedies as Phase 2. The
			// !hasReviews check matters: in authoritative mode a reviewer who
			// submitted a formal review is no longer "pending" (pendingLine
			// goes back to ""), but a review did happen — that case must
			// fall through to the other branch below instead of falsely
			// claiming none was submitted. The declaredOutstandingNames
			// check matters for the same reason: a declared-but-unrequested
			// reviewer that hasn't responded yet is a case Fabrik *can* name
			// (see the standard branch below, which surfaces it via
			// expectedReviewersLine) — framing this branch's "whether one is
			// ever coming" as declarable rather than unknowable would
			// contradict the operator's own declaration if it applied here.
			prRef := "the linked PR"
			if item.LinkedPRNumber > 0 {
				prRef = fmt.Sprintf("PR #%d", item.LinkedPRNumber)
			}
			// The self-review caveat is only true when Fabrik actually knows
			// (or can't rule out) that a self-COMMENT won't be enough: that's
			// exactly when authorityLine is non-empty — authoritative mode is
			// active and currently blocking, whether because branch
			// protection requires an approval or because the verdict
			// couldn't be read. When authorityLine is empty (advisory mode,
			// or authoritative with a confirmed non-blocking verdict), a
			// self-review is known to satisfy the gate outright, so stating
			// the caveat would contradict the PR's own "say what Fabrik
			// actually knows to be true right now" goal (#1268 review
			// thread).
			selfReviewCaveat := ""
			if authorityLine != "" {
				selfReviewCaveat = ", unless this stage is `authoritative` and the repo requires approving reviews, " +
					"in which case an approval from another account is needed"
			}
			msg = fmt.Sprintf(
				"🏭 **Fabrik — review wait timeout**\n\n"+
					"The review gate for stage **%s** timed out. No reviewer was ever requested on %s, and no review "+
					"has been submitted. Whether one is ever coming is declarable rather than unknowable — see remedy (b) below.%s\n\n"+
					"Fabrik has paused this issue. To resume, either:\n"+
					"- (a) post a review on %s yourself — a `COMMENTED` self-review from the PR author satisfies "+
					"the gate, even though GitHub forbids self-approval%s,\n"+
					"- (b) set `expected_reviewers: []` in the %s stage YAML to declare that no *unrequested* "+
					"reviewer is expected here — an explicitly requested reviewer is still honored,\n"+
					"- (c) set `wait_for_reviews: false` in the %s stage YAML to disable the review gate entirely "+
					"if this repo has no reviewer,\n"+
					"- (d) merge %s manually, or\n"+
					"- (e) remove `fabrik:paused` to let the engine wait again.",
				stage.Name, prRef, authorityLine, prRef, selfReviewCaveat, stage.Name, stage.Name, prRef,
			)
		} else if pendingLine == "" && hasReviews && authorityLine != "" {
			// No reviewer is currently outstanding, but a review does exist —
			// either a formerly-requested reviewer already responded (and was
			// dropped from the requested-reviewers list), or a self-review was
			// submitted by the PR author. Either way, nobody is "outstanding"
			// right now, so the standard message below (which literally says
			// "waiting for outstanding reviewers") would be false. Requiring
			// authorityLine != "" keeps this branch's "does not satisfy" claim
			// something Fabrik can actually back up: hasReviews can also be
			// true defensively (the FetchPRReviews-failure fallback above
			// treats review state as unknown-but-possibly-present, not
			// confirmed), in which case authorityLine may still be "" if the
			// separate reviewDecision fetch succeeded against that same stale,
			// empty review list — falling through to the standard branch below
			// rather than asserting a submitted review this branch can't
			// actually confirm (#1268 review thread).
			prRef := "the linked PR"
			if item.LinkedPRNumber > 0 {
				prRef = fmt.Sprintf("PR #%d", item.LinkedPRNumber)
			}
			msg = fmt.Sprintf(
				"🏭 **Fabrik — review wait timeout**\n\n"+
					"The review gate for stage **%s** timed out. No reviewer is currently outstanding on %s — a review "+
					"has already been submitted, but it does not satisfy this stage's authoritative requirement.%s%s\n\n"+
					"Fabrik has paused this issue. Please check the review status on %s, address any outstanding "+
					"feedback, and then remove the `fabrik:paused` label to resume.",
				stage.Name, prRef, authorityLine, expectedReviewersLine, prRef,
			)
		} else {
			// Standard timeout message with named reviewers, plus per-declared-
			// reviewer status (expectedReviewersLine, FR-4) when the stage
			// declared expected_reviewers — this is also where a declared
			// reviewer that never got a Phase 1 re-prompt (e.g. the PR number
			// wasn't resolved yet at the earlier timeout check) is surfaced.
			msg = fmt.Sprintf(
				"🏭 **Fabrik — review wait timeout**\n\nThe review gate for stage **%s** timed out waiting for outstanding reviewers.%s%s%s\n\n"+
					"Fabrik has paused this issue. Please check the PR for pending reviews, address any issues, and then remove the `fabrik:paused` label to resume.",
				stage.Name, pendingLine, authorityLine, expectedReviewersLine,
			)
		}
	}

	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})
}

// dispatchReviewReinvoke re-invokes the stage agent via processComments with
// synthetic review comments (unresolved inline thread comments plus
// unaddressed review bodies, Finding 4 — see buildReviewFeedbackComments). A
// thin wrapper over dispatchReinvoke, supplying only review's pre-dispatch
// emptiness precheck and its comment builder — the shared goroutine scaffold
// (WorkerEntered/semaphore/processComments) lives in reinvoke.go.
func (e *Engine) dispatchReviewReinvoke(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.dispatchReinvoke(ctx, board, item, stage, reinvokeOpts{
		tag: "review-reinvoke",
		// precheck runs synchronously before WorkerEntered/goroutine dispatch —
		// buildReviewFeedbackComments needs no workDir, so there's no reason to
		// incur ensureRepoReady/WorkerEntered churn for a same-poll no-op.
		precheck: func() bool {
			return len(e.buildReviewFeedbackComments(item)) > 0
		},
		build: func(workDir string) []gh.Comment {
			return e.buildReviewFeedbackComments(item)
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

// stripBotSuffix removes a trailing "[bot]" (case-insensitive) from login.
// GitHub's REST API reports a self-submitting bot's review author with this
// suffix (e.g. "handarbeit-pruefer[bot]"), while its GraphQL API and its live
// @mention surface both omit it. A declared expected_reviewers identity is
// validated at load time (FR-8) to never carry the suffix, so this is only
// ever meaningful on the live PRReview.Author side of a match.
func stripBotSuffix(login string) string {
	if strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return login[:len(login)-len("[bot]")]
	}
	return login
}

// reviewerIdentityMatches reports whether a declared expected_reviewers
// identity refers to the same reviewer as a live review author. Both sides
// are normalized the same way — trailing "[bot]" stripped, then the existing
// copilot-* mention collapse applied — before a case-insensitive compare, so
// a declared "handarbeit-pruefer" matches both the GraphQL-sourced author
// "handarbeit-pruefer" and the REST-sourced "handarbeit-pruefer[bot]", and a
// declared "copilot" matches the actual login
// "copilot-pull-request-reviewer[bot]".
func reviewerIdentityMatches(declared, author string) bool {
	return strings.EqualFold(botMentionHandle(stripBotSuffix(declared)), botMentionHandle(stripBotSuffix(author)))
}

// declaredReviewersOutstanding returns the subset of declared identities that
// have not yet been matched (via reviewerIdentityMatches) to a non-DISMISSED
// review in reviews. A DISMISSED review does not count as a response — the
// same rule reviewGateOutstanding applies to hasReviews. Returns nil when
// declared is empty (explicit "none expected", or the caller passed an
// undeclared stage's zero value).
func declaredReviewersOutstanding(declared []string, reviews []gh.PRReview) []string {
	var outstanding []string
	for _, name := range declared {
		matched := false
		for _, r := range reviews {
			if r.State == "DISMISSED" {
				continue
			}
			if reviewerIdentityMatches(name, r.Author) {
				matched = true
				break
			}
		}
		if !matched {
			outstanding = append(outstanding, name)
		}
	}
	return outstanding
}
