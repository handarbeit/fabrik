//go:build e2e

package e2e

import (
	"fmt"
	"time"

	"testing"
)

// PAT self-recognition counterparts (#1975, ADR-1975).
//
// The three TestAppSelfRecognition* scenarios (#1877) run only on the App leg and
// guard the engine's "<slug>[bot]" identity. Under a PAT the engine is the
// harness account itself — a User-typed login, "arbeithand" on the bed — so the
// same comparisons (engine.selfLogin() = cfg.User) are made against a very
// different author shape. These cases run only on a PAT leg (requirePATLeg skips
// otherwise) and close the asymmetry: self-recognition was previously proven for
// one auth mode only.
//
// What each can and cannot prove, honestly. Under a PAT the harness account is
// both the engine and the "human" of every other scenario, which constrains the
// counterparts:
//
//   - A2 and A3 share their bodies with the App cases (blockedCommentUpdatedInPlace,
//     durableReviewSuppression) and DO discriminate selfLogin(): neutralising the
//     engine's self-login comparison makes the blocked comment stop being edited in
//     place (stale body) and the self-authored review-ids-addressed marker stop
//     suppressing its review. The control arm of A3 comes from the distinct
//     FABRIK_REVIEWER_TOKEN account, the only second identity a PAT bed has.
//   - A1 has no pure PAT analogue: filterHuman contains no cfg.User comparison, so
//     a marker-free comment from the PAT account reads as human in BOTH modes (it
//     is what resumes a pause in every other scenario). The case below guards the
//     other half of the engine's own-comment recognition — the "🏭 **Fabrik"
//     prefix exclusion in findNewComments — under the User-typed identity. It does
//     not discriminate selfLogin().
//
// Non-vacuity of all three against a neutralised self-login check can only be
// shown on a live PAT leg and is recorded by the operator who runs the gate.

// TestPATSelfRecognitionOwnMarkedCommentNeverResumes (A1, PAT form): a comment
// authored by the PAT account that carries Fabrik's own output prefix never
// resumes a paused item — and a marker-free comment from the same account does
// (positive control).
//
// The engine logs nothing when every new comment is excluded by the prefix rule
// (the raw set is empty), so unlike the App A1 there is no "evaluated, non-human"
// line to wait for first. The control is what proves the item is being evaluated:
// after the hold, the plain comment must resume it, and the resume line must name
// that comment's poll, not an earlier one.
//
// Cost: one comment-processing Claude invocation (the control). Wall-clock: ~6-10 min.
func TestPATSelfRecognitionOwnMarkedCommentNeverResumes(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	leg := patSelfRecognitionLeg(t, env)
	repo := env.RepoAlpha

	ensurePausedLabelExists(t, env, repo)
	ensureEngineLabelExists(t, env, repo, awaitingInputLabel)

	off := LogOffset(t, env)
	stamp := time.Now().UTC().Format("150405.000")
	// Filed already paused, so no stage (and no Claude) runs before the control.
	num := FileIssue(t, env, repo, fmt.Sprintf("e2e pat self-recognition: own marked comment never resumes (%s)", stamp),
		"e2e scenario for #1975 (A1, PAT form). Filed paused; a comment carrying Fabrik's own prefix must not resume it.", pausedLabel, awaitingInputLabel)
	itemID := AddIssueToProject(t, env, repo, num)
	SetIssueStatus(t, env, itemID, "Specify")

	marked := leg.PostSelf(t, repo, num,
		"🏭 **Fabrik — e2e self-recognition probe (#1975)**\n\nA comment carrying Fabrik's own output prefix, authored by the PAT identity. It must never resume the pause.")
	t.Logf("posted prefixed comment %d on %s#%d as %s", marked.ID, repo, num, marked.Login)

	// Hold: three poll intervals in which nothing may change.
	hold := 3*bedPollInterval() + 30*time.Second
	t.Logf("holding %s (3 polls) asserting the pause persists", hold)
	holdFor(hold, bedPollInterval()/2, func() {
		labels, err := restIssueLabels(env, repo, num)
		if err != nil {
			t.Logf("transient label read error on %s#%d: %v", repo, num, err)
		} else if !containsString(labels, pausedLabel) || !containsString(labels, awaitingInputLabel) {
			t.Fatalf("%s#%d lost its pause labels after a Fabrik-prefixed comment from the PAT account (labels: %v) — the engine read its own output as human input",
				repo, num, labels)
		}
		if l := anyLogLineWhere(t, env, fmt.Sprintf("[#%d ", num), func(l string) bool { return resumeLine(l, num) }, off); l != "" {
			t.Fatalf("engine resumed #%d on its own Fabrik-prefixed comment: %s", num, l)
		}
		comments, err := listComments(env, repo, num)
		if err != nil {
			t.Logf("transient comment read error on %s#%d: %v", repo, num, err)
			return
		}
		for _, c := range comments {
			if c.ID == marked.ID && (c.Eyes > 0 || c.Rocket > 0) {
				t.Fatalf("prefixed comment %d on %s#%d has reactions (👀=%d 🚀=%d): the engine processed it as input", c.ID, repo, num, c.Eyes, c.Rocket)
			}
		}
	})
	t.Logf("A1 (pat) negative arm held: %s#%d stayed paused, no resume line, no reactions on the prefixed comment", repo, num)

	// Positive control: a marker-free comment from the same account is human input.
	CommentOnIssue(t, env, repo, num, "e2e self-recognition control (#1975): a marker-free comment from "+leg.Login+" must resume the pause.")
	WaitForLabelAbsent(t, env, repo, num, pausedLabel, 15*time.Minute)
	resume := waitForLogLineWhere(t, env, fmt.Sprintf("[#%d ", num),
		func(l string) bool { return resumeLine(l, num) }, off, 2*time.Minute,
		fmt.Sprintf("the resume line for #%d after the marker-free comment", num))
	t.Logf("A1 (pat) positive control: marker-free comment from %s resumed %s#%d: %s", leg.Login, repo, num, resume)
}

// TestPATSelfRecognitionBlockedCommentUpdatedInPlace (A2, PAT form): the engine
// edits its own blocked comment in place when the dependency set changes, and the
// comment's author is the User-typed PAT account. Shares its body with the App case.
//
// No Claude cost. Wall-clock: ~12-25 min.
func TestPATSelfRecognitionBlockedCommentUpdatedInPlace(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	blockedCommentUpdatedInPlace(t, env, patSelfRecognitionLeg(t, env))
}

// TestPATSelfRecognitionDurableReviewSuppression (A3, PAT form): a
// review-ids-addressed marker authored by the PAT account (the engine's own
// identity) suppresses redelivery of its review; the same marker from the
// distinct reviewer account does not. Shares its body with the App case.
//
// Cost: one review-reinvoke Claude invocation (the control arm). Wall-clock: ~8-15 min.
func TestPATSelfRecognitionDurableReviewSuppression(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	durableReviewSuppression(t, env, patSelfRecognitionLeg(t, env))
}
