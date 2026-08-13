//go:build e2e

package e2e

import (
	"fmt"
	"regexp"
	"slices"
	"testing"
	"time"
)

// This file covers ADR-1250's review_authority: authoritative mode
// end-to-end (issue #1258), using deterministic harness-posted verdicts
// (SubmitPRReview + FABRIK_REVIEWER_TOKEN) — never the bed's real reviewer
// (Pruefer, as of #1396; formerly claude-review.yml, now disabled) — for
// every verdict assertion.
//
// Mechanism: all scenarios seed a member PR directly via the GitHub API
// (CreateMemberPR, zero Claude cost) against the bed's existing Review
// column/stage (default, advisory config — untouched). Authoritative mode is
// applied per issue via the "review-authority:authoritative" label passed as
// an extraLabel to seedReviewGateItem; engine support for that label is
// #1261 (filed, decoupled from this issue — these scenarios cannot pass
// until both #1261 and this issue are merged). This exercises checkReviewGate
// (the advance gate) and its shared pure predicate reviewGateAuthorityVerdict
// without introducing any bed-local board column or stage-YAML variant — an
// earlier design that did so was rejected because review_authority is a
// property of a stage's config, not a distinct kind of stage, and because a
// bed prerequisite the operator hadn't yet set up would silently skip most of
// the scenarios, letting the suite go green having validated zero
// authoritative behavior. See adrs/1258-e2e-review-authority-coverage.md.
//
// Bed setup required: FABRIK_REVIEWER_TOKEN, and the
// "review-authority:authoritative" label must already exist as a label
// object in the target repo (gh issue create --label fails hard, not
// gracefully, if it doesn't — see README prerequisite). Neither is a bed
// column or stage-YAML variant, and no scenario skips for lack of either —
// a missing label surfaces as a t.Fatalf from FileIssue, by design (see the
// ADR's rationale for rejecting silent skips).
//
// Scope limitation (see adrs/1258-e2e-review-authority-coverage.md): the
// landing/auto-merge gate (reviewGateBlocksLanding, called only from
// attemptMergeOnValidate) is reachable ONLY through a stage literally named
// "Validate" (engine/stages.go, engine/poll.go, engine/pr_terminal_advance.go
// all hard-gate on stage.Name == "Validate"). Applying the authority label to
// an item sitting on Review cannot reach that stage-name-gated path, so
// scenarios 2/3 below assert the gate *clears* (fabrik:awaiting-review
// disappears, fabrik:paused never applied), not that the item merges —
// reviewGateBlocksLanding's wiring around the same shared predicate is a
// documented, accepted e2e gap, not a silently missing one.
//
// All scenarios only touch checkReviewGate (plus, as of #1375, the
// buildReviewFeedbackComments/dispatchReviewReinvoke reinvoke path it now
// unlocks), none of which have FABRIK_MERGE_TRAIN branching — they pass
// identically under both legs of the suite's two-mode gate.
//
// Determinism (#1312): the scenarios below that assert fabrik:awaiting-review
// as a *precondition* before their own deliberate verdict lands
// (ReinvokesOnChangesRequested, CycleLimitPauses, ClearsOnApproval,
// AdvisoryRegressionGuard) each call RequestPRReviewer right after seeding,
// making outstanding genuinely non-empty by construction. This corrects
// ADR-1258's original "no RequestPRReviewer call is needed" rationale — that
// reasoning covered checkReviewGate's outer clearing condition
// (len(outstanding)==0 && hasReviews) in isolation, but didn't account for
// the bed's own incidental reviewer (formerly claude-review.yml, now
// Pruefer as of #1396) satisfying that same condition with an incidental
// review before the engine's first gate evaluation, which is exactly what
// #1312 reports. YoloDoesNotBypassBlock's two fabrik:awaiting-review waits
// need no such fix: both occur after this test's own genuine
// REQUEST_CHANGES review is already submitted, and once a genuine
// CHANGES_REQUESTED review exists, an unrelated incidental bot review
// landing before or after it can never satisfy the outer clearing
// condition on its own — the wait is already deterministic. See
// adrs/1258-e2e-review-authority-coverage.md's Revision section.
//
// R3 (#1375): TestReviewAuthorityBlocksAndPausesOnChangesRequested — which
// asserted an authoritative CHANGES_REQUESTED escalates directly to
// fabrik:paused — has been retired and replaced by
// TestReviewAuthorityReinvokesOnChangesRequested (the primary reinvoke
// response, AC1/AC6/AC7) and TestReviewAuthorityCycleLimitPauses (the
// MaxReviewCycles terminal fallback, AC4). The old test encoded the
// pre-#1375 behavior; see adrs/1375-review-authority-reinvoke-not-pause.md
// for why that behavior was wrong and what replaces it.

