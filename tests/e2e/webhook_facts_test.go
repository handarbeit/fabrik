//go:build e2e

package e2e

import (
	"fmt"
	"time"

	"testing"
)

// Live webhook coverage: the facts the engine uses (#2427, FR-005–FR-007; ADR 2352,
// ADR 1995, ADR 2351). Each test holds the bed's polls so only a delivery can move
// the daemon's copy, proves the delivery was applied with the intake counters in
// fabrik_health, and reads the effect through fabrik_status.

// seedInertPRItem seeds a paused item with a draft PR at Review (a draft is never
// reviewed by the bed's bot, and a paused item runs no stage) and closes both in
// t.Cleanup. It returns the issue and PR numbers.
func seedInertPRItem(t *testing.T, env *Env, marker string) (issue, pr int) {
	t.Helper()
	issue, pr, _ = seedReviewGateItemDraft(t, env, env.RepoAlpha, "main", "Review", marker, "fabrik:paused", "fabrik:awaiting-input")
	t.Cleanup(func() {
		_, _ = ghOutput(env, "pr", "close", fmt.Sprint(pr), "-R", env.RepoAlpha)
		CloseIssue(env, env.RepoAlpha, issue)
	})
	return issue, pr
}

// reviewerOutstanding returns the daemon's count of outstanding findings from the
// reviewer on repo#issue, and whether the daemon knows it.
func reviewerOutstanding(env *Env, repo string, issue int, reviewer string) (int, bool) {
	st, err := bedStatus(env, repo, issue)
	if err != nil {
		return 0, false
	}
	for _, r := range st.PR.Reviewers {
		if r.Reviewer == reviewer && r.Outstanding.Valid {
			return r.Outstanding.V, true
		}
	}
	return 0, false
}

// TestWebhookReviewThreadFacts (FR-005): a review-thread resolution and a review
// comment edit, each by webhook, reach the daemon with polls held. The thread
// resolution lowers the reviewer's outstanding findings in the feedback view
// (fabrik_status pr.reviewers); the comment edit is proven by the
// pull_request_review_comment apply counter (the edited text is on no API surface).
func TestWebhookReviewThreadFacts(t *testing.T) {
	env := LoadEnv(t)
	webhookLegSetup(t, env)
	repo := env.RepoAlpha
	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		Inconclusive(t, "the bed has no FABRIK_REVIEWER_TOKEN, so there is no second identity to author a review")
	}
	reviewer := TokenLogin(t, reviewerToken)
	issue, pr := seedInertPRItem(t, env, "webhook-review-thread-"+time.Now().UTC().Format("150405.000"))

	// A review with one unresolved inline thread, free-running so the daemon hydrates it.
	files := FetchPRFilePath(t, env, repo, pr)
	ref := SubmitPRReviewWithThread(t, env, reviewerToken, repo, pr, files, "e2e webhook finding: please resolve this thread")
	if !awaitCond(5*time.Minute, func() bool {
		n, ok := reviewerOutstanding(env, repo, issue, reviewer)
		return ok && n >= 1
	}) {
		Inconclusive(t, "the daemon never showed an outstanding finding from %s on %s#%d within 5m, so there is no thread to resolve by webhook", reviewer, repo, issue)
	}

	HoldPolls(t)
	base := mustReadIntake(t, env)
	ResolveReviewThread(t, env, ref.ThreadID)
	if _, ok := awaitApplied(env, base, "pull_request_review_thread", webhookApplyWait); !ok {
		webhookEffectMissing(t, env, "the pull_request_review_thread delivery was not applied within %v while polls were held", webhookApplyWait)
	}
	if !awaitCond(webhookApplyWait, func() bool {
		n, ok := reviewerOutstanding(env, repo, issue, reviewer)
		return ok && n == 0
	}) {
		n, _ := reviewerOutstanding(env, repo, issue, reviewer)
		t.Fatalf("after the thread was resolved the daemon still counts %d outstanding finding(s) from %s on %s#%d (polls held): the resolution did not reach the feedback view", n, reviewer, repo, issue)
	}

	base = mustReadIntake(t, env)
	EditReviewComment(t, repo, reviewerToken, ref.CommentID, "e2e webhook finding (edited): resolved thread")
	if _, ok := awaitApplied(env, base, "pull_request_review_comment", webhookApplyWait); !ok {
		webhookEffectMissing(t, env, "the pull_request_review_comment edit delivery was not applied within %v while polls were held", webhookApplyWait)
	}
}

