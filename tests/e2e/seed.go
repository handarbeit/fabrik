//go:build e2e

package e2e

import (
	"testing"

	"github.com/handarbeit/fabrik/tests/e2e/seedspec"
)

// seedAtStage seeds the state a live test is about directly through the GitHub API —
// no Claude call, no pipeline traversal — and returns the issue number, the member PR
// number (0 when the column implies no PR yet) and the board item ID (#1992,
// ADR-1992).
//
// What to create is decided by seedspec.Build, a pure function whose output is
// checked, in plain `go test ./...`, against the state the real engine produces at
// that column (seedspec.CheckFidelity against the sim-recorded fixtures): a seed may
// omit labels a traversal would carry but never invents one. This function only
// executes the plan:
//
//  1. file the issue (with spec.ExtraLabels) and add it to the project, with no Status;
//  2. for a column that implies a PR (Implement onward), open the member PR on
//     fabrik/issue-<N> with a "Closes #N" body, and wait until the engine can resolve it;
//  3. post any prior-stage comments (default none);
//  4. apply the stage:<S>:complete labels;
//  5. place the item at the column — unless spec.DeferStatus, which leaves the card
//     without a Status so the caller decides when the engine may first see it.
//
// Every wait goes through the awaitVisible family (#1974), so a seed is never reported
// ready before GitHub reflects it on the read path the engine uses, and this function
// keeps no wait loop of its own (inconclusive/guards_test.go enforces both).
//
// The seedLandingCandidate and seedReviewGateItem* helpers are thin wrappers over it.
func seedAtStage(t *testing.T, env *Env, repo string, spec seedspec.Spec) (issueNum, prNum int, itemID string) {
	t.Helper()
	if err := spec.Validate(); err != nil {
		t.Fatalf("seedAtStage: %v", err)
	}
	issueNum = FileIssue(t, env, repo, spec.Title, spec.IssueBody, spec.ExtraLabels...)
	itemID = AddIssueToProject(t, env, repo, issueNum)
	plan, err := seedspec.Build(spec, issueNum)
	if err != nil {
		t.Fatalf("seedAtStage: %v", err)
	}

	if plan.CreatePR {
		prNum = createMemberPRBody(t, env, repo, plan.BaseBranch, plan.Branch, plan.Path, plan.Content,
			spec.Title, plan.PRBody, issueNum, plan.PRDraft)
		// createMemberPR's own wait, for the same reason: exposing the item before GitHub
		// reports the Closes linkage lets the review gate's broken-linkage check pause
		// it (#1962).
		if plan.BaseBranch == seedspec.DefaultBase {
			AwaitClosingLinkage(t, env, repo, issueNum, prNum, awaitSeedTimeout)
		}
		// Confirm the PR is resolvable by the fabrik/issue-<N> branch convention
		// (mirrors the engine's resolver) before seeding the completion labels.
		AwaitPRForBranchVisible(t, env, repo, issueNum, awaitSeedTimeout)
	}
	postSeedComments(t, env, repo, issueNum, plan.Comments)
	addSeedLabels(t, env, repo, issueNum, plan.StageLabels)
	if !plan.DeferStatus {
		SetIssueStatus(t, env, itemID, plan.Status)
	}
	AwaitBoardItemVisible(t, env, repo, issueNum, awaitSeedTimeout)
	t.Logf("seeded %s: issue #%d, PR #%d, labels %v, Status=%q (deferred=%v, draft=%v, path %s)",
		plan.Column, issueNum, prNum, plan.StageLabels, plan.Status, plan.DeferStatus, plan.PRDraft, plan.Path)
	return issueNum, prNum, itemID
}

// postSeedComments posts the plan's prior-stage comments (none by default). It is a
// plain iteration over a list, kept out of seedAtStage so that function stays free of
// loops of any kind (the seed-path guard in tests/e2e/inconclusive).
func postSeedComments(t *testing.T, env *Env, repo string, issueNum int, bodies []string) {
	t.Helper()
	for _, body := range bodies {
		CommentOnIssue(t, env, repo, issueNum, body)
	}
}

// addSeedLabels applies each label and waits for it to be visible on the engine's
// read path (awaitVisible, #1974) before the next, so none is reported ready early.
func addSeedLabels(t *testing.T, env *Env, repo string, issueNum int, labels []string) {
	t.Helper()
	for _, label := range labels {
		AddLabel(t, env, repo, issueNum, label)
		AwaitLabelVisible(t, env, repo, issueNum, label, awaitSeedTimeout)
	}
}