// reviewBodyIDPattern extracts a review-body synthetic comment ID
// ("review-body:<database ID>", produced by engine/reviews.go's
// reviewBodyCommentID) from a dispatchReinvoke log line enriched by
// commentIDsForLog (engine/reinvoke.go, #1519). Used by AC7 below to key its
// "same review not reprocessed" check off the review's own identity, not off
// reinvoke count or timing.
var reviewBodyIDPattern = regexp.MustCompile(`review-body:\d+`)

// TestReviewAuthorityReinvokesOnChangesRequested covers AC1 and AC6 (R1/R3,
// #1375, ADR-1375). This supersedes the retired
// TestReviewAuthorityBlocksAndPausesOnChangesRequested and its expectation
// that an authoritative CHANGES_REQUESTED escalates directly to
// fabrik:paused: R1 decided authoritative mode governs merging, never
// working, so the primary response to a change request is a bounded
// reinvoke, not an immediate pause. Pausing is now reached only at
// FABRIK_MAX_REVIEW_CYCLES (R5), covered separately by
// TestReviewAuthorityCycleLimitPauses below.
//
// AC1 requires asserting an observable unique to a reinvoke — a stage
// invocation in the engine log — rather than a label transition (other paths,
// e.g. a plain "reviewer hasn't responded" timeout, also produce label
// churn). AC6 requires the review body to be non-empty with zero inline
// comments — exactly the shape SubmitPRReview produces (body-only
// REQUEST_CHANGES, no inline thread comments) — and confirms it alone
// triggers the reinvoke. AC7 requires the same review is not re-processed on
// a later poll: after the reinvoke completes, this test waits through a
// further poll window and asserts no reinvoke dispatch carries the same
// review's ID among its comments.
//
// AC7's premise (#1519, correcting the original design under #1375): a
// *second* review-reinvoke dispatch in the settle window is not by itself a
// violation. #1045 widened buildReviewBodyCommentsFromReviews's
// actionable-feedback filter so any non-DISMISSED, non-PENDING review body is
// actionable, not just CHANGES_REQUESTED — so a distinct, previously
// unprocessed review body (e.g. the bed's self-submitting bot reviewer
// posting a generic COMMENTED summary, which every real bed observes within
// seconds of the test's own review) correctly triggers its own reinvoke, and
// AC7 must tolerate it. What AC7 actually guards is narrower: the dedup on
// the *original* review (keyed on reviewBodyCommentID, "review-body:<review
// database ID>") must hold — that review's ID must never reappear in a later
// reinvoke's comment batch. dispatchReinvoke's log line is enriched (#1519,
// engine/reinvoke.go's commentIDsForLog) to carry the dispatched comment IDs
// specifically so this test can key the check off the original review's ID,
// not off dispatch count or timing — a widened match that merely tolerated
// *any* second reinvoke would satisfy R3 while reintroducing a vacuous test
// (R2), since it could no longer detect a genuine dedup regression.
//
// Requires #1261 (engine support for the review-authority:authoritative
// label) and #1375 (this issue) to be merged.
//
// Wall-clock: dominated by one real Claude invocation to address the review
// feedback (the reinvoke itself), typically several minutes, plus a bounded
// AC7 settle window.
func TestReviewAuthorityReinvokesOnChangesRequested(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required for deterministic verdict scenarios")
	}

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "reinvoke-changes", "review-authority:authoritative")

	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)
	reviewerLogin := TokenLogin(t, reviewerToken)
	if engineLogin := TokenLogin(t, env.GHToken); engineLogin == reviewerLogin {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the engine/PR author — "+
			"set FABRIK_REVIEWER_TOKEN to a distinct GitHub account's PAT", reviewerLogin)
	}

	// Request the reviewer so outstanding is genuinely non-empty by
	// construction, before confirming the gate's pre-verdict block. Without
	// this, the bed's real reviewer (Pruefer, as of #1396) can land its own
	// incidental review before the engine's first gate evaluation; that alone
	// satisfies checkReviewGate's outer len(outstanding)==0 && hasReviews
	// clearing branch, which never applies fabrik:awaiting-review — the wait
	// below would then time out against genuinely correct engine behavior
	// (#1312), rather than proving the gate engaged.
	RequestPRReviewer(t, env, env.RepoAlpha, prNum, reviewerLogin)
	t.Logf("requested reviewer %q on %s PR #%d — outstanding is non-empty by construction, gate engagement is deterministic", reviewerLogin, env.RepoAlpha, prNum)

	// Confirm the gate's trivial, pre-verdict block (hasReviews==false blocks
	// unconditionally) before the review lands — proves the gate is genuinely
	// engaged rather than a poll racing ahead of the review.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-review")
	t.Logf("fabrik:awaiting-review confirmed on %s#%d before any review submitted — gate is genuinely engaged", env.RepoAlpha, num)

	logOffset := LogOffset(t, env)
	submittedReviewID := SubmitPRReviewID(t, env, reviewerToken, env.RepoAlpha, prNum, "REQUEST_CHANGES")
	t.Logf("submitted REQUEST_CHANGES review %d on %s PR #%d (body-only, zero inline comments — AC6's target shape)", submittedReviewID, env.RepoAlpha, prNum)

	// AC1/AC6: the reinvoke must fire — assert the engine log line unique to
	// dispatchReinvoke's goroutine ("re-invoking stage ... via comment
	// processing", tagged "review-reinvoke"), not merely a label transition.
	// checkReviewGate still reports blocked=true here (the CHANGES_REQUESTED
	// verdict has not cleared) — before Finding 1's fix this reinvoke was
	// unreachable, and the gate would instead sit on fabrik:awaiting-review
	// until FABRIK_REVIEW_WAIT_TIMEOUT elapsed.
	firstReinvokeLine := WaitForLogLine(t, env, fmt.Sprintf("[#%d review-reinvoke] re-invoking stage", num), logOffset, 10*time.Minute)
	t.Logf("review-reinvoke dispatched for %s#%d — authoritative CHANGES_REQUESTED body alone triggered a reinvoke", env.RepoAlpha, num)

	// Key AC7 off the identity of the review THIS scenario submitted, taken
	// from the submit call's own response — not off "the only review-body: ID
	// in the log line."
	//
	// The reinvoke line legitimately enumerates every unaddressed review body,
	// and a bed PR can carry more than this scenario's own: the bed has a real
	// reviewer (Pruefer, #1396 — see the note above on why it forces the
	// explicit RequestPRReviewer call), and since #1045 every non-DISMISSED
	// review body is actionable. Observed here: the engine dispatched
	// "review-body:4923009567|review-body:4923009678" for one submitted review,
	// which is correct engine behaviour and only broke the old
	// exactly-one assumption. Same shape as #1519.
	originalReviewID := fmt.Sprintf("review-body:%d", submittedReviewID)
	dispatchedIDs := reviewBodyIDPattern.FindAllString(firstReinvokeLine, -1)
	if !slices.Contains(dispatchedIDs, originalReviewID) {
		t.Fatalf("first review-reinvoke log line %q does not name this scenario's own review %s (dispatched: %v) — "+
			"expected commentIDsForLog (engine/reinvoke.go) to have enriched it with the submitted review's synthetic ID",
			firstReinvokeLine, originalReviewID, dispatchedIDs)
	}
	t.Logf("original review's synthetic comment ID: %s (dispatched alongside %d review body/bodies total)", originalReviewID, len(dispatchedIDs))

	// The reinvoke, not a pause, is the primary response — fabrik:paused must
	// not have fired from this alone.
	AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:paused")
	t.Logf("AC1/AC6 verified: %s#%d reinvoked on an authoritative CHANGES_REQUESTED body with zero inline comments, never paused", env.RepoAlpha, num)

	// Wait for the reinvoke's Claude invocation to actually complete
	// (comments.go's finalizeComments logs "comment processing complete[d]")
	// before checking AC7 — CommentProcessed is only recorded once this
	// completes, and checking too early would let AC7 pass trivially without
	// having exercised the dedup this scenario targets.
	WaitForLogLine(t, env, fmt.Sprintf("[#%d done] comment processing complete", num), logOffset, 20*time.Minute)
	t.Logf("review-reinvoke completed for %s#%d", env.RepoAlpha, num)

	// AC7: the same review must not be re-processed on a later poll. The
	// dedup in buildReviewBodyCommentsFromReviews (engine/reviews.go) is keyed
	// per review-body ID (snap.CommentProcessed(reviewBodyCommentID(r))), so
	// once the original review is processed, its own ID must never reappear
	// in a later reinvoke's dispatched comment batch. A *different* review's
	// ID reappearing is not a violation — since #1045, any distinct,
	// previously unprocessed non-DISMISSED review body is actionable, and the
	// bed's self-submitting bot reviewer routinely posts a generic COMMENTED
	// summary within seconds of the test's own review, correctly triggering
	// its own reinvoke. This scans every review-reinvoke line in the settle
	// window (not just the first) and fails only if the original review's ID
	// is among them.
	settleOffset := LogOffset(t, env)
	settleDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(settleDeadline) {
		pollSleep(pollBase())
	}
	lines, err := allLogLinesContaining(env, fmt.Sprintf("[#%d review-reinvoke] re-invoking stage", num), settleOffset)
	if err != nil {
		t.Fatalf("scanning settle window for review-reinvoke lines: %v", err)
	}
	for _, line := range lines {
		if slices.Contains(reviewBodyIDPattern.FindAllString(line, -1), originalReviewID) {
			t.Fatalf("AC7 violated: a review-reinvoke dispatched for %s#%d reprocessed the already-processed review %s: %q",
				env.RepoAlpha, num, originalReviewID, line)
		}
		t.Logf("tolerated: a review-reinvoke dispatched for %s#%d for a different review (not %s), the bed's real steady-state behavior since #1045: %q",
			env.RepoAlpha, num, originalReviewID, line)
	}
	t.Logf("AC7 verified: %s#%d did not re-dispatch a reinvoke for the same, already-processed review %s", env.RepoAlpha, num, originalReviewID)
}

