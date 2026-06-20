//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// TestConjunctiveCIReviewGate is the regression test for the conjunctive
// CI∧review gate (ADR-056 D2). The Validate stage in the test bed is already
// configured with both wait_for_ci: true and wait_for_reviews: true.
//
// The slow-gate CI check (~10 minutes, enrolled as required on
// handarbeit/fabrik-test-alpha/main) is triggered by placing "slow-ci-required"
// in the PR body. This creates a wide CI-await window without triggering the
// CI-fix reinvoke machinery.
//
// Pass criteria (R1–R5):
//   - R1: fabrik:awaiting-ci is applied after FABRIK_STAGE_COMPLETE; stage:Validate:complete
//     is absent during CI-await (withheld 2 min window).
//   - R2: fabrik:awaiting-ci clears when CI passes, then fabrik:awaiting-review appears.
//   - R3: A PR comment posted during CI-await receives 👀 reaction (not dropped).
//   - R4: Issue remains OPEN while fabrik:awaiting-review is present (advance suppressed).
//   - R5 (approval path): SubmitPRReview(APPROVE) clears the review gate; issue closes;
//     stage:Validate:complete applied; fabrik:paused never applied.
//   - R5 (timeout fallback): if no FABRIK_REVIEWER_TOKEN, review gate times out;
//     fabrik:paused + fabrik:awaiting-input appear; issue stays OPEN.
//
// Prerequisites:
//   - "slow-gate" enrolled as a required status check on fabrik-test-alpha/main.
//     Test skips gracefully if not enrolled.
//   - FABRIK_REVIEWER_TOKEN in test bed .env for the approval path. If absent AND
//     FABRIK_REVIEW_WAIT_TIMEOUT > 5, the test skips with an instructional message.
//     If absent AND FABRIK_REVIEW_WAIT_TIMEOUT ≤ 5, the timeout fallback path runs.
//   - FABRIK_REVIEW_WAIT_TIMEOUT=2 (minutes) in the test bed .env when using the
//     timeout fallback path (otherwise the default 15-minute wait is impractical).
//
// Wall-clock: ~65–100 min (approval path); ~35–60 min (timeout path). Use E2E_TIMEOUT=2h.
// Cost: ~$1.00–2.50.
func TestConjunctiveCIReviewGate(t *testing.T) {
	t.Parallel()
	// Skipped pending handarbeit/fabrik#925. R1 (CI gate holds at the 600s slow-gate
	// — the #917 fix) and R3 (comment processed during CI-await) pass, but R2/R5
	// (the review-gate approval path) cannot pass in the current test bed due to
	// three test-harness/environment confounds — none an engine bug:
	//   1. Engine identity collides with the reviewer: GITHUB_TOKEN=verveguy in the
	//      engine process env overrides FABRIK_TOKEN=arbeithand (shell env > .env),
	//      so PRs are authored by verveguy — the same identity as
	//      FABRIK_REVIEWER_TOKEN. GitHub forbids self-review, so RequestPRReviewer
	//      is a silent no-op and the R5 approval is impossible.
	//   2. Dual review gate: both Review and Validate have wait_for_reviews:true, so
	//      reviews gate at Review — not only at Validate as this test's R2 assumes.
	//   3. gemini-code-assist auto-reviews every alpha PR, clearing the gate
	//      (outstanding==0 && hasReviews) before any human approval path runs.
	// The engine's checkReviewGate logic is correct throughout. See #925.
	t.Skip("blocked on #925: review-gate approval path has identity/dual-gate/bot-reviewer confounds (engine is correct)")
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	assertSlowGateRequired(t, env, env.RepoAlpha)

	reviewerToken := readEnvFileReviewerToken(t, env)
	reviewWaitTimeout := readEnvFileReviewWaitTimeout(t, env)
	if reviewerToken == "" && reviewWaitTimeout > 5 {
		t.Skipf("FABRIK_REVIEWER_TOKEN not set in test bed .env and FABRIK_REVIEW_WAIT_TIMEOUT=%d min (> 5); "+
			"set FABRIK_REVIEWER_TOKEN to a non-author PAT for the approval path, or "+
			"set FABRIK_REVIEW_WAIT_TIMEOUT=2 (and restart Fabrik) for the timeout-fallback path",
			reviewWaitTimeout)
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	title := fmt.Sprintf("e2e conjunctive-ci-review-gate (%s)", stamp)

	num := FileIssue(t, env, env.RepoAlpha, title, conjunctiveCIReviewGateBody, "fabrik:yolo")
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d — waiting for Implement to complete", env.RepoAlpha, num)

	// Wait for Implement to complete so the linked PR exists.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:Implement:complete", 60*time.Minute)
	prNumber := LinkedPRNumber(t, env, env.RepoAlpha, num)
	t.Logf("Implement complete; PR #%d created for %s#%d", prNumber, env.RepoAlpha, num)

	// Establish an outstanding reviewer request so the review gate has something
	// to hold on. The engine's checkReviewGate only applies fabrik:awaiting-review
	// when the PR has outstanding requested reviewers; nothing requests one
	// automatically (validate.yaml has wait_for_reviews:true but no reviewers
	// list). Request the reviewer-token identity (a non-author account) now, well
	// before Validate completes. (Approval path only; the timeout-fallback path
	// has no token to resolve a reviewer login.)
	if reviewerToken != "" {
		reviewerLogin := TokenLogin(t, reviewerToken)
		RequestPRReviewer(t, env, env.RepoAlpha, prNumber, reviewerLogin)
		t.Logf("requested reviewer %q on PR #%d so the review gate engages", reviewerLogin, prNumber)
	}

	// R1: fabrik:awaiting-ci must appear after Validate fires (CI gate holds).
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-ci", 30*time.Minute)
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-ci")
	t.Logf("fabrik:awaiting-ci confirmed on %s#%d (CI gate is holding)", env.RepoAlpha, num)

	// R1 withheld window (2 min): stage:Validate:complete must NOT appear while
	// fabrik:awaiting-ci is present — CI has not yet passed (~10 min).
	withheldDeadline := time.Now().Add(2 * time.Minute)
	r1Checked := false
	for time.Now().Before(withheldDeadline) {
		labels, err := tryIssueLabels(env, env.RepoAlpha, num)
		if err != nil {
			t.Logf("transient error fetching labels during R1 withheld window: %v (retrying)", err)
			time.Sleep(15 * time.Second)
			continue
		}
		r1Checked = true
		for _, l := range labels {
			if l == "stage:Validate:complete" {
				t.Fatalf("stage:Validate:complete appeared during CI-await window — CI gate did not hold on %s#%d",
					env.RepoAlpha, num)
			}
		}
		time.Sleep(15 * time.Second)
	}
	if !r1Checked {
		t.Fatalf("failed to fetch labels at least once during the R1 withheld window on %s#%d", env.RepoAlpha, num)
	}
	t.Logf("R1 withheld window passed: CI gate held for 2 minutes on %s#%d", env.RepoAlpha, num)

	// R3: Post a regular PR comment while fabrik:awaiting-ci is still present.
	// The engine must process this comment (adding 👀 reaction) even during CI-await,
	// because itemNeedsWork returns true for new comments before the CI-gate guard.
	commentBody := fmt.Sprintf("e2e-conjunctive-gate-ci-await (%s)", stamp)
	CommentOnPR(t, env, env.RepoAlpha, prNumber, commentBody)
	t.Logf("posted PR comment %q on #%d during CI-await (R3)", commentBody, prNumber)

	// Wait for CI (~10 min) to pass — fabrik:awaiting-ci clears when
	// addCompleteLabelAndRemoveCI runs after checkCIGate returns cleared.
	WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:awaiting-ci", 20*time.Minute)
	t.Logf("fabrik:awaiting-ci cleared on %s#%d (CI gate passed)", env.RepoAlpha, num)

	// R3 verify: the PR comment must have received 👀 (eyes) reaction.
	// This confirms the comment was not silently dropped during CI-await.
	WaitForPRCommentReaction(t, env, env.RepoAlpha, prNumber, commentBody, "eyes", 10*time.Minute)
	t.Logf("R3 verified: 👀 reaction on PR comment confirms processing during CI-await on %s#%d",
		env.RepoAlpha, num)

	// R2: fabrik:awaiting-review must appear after CI clears.
	// (addCompleteLabelAndRemoveCI adds stage:Validate:complete; next poll
	// checkReviewGate fires and adds fabrik:awaiting-review — one poll cycle gap.)
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 5*time.Minute)
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-review")
	t.Logf("fabrik:awaiting-review confirmed on %s#%d (review gate is holding)", env.RepoAlpha, num)

	// R4 withheld window (2 min): advance must NOT occur while fabrik:awaiting-review
	// is present. Issue must remain OPEN even if stage:Validate:complete is already set
	// (current engine behavior: stage:Validate:complete IS added when CI clears, before
	// the review gate runs — asserting it absent here would be wrong; see #890).
	noteCompleteLogged := false
	r4Checked := false
	reviewWithheldDeadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(reviewWithheldDeadline) {
		state, err := tryIssueState(env, env.RepoAlpha, num)
		if err != nil {
			t.Logf("transient error fetching issue state during R4 withheld window: %v (retrying)", err)
			time.Sleep(15 * time.Second)
			continue
		}
		r4Checked = true
		if state == "CLOSED" {
			t.Fatalf("issue %s#%d closed during review-await window — review gate did not hold",
				env.RepoAlpha, num)
		}
		// Informational only: stage:Validate:complete is expected to be present during
		// review-await (current behavior). Not a failure — see issue #890 for reconciliation.
		if !noteCompleteLogged {
			if labels, lerr := tryIssueLabels(env, env.RepoAlpha, num); lerr == nil {
				for _, l := range labels {
					if l == "stage:Validate:complete" {
						t.Logf("note: stage:Validate:complete present during review-await (current behavior, not a failure — see #890)")
						noteCompleteLogged = true
						break
					}
				}
			}
		}
		time.Sleep(15 * time.Second)
	}
	if !r4Checked {
		t.Fatalf("failed to fetch issue state at least once during the R4 withheld window on %s#%d", env.RepoAlpha, num)
	}
	t.Logf("R4 withheld window passed: review gate held for 2 minutes on %s#%d", env.RepoAlpha, num)

	if reviewerToken != "" {
		// R5 — approval path: submit an approving review from the second identity.
		// GitHub forbids the PR author (@arbeithand) from approving their own PR, so
		// FABRIK_REVIEWER_TOKEN must be a different GitHub account.
		SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNumber, "APPROVE")
		t.Logf("submitted APPROVE review on %s PR #%d using reviewer token", env.RepoAlpha, prNumber)

		// Issue must close (yolo auto-merges after both gates clear).
		WaitForIssueClosed(t, env, env.RepoAlpha, num, 30*time.Minute)
		AssertLabelWasApplied(t, env, env.RepoAlpha, num, "stage:Validate:complete")
		AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:paused")
		t.Logf("R5 verified: %s#%d closed after approval; stage:Validate:complete applied, fabrik:paused never applied",
			env.RepoAlpha, num)
	} else {
		// R5 fallback — timeout path: with no second reviewer, the review gate
		// times out after FABRIK_REVIEW_WAIT_TIMEOUT minutes and pauses the issue.
		t.Logf("no FABRIK_REVIEWER_TOKEN — using review-timeout fallback path (FABRIK_REVIEW_WAIT_TIMEOUT=%d min)",
			reviewWaitTimeout)
		timeoutWait := time.Duration(reviewWaitTimeout+5) * time.Minute
		WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:paused", timeoutWait)
		t.Logf("fabrik:paused appeared on %s#%d (review gate timed out)", env.RepoAlpha, num)
		WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-input", 5*time.Minute)
		t.Logf("fabrik:awaiting-input appeared on %s#%d", env.RepoAlpha, num)
		if state := IssueState(t, env, env.RepoAlpha, num); state != "OPEN" {
			t.Fatalf("expected issue OPEN after review timeout, got %s on %s#%d",
				state, env.RepoAlpha, num)
		}
		t.Logf("R5 timeout path verified: %s#%d paused with fabrik:awaiting-input; review gate held (no premature advance)",
			env.RepoAlpha, num)
		t.Logf("NOTE: R5 approval path (joint-clear on review submission) was not exercised — " +
			"set FABRIK_REVIEWER_TOKEN in the test bed .env to test full joint-clear")
	}
}

