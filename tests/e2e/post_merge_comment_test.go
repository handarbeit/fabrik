//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestPostMergeCommentNotApplied is the live-e2e proof of the #1862 post-merge
// guard (ADR-1862, engine/post_merge_comments.go): a human comment that arrives on
// an item whose work has already landed is answered with a "comment not applied"
// reply and is neither processed nor marked 🚀 — Fabrik never commits to or pushes
// a branch whose PR already merged.
//
//   - A6: `🏭 **Fabrik — comment not applied**` is posted on the issue and on the PR
//     (exactly once each, after further polls — the durable marker dedupes), and the
//     triggering comment gets no 🚀 (and no 👀).
//   - A7: no stage work is dispatched for it: the
//     `[#N post-merge] work already landed — not applying 1 comment(s); replying instead`
//     line is present, there is no `[#N comments] processing … new comment(s)` line
//     since the log offset, stage:Implement:in_progress is never applied, and the
//     branch tip SHA is unchanged.
//
// # Deterministic construction
//
// "Merged and still open" is a tiny window in normal flows: GitHub closes the issue
// via `Closes #N`, and at Validate the terminal advance moves the item to Done.
// Reopening a merged issue (as the sim does) is racy live. So:
//
//   - the member PR is opened with a body that has NO closing keyword — the engine
//     resolves the linked PR by the fabrik/issue-<N> branch name, not the body — so an
//     admin merge leaves the issue OPEN;
//   - the item is parked at Implement with stage:Implement:complete. Implement has
//     neither wait_for_ci nor wait_for_reviews and no auto-advance label is present,
//     so no catch-up or terminal-advance path acts on it;
//   - before commenting, the scenario re-reads the issue state, labels, board Status
//     and PR merged state and FAILS LOUDLY if the item was already closed or moved —
//     it must never pass vacuously.
//
// Faking `fabrik:credited-pr:<N>` (the merge-train landed signal) is deliberately not
// done: settleLandingVerification would fail a made-up PR number and reopen the item
// (ADR-1616).
//
// Mode-invariant (Implement is not train-specific) and free of Claude: it runs in
// every leg at essentially no cost. The comment is posted by FABRIK_TOKEN's account,
// a human to Fabrik in both auth legs (see postHumanIssueComment); reaction
// assertions check existence only, never the reacting login.
//
// Wall-clock: ~3–8 min. Cost: none (no Claude invocation).
func TestPostMergeCommentNotApplied(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	repo := env.RepoAlpha
	const base = "main"

	stamp := time.Now().UTC().Format("150405.000")
	title := fmt.Sprintf("e2e post-merge comment (%s)", stamp)
	issue := FileIssue(t, env, repo, title, "e2e post-merge comment guard scenario.")
	itemID := AddIssueToProject(t, env, repo, issue)

	branch := fmt.Sprintf("fabrik/issue-%d", issue)
	path := uniqueMemberPath("e2e/comment-landing/post-merge.md", issue)
	// No closing keyword by design: the issue must stay open after the merge.
	prBody := "e2e post-merge comment guard scenario member PR. Deliberately carries no closing keyword, so merging it leaves the issue open."
	pr := createMemberPRBody(t, env, repo, base, branch, path, "# e2e post-merge comment marker\n", title, prBody, issue, false)
	AwaitPRForBranchVisible(t, env, repo, issue, awaitSeedTimeout)

	AddLabel(t, env, repo, issue, "stage:Implement:complete")
	SetIssueStatus(t, env, itemID, "Implement")
	AwaitStatusVisible(t, env, repo, issue, "Implement", awaitSeedTimeout)
	MergePR(t, env, repo, pr) // --admin: does not wait for slow-gate
	t.Logf("parked #%d at Implement (stage:Implement:complete), merged PR #%d without a closing keyword", issue, pr)

	// Defensive re-read: fail loudly if the item was already disturbed.
	state, err := tryIssueState(env, repo, issue)
	if err != nil || state != "OPEN" {
		t.Fatalf("precondition: %s#%d is %q (err=%v) after the merge, want OPEN — the issue was closed, so the post-merge guard cannot be exercised (not a pass)", repo, issue, state, err)
	}
	if merged, err := restPRField(env, repo, pr, ".merged"); err != nil || merged != "true" {
		t.Fatalf("precondition: PR #%d merged=%q (err=%v), want true", pr, merged, err)
	}
	labels, err := tryIssueLabels(env, repo, issue)
	if err != nil || !slices.Contains(labels, "stage:Implement:complete") || slices.Contains(labels, pausedLabel) {
		t.Fatalf("precondition: %s#%d labels %v (err=%v) — want stage:Implement:complete present and not paused", repo, issue, labels, err)
	}
	if s := strings.TrimSpace(projectStatus(t, env, repo, issue)); s != "Implement" {
		t.Fatalf("precondition: %s#%d board Status is %q, want Implement — the item was moved after the merge", repo, issue, s)
	}
	tipBefore, err := restPRField(env, repo, pr, ".head.sha")
	if err != nil || tipBefore == "" {
		t.Fatalf("read PR #%d head sha: %v", pr, err)
	}

	offset := LogOffset(t, env)
	commentID := postHumanIssueComment(t, env, repo, issue,
		"Late note after the merge: please also tweak the wording. No code change is needed from this run — this is only a check that the comment is answered.")
	t.Logf("posted human comment %d on %s#%d after the merge", commentID, repo, issue)

	// --- A6: the reply appears on the issue and on the PR. ---
	WaitForIssueComment(t, env, repo, issue, postMergeReplyPrefix, 15*time.Minute)
	WaitForPRCommentContaining(t, env, repo, pr, postMergeReplyPrefix, 5*time.Minute)

	// Let a couple more polls pass so a missing dedupe (a reply every poll) shows up.
	time.Sleep(2*pollBase() + 30*time.Second)

	issueBodies, err := tryPRComments(env, repo, issue)
	if err != nil {
		t.Fatalf("read comments on %s#%d: %v", repo, issue, err)
	}
	if n := countMatching(issueBodies, isPostMergeReply); n != 1 {
		t.Fatalf("A6: %d \"comment not applied\" repl(ies) on the issue, want exactly 1 (the durable marker must dedupe)", n)
	}
	prBodies, err := tryPRComments(env, repo, pr)
	if err != nil {
		t.Fatalf("read comments on %s PR #%d: %v", repo, pr, err)
	}
	if n := countMatching(prBodies, isPostMergeReply); n != 1 {
		t.Fatalf("A6: %d \"comment not applied\" repl(ies) on PR #%d, want exactly 1", n, pr)
	}
	eyes, rocket, err := commentReactionTimes(env, repo, commentID)
	if err != nil {
		t.Fatalf("read reactions on comment %d: %v", commentID, err)
	}
	if !rocket.IsZero() || !eyes.IsZero() {
		t.Fatalf("A6: comment %d carries a reaction (👀=%v 🚀=%v) — the post-merge guard must leave it un-reacted so it does not look processed", commentID, eyes, rocket)
	}

	// --- A7: no stage work was dispatched for it. ---
	lines := logLinesSince(t, env, offset)
	if n := countPostMergeGuardReplies(lines, issue); n != 1 {
		t.Fatalf("A7: the post-merge guard's \"not applying N comment(s); replying instead\" line accounts for %d comment(s) on #%d, want 1", n, issue)
	}
	if n := countCommentWorkerLines(lines, issue); n != 0 {
		t.Fatalf("A7: %d \"processing N new comment(s)\" line(s) for #%d since the comment — a comment-review worker was dispatched after the merge", n, issue)
	}
	AssertLabelWasNeverApplied(t, env, repo, issue, "stage:Implement:in_progress")
	tipAfter, err := restPRField(env, repo, pr, ".head.sha")
	if err != nil || tipAfter != tipBefore {
		t.Fatalf("A7: PR #%d head moved (%s -> %s, err=%v) — something pushed to a branch whose PR already merged", pr, tipBefore, tipAfter, err)
	}
	t.Logf("TestPostMergeCommentNotApplied passed: reply posted once on issue and PR, comment left un-reacted, no worker dispatched")
}