// TestReviewAuthorityCycleLimitPauses covers AC4: a reviewer requesting
// changes FABRIK_MAX_REVIEW_CYCLES times terminates in
// pauseForReviewCycleLimit, not an unbounded reinvoke loop. This is the
// terminal-fallback half of R1's reinvoke model (R5) — the primary,
// non-terminal reinvoke response is covered by
// TestReviewAuthorityReinvokesOnChangesRequested above.
//
// Requires a small bed-configured FABRIK_MAX_REVIEW_CYCLES for a bounded
// wall-clock: the engine dispatches one reinvoke (a real Claude invocation)
// per poll for as long as the gate blocks, so the run costs roughly that many
// invocations. Skips if the bed's configured value is too large to run in a
// reasonable e2e window.
//
// Note the test drives this with a SINGLE unresolved review, not one per
// cycle — see the comment at the submission below for why the previous
// one-review-per-cycle shape was racy against the poll loop.
func TestReviewAuthorityCycleLimitPauses(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required for deterministic verdict scenarios")
	}
	maxCycles := readEnvFileMaxReviewCycles(t, env)
	if maxCycles > 5 {
		t.Skipf("FABRIK_MAX_REVIEW_CYCLES=%d is too large for a bounded e2e run (the engine dispatches one reinvoke per poll while the gate blocks) — "+
			"set a small bed value (e.g. 2) for this test, see README", maxCycles)
	}

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "cycle-limit", "review-authority:authoritative")

	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)
	reviewerLogin := TokenLogin(t, reviewerToken)
	if engineLogin := TokenLogin(t, env.GHToken); engineLogin == reviewerLogin {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the engine/PR author — "+
			"set FABRIK_REVIEWER_TOKEN to a distinct GitHub account's PAT", reviewerLogin)
	}

	RequestPRReviewer(t, env, env.RepoAlpha, prNum, reviewerLogin)
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	t.Logf("fabrik:awaiting-review confirmed on %s#%d — gate is genuinely engaged", env.RepoAlpha, num)

	// A SINGLE unresolved CHANGES_REQUESTED review is enough to drive the whole
	// budget: while an authoritative gate is blocking, checkReviewGate
	// re-invokes once per poll ("authoritative gate still blocking:
	// reviewDecision=CHANGES_REQUESTED") without waiting for a fresh review,
	// and every such dispatch increments ReviewBlockedCycles — the counter
	// ADR-1518 deliberately never refunds, precisely so a non-converging gate
	// cannot loop forever behind #1045's ReviewCycles refund.
	//
	// This test previously submitted one review per cycle and waited for a
	// reinvoke after each, on the premise (in its doc comment) that "each cycle
	// requires a full reinvoke before the next distinct REQUEST_CHANGES review
	// can be submitted." The engine never promised that 1:1 mapping. It races
	// the poll loop and loses under -parallel contention: the engine burned all
	// maxCycles blocked cycles from earlier reviews and paused, so the final
	// iteration's WaitForLogLine sat waiting for a reinvoke that could no
	// longer come. Observed at offset 21233 scanning for a line the engine had
	// already written at 19615.
	offset := LogOffset(t, env)
	SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNum, "REQUEST_CHANGES")
	t.Logf("submitted one unresolved REQUEST_CHANGES review on %s PR #%d — the engine now self-drives its blocked-cycle budget", env.RepoAlpha, prNum)

	// The terminal assertion (AC4): bounded, and terminating in the cycle-limit
	// pause rather than an unbounded loop.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:paused", 20*time.Minute)
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-input", 5*time.Minute)
	WaitForPRCommentContainingAny(t, env, env.RepoAlpha, num,
		[]string{"review cycle limit", "maximum configured limit"}, 5*time.Minute)

	// Assert the bound itself, not merely that a pause eventually happened:
	// exactly maxCycles reinvokes may be dispatched before the pause. Counting
	// the log lines is what distinguishes "bounded at the configured limit"
	// from "paused for some other reason after an arbitrary number of
	// reinvokes" — the latter would satisfy the labels above while still being
	// the unbounded loop AC4 exists to rule out.
	reinvokes := CountLogLines(t, env, fmt.Sprintf("[#%d review-reinvoke] re-invoking stage", num), offset)
	if reinvokes != maxCycles {
		t.Errorf("expected exactly %d review-reinvoke dispatch(es) before the cycle-limit pause on %s#%d, got %d — "+
			"fewer means the pause fired early (not at the configured limit); more means the bound leaked",
			maxCycles, env.RepoAlpha, num, reinvokes)
	}
	t.Logf("AC4 verified: %s#%d paused via the review cycle limit after exactly %d reinvoke dispatch(es), not an unbounded loop", env.RepoAlpha, num, reinvokes)
}