// conjunctiveCIReviewGateBody is the issue body for TestConjunctiveCIReviewGate.
// Claude makes a minimal change to README.md and includes "slow-ci-required"
// in the PR body to trigger the ~10-minute slow-gate required CI check, creating
// a wide CI-await window for the conjunctive gate test.
const conjunctiveCIReviewGateBody = `## Goal

End-to-end regression test for the Fabrik conjunctive CI∧review gate
(handarbeit/fabrik#895, ADR-056 D2). Validates that both the CI gate and the
review gate must both be satisfied before Validate advances the issue.

## The change

Add exactly one new HTML comment to README.md, on its own line, immediately
after the line containing "# fabrik-test-alpha". The comment must be EXACTLY:

    <!-- conjunctive-ci-review-gate-test -->

This is the only change needed. No other files should be modified. Do NOT
reuse any other HTML comment from prior tests — the marker above is unique
to this test.

## CI behaviour required

The PR body MUST carry the literal marker below so the test repo's CI
slow-gate check fires (~10 minutes, enrolled as a required check):

slow-ci-required

This creates the CI-await window the test needs to verify the conjunctive gate
behaviour. Do NOT include ci-fix-sentinel-required in the PR body.

## Scope

Single file (README.md). Minimal one-line change. No decomposition.
Plan and Implement should be a one-commit change.
`