// TestWebhookCheckRunCompletion (FR-006): a check run's completion reaches the
// daemon by check_run delivery with polls held (the apply counter), and the next
// poll turns the PR's CI verdict from pending to green. The test supplies its own
// controllable check (a check run it creates and completes as the App), so it does
// not depend on production CI timing.
func TestWebhookCheckRunCompletion(t *testing.T) {
	env := LoadEnv(t)
	webhookLegSetup(t, env)
	repo := env.RepoAlpha
	leg := requireAppLeg(t, env)
	issue, pr := seedInertPRItem(t, env, "webhook-check-run-"+time.Now().UTC().Format("150405.000"))
	sha, err := prHeadSHA(env, repo, pr)
	if err != nil {
		t.Fatal(err)
	}
	id, err := createCheckRun(leg.Token, repo, sha, "e2e-webhook-ci", "in_progress", "")
	if err != nil {
		Inconclusive(t, "could not create a controllable check run as the App (needs checks:write): %v", err)
	}
	ciVerdict := func() string {
		st, err := bedStatus(env, repo, issue)
		if err != nil || !st.PR.CI.Valid {
			return ""
		}
		return st.PR.CI.V
	}
	if !awaitCond(4*time.Minute, func() bool { return ciVerdict() == "pending" }) {
		Inconclusive(t, "the daemon never showed CI pending for %s#%d with the controllable check in progress (verdict %q), so there is no pending gate to clear", repo, issue, ciVerdict())
	}

	HoldPolls(t)
	base := mustReadIntake(t, env)
	if err := completeCheckRun(leg.Token, repo, id, "success"); err != nil {
		t.Fatalf("completing the controllable check run: %v", err)
	}
	if _, ok := awaitApplied(env, base, "check_run", webhookApplyWait); !ok {
		webhookEffectMissing(t, env, "the check_run completion delivery was not applied within %v while polls were held", webhookApplyWait)
	}
	// fabrik_status's pr.ci is the verdict the engine records when it evaluates CI
	// (LinkedPRState.CIVerdict), not one recomputed from the cached runs, so with
	// polls held it cannot move. The delivery is proven by the apply counter above;
	// one triggered poll then evaluates CI from the runs the delivery updated (the
	// documented proxy, ADR 2427-a decision 4, as FR-003 and FR-007 do).
	TriggerPoll(t)
	if !awaitCond(webhookApplyWait, func() bool { return ciVerdict() == "green" }) {
		t.Fatalf("after the check run completed successfully and one poll ran, the daemon's CI verdict for %s#%d is %q, want green: the check_run delivery did not clear the gate", repo, issue, ciVerdict())
	}
}

// TestWebhookDependencyRemovalWakes (FR-007): removing a blockedBy edge on GitHub
// is delivered as issue_dependencies, which wakes the blocked item. With polls
// held the delivery's apply is proven by the issue_dependencies counter; the one
// poll that follows must then admit the item and clear fabrik:blocked, which it
// cannot do from the dep-blocked cooldown alone — the delivery drops it. The item
// runs its Specify stage once it is unblocked, which costs one short real run.
func TestWebhookDependencyRemovalWakes(t *testing.T) {
	env := LoadEnv(t)
	webhookLegSetup(t, env)
	repo := env.RepoAlpha
	stamp := time.Now().UTC().Format("20060102-150405")
	blocker := FileIssue(t, env, repo, "e2e webhook dependency blocker "+stamp,
		"e2e scenario for #2427 FR-007: the open blocker. Not on the board; nothing to implement.")
	t.Cleanup(func() { CloseIssue(env, repo, blocker) })
	blocked := FileIssue(t, env, repo, "e2e webhook dependency blocked "+stamp,
		"e2e scenario for #2427 FR-007: blocked until its blocker's edge is removed. Nothing to specify beyond acknowledging this; complete the stage immediately.")
	t.Cleanup(func() { CloseIssue(env, repo, blocked) })
	AddBlockedBy(t, env, repo, blocked, repo, blocker)
	itemID := AddIssueToProject(t, env, repo, blocked)
	SetIssueStatus(t, env, itemID, "Specify")
	AwaitStatusVisible(t, env, repo, blocked, "Specify", awaitSeedTimeout)
	if !awaitDaemonLabel(env, repo, blocked, "fabrik:blocked", true, 5*time.Minute) {
		Inconclusive(t, "the engine never marked %s#%d fabrik:blocked within 5m, so there is no blocked item to wake", repo, blocked)
	}

	HoldPolls(t)
	base := mustReadIntake(t, env)
	RemoveBlockedBy(t, env, repo, blocked, repo, blocker)
	if _, ok := awaitApplied(env, base, "issue_dependencies", webhookApplyWait); !ok {
		webhookEffectMissing(t, env, "the issue_dependencies delivery was not applied within %v while polls were held (GitHub sends no other delivery for an edge removal)", webhookApplyWait)
	}
	TriggerPoll(t)
	if !awaitDaemonLabel(env, repo, blocked, "fabrik:blocked", false, 3*time.Minute) {
		t.Fatalf("one poll after the delivered edge removal the daemon still holds fabrik:blocked on %s#%d: the dependency delivery did not wake the item", repo, blocked)
	}
}