// TestReviewAuthorityClearsOnApproval covers scenario 2: authoritative +
// APPROVED clears the gate. Descoped from "merges" to "gate clears" — see
// the file-level doc comment on the landing-gate scope limitation.
//
// Requires #1261 (engine support for the review-authority:authoritative
// label) to be merged.
//
// Wall-clock: ~2-5 min.
func TestReviewAuthorityClearsOnApproval(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required for deterministic verdict scenarios")
	}

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "clears-approval", "review-authority:authoritative")

	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)
	reviewerLogin := TokenLogin(t, reviewerToken)
	if engineLogin := TokenLogin(t, env.GHToken); engineLogin == reviewerLogin {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the engine/PR author — "+
			"set FABRIK_REVIEWER_TOKEN to a distinct GitHub account's PAT", reviewerLogin)
	}

	// Request the reviewer so outstanding is genuinely non-empty by
	// construction, before confirming the gate engages. Without this, the
	// bed's real reviewer (Pruefer, as of #1396) can land its own incidental
	// review before the engine's first gate evaluation and before this test's
	// own APPROVE — that alone satisfies checkReviewGate's outer
	// len(outstanding)==0 && hasReviews clearing branch, which never applies
	// fabrik:awaiting-review. The wait below would then time out against
	// genuinely correct engine behavior (#1312) instead of proving the gate
	// engaged before the deliberate approval.
	RequestPRReviewer(t, env, env.RepoAlpha, prNum, reviewerLogin)
	t.Logf("requested reviewer %q on %s PR #%d — outstanding is non-empty by construction, gate engagement is deterministic", reviewerLogin, env.RepoAlpha, prNum)

	// Confirm the gate genuinely engages (no review submitted yet, so
	// checkReviewGate's outer hasReviews==false condition blocks and applies
	// the label unconditionally) before approving. Without this, a poll
	// racing ahead of SubmitPRReview below could observe the item only after
	// the approval already exists, clearing on the very first evaluation and
	// never applying fabrik:awaiting-review at all — the later
	// WaitForLabelAbsent would then pass trivially without having exercised
	// the authoritative clearing transition this scenario exists to check.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-review")
	t.Logf("fabrik:awaiting-review confirmed on %s#%d before approval — gate is genuinely engaged", env.RepoAlpha, num)

	SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNum, "APPROVE")
	t.Logf("submitted APPROVE review on %s PR #%d", env.RepoAlpha, prNum)

	WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	t.Logf("fabrik:awaiting-review cleared on %s#%d — authoritative gate cleared on APPROVED", env.RepoAlpha, num)

	AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:paused")
	t.Logf("R2 verified: %s#%d never paused; authoritative gate cleared on approval", env.RepoAlpha, num)
}

// TestReviewAuthorityYoloDoesNotBypassBlock covers scenario 3 — the core
// composition guarantee from ADR-1250: fabrik:yolo does not bypass an
// authoritative gate. The item stays blocked while CHANGES_REQUESTED stands,
// and only clears once approved. Descoped from "merges under yolo" to "gate
// clears" for the same reason as TestReviewAuthorityClearsOnApproval.
//
// Requires #1261 (engine support for the review-authority:authoritative
// label) to be merged.
//
// Requires FABRIK_REVIEW_WAIT_TIMEOUT set comfortably above ~2 minutes: the
// 90s "block persists" window below runs shortly after fabrik:awaiting-review
// is first observed, and a too-short timeout risks the review-wait timeout
// pause (checkAwaitingReviewTimeout, a legitimate outcome unrelated to yolo)
// firing inside that window. The loop distinguishes the two cases explicitly
// rather than misreporting a timeout-pause as a yolo bypass, but a timeout
// pause still fails the test (it can't proceed to the approval half of the
// scenario), so the timeout value still needs to be sized accordingly.
//
// Wall-clock: ~5-10 min.
func TestReviewAuthorityYoloDoesNotBypassBlock(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required for deterministic verdict scenarios")
	}

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "yolo-no-bypass", "review-authority:authoritative", "fabrik:yolo")

	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)
	if engineLogin, reviewerLogin := TokenLogin(t, env.GHToken), TokenLogin(t, reviewerToken); engineLogin == reviewerLogin {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the engine/PR author — "+
			"set FABRIK_REVIEWER_TOKEN to a distinct GitHub account's PAT", reviewerLogin)
	}

	SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNum, "REQUEST_CHANGES")
	t.Logf("submitted REQUEST_CHANGES review on %s PR #%d (fabrik:yolo present)", env.RepoAlpha, prNum)

	// The gate must block even though yolo is set — a bounded withheld window
	// is sufficient here (unlike the first test, we don't need to wait out
	// the full timeout; we only need to confirm yolo doesn't short-circuit
	// the block).
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	blockedDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(blockedDeadline) {
		if state, err := tryIssueState(env, env.RepoAlpha, num); err == nil && state == "CLOSED" {
			t.Fatalf("yolo bypassed the authoritative gate — %s#%d closed while CHANGES_REQUESTED still stood", env.RepoAlpha, num)
		}
		if labels, err := tryIssueLabels(env, env.RepoAlpha, num); err == nil && !slices.Contains(labels, "fabrik:awaiting-review") {
			if slices.Contains(labels, "fabrik:paused") {
				t.Fatalf("fabrik:awaiting-review cleared via a review-wait-timeout pause during this test's 90s block-persist "+
					"window on %s#%d — this is a legitimate outcome unrelated to yolo bypass, not evidence of one; "+
					"FABRIK_REVIEW_WAIT_TIMEOUT is set too short for this test (needs to be comfortably above ~2 minutes, see README)",
					env.RepoAlpha, num)
			}
			t.Fatalf("yolo bypassed the authoritative gate — fabrik:awaiting-review cleared from %s#%d while CHANGES_REQUESTED still stood", env.RepoAlpha, num)
		}
		pollSleep(pollBase())
	}
	t.Logf("yolo did not bypass the block: %s#%d still OPEN with fabrik:awaiting-review after CHANGES_REQUESTED", env.RepoAlpha, num)

	SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNum, "APPROVE")
	t.Logf("submitted APPROVE review on %s PR #%d", env.RepoAlpha, prNum)

	WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	t.Logf("R3 verified: %s#%d — yolo composed correctly: blocked on CHANGES_REQUESTED, cleared on APPROVE", env.RepoAlpha, num)
}

// TestReviewAuthorityAdvisoryRegressionGuard covers scenario 4: the
// regression guard proving the additive authoritative check inside
// checkReviewGate does not leak into advisory (default) mode. Runs against
// the existing "Review" column/stage with no authority label at all
// (untouched default config), so it ships green independent of #1261.
//
// This test would fail today if the authoritative check were made
// non-additive (e.g. if it replaced rather than extended the
// len(outstanding)==0 && hasReviews condition): a CHANGES_REQUESTED review
// on an advisory-mode stage must still clear the gate, exactly as it did
// before ADR-1250.
//
// Wall-clock: ~2-5 min.
func TestReviewAuthorityAdvisoryRegressionGuard(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required for deterministic verdict scenarios")
	}

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "advisory-regression")

	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)
	reviewerLogin := TokenLogin(t, reviewerToken)
	if engineLogin := TokenLogin(t, env.GHToken); engineLogin == reviewerLogin {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the engine/PR author — "+
			"set FABRIK_REVIEWER_TOKEN to a distinct GitHub account's PAT", reviewerLogin)
	}

	// Request the reviewer so outstanding is genuinely non-empty by
	// construction, before confirming the gate engages. Without this, the
	// bed's real reviewer (Pruefer, as of #1396) can land its own incidental
	// review before the engine's first gate evaluation and before this test's
	// own REQUEST_CHANGES — that alone satisfies checkReviewGate's outer
	// len(outstanding)==0 && hasReviews clearing branch, which never applies
	// fabrik:awaiting-review. The wait below would then time out against
	// genuinely correct engine behavior (#1312) instead of proving the gate
	// engaged before the deliberate verdict.
	RequestPRReviewer(t, env, env.RepoAlpha, prNum, reviewerLogin)
	t.Logf("requested reviewer %q on %s PR #%d — outstanding is non-empty by construction, gate engagement is deterministic", reviewerLogin, env.RepoAlpha, prNum)

	// Confirm the gate genuinely engages (no review submitted yet, so
	// checkReviewGate's outer hasReviews==false condition blocks and applies
	// the label unconditionally, regardless of authority mode) before
	// submitting the review. Without this, a poll racing ahead of
	// SubmitPRReview below could observe the item only after the review
	// already exists, clearing on the very first evaluation and never
	// applying fabrik:awaiting-review at all — the later WaitForLabelAbsent
	// would then pass trivially without ever exercising the clearing
	// transition this regression guard exists to check.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-review")
	t.Logf("fabrik:awaiting-review confirmed on %s#%d before review submission — gate is genuinely engaged", env.RepoAlpha, num)

	SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNum, "REQUEST_CHANGES")
	t.Logf("submitted REQUEST_CHANGES review on %s PR #%d (advisory Review stage)", env.RepoAlpha, prNum)

	// Advisory mode: any submitted review (regardless of verdict) with no
	// outstanding requested reviewers clears the gate — this is the
	// pre-#1250 behavior and must be unchanged.
	WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	t.Logf("R4 verified: %s#%d — advisory gate cleared on CHANGES_REQUESTED (additive check did not narrow the default path)", env.RepoAlpha, num)
}
