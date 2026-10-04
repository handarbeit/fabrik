package engine

import (
	"context"
	"fmt"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// singletonReuseFixture builds a landSingleton call whose trial branch already carries the
// draft CI PR assembleAndValidateInner opened for it — the real-CI shape of the
// one-at-a-time fallback. existing is the PR ListPRs reports on that branch.
func singletonReuseFixture(t *testing.T, trialName string, existing gh.PRDetails) (*Engine, *mockGitHubClient, *mergeTrainWorkerState, trialParams, trainMember) {
	t.Helper()
	m := makeQueuedMember(9, 90, "Issue Nine")
	existing.HeadRefName = "fabrik/merge-train/" + trialName
	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) {
			return []gh.PRDetails{
				// Another trial's PR on a different branch must never be picked up.
				{Number: 700, HeadRefName: "fabrik/merge-train/other-trial", State: "closed", Merged: true},
				existing,
			}, nil
		},
		// GitHub allows one open PR per head branch: a second one is a 422.
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			if head == existing.HeadRefName {
				return 0, fmt.Errorf("GitHub API returned 422: A pull request already exists for %s", head)
			}
			return 999, nil
		},
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) { return nil, nil },
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{projectID: "PVT_test"}
	p := trialParams{
		owner: "owner", repo: "repo", baseBranch: "main",
		trainKey: defaultTrainKey("owner", "repo"),
		wm:       wm, holdingStg: holdingStage(eng.cfg),
	}
	return eng, client, state, p, m
}

// A green singleton trial lands through its own draft CI PR: marked ready and merged, with no
// second CreatePR on the same branch (which GitHub rejects with a 422 — the concept-maps
// incident where every green one-at-a-time trial was discarded).
func TestLandSingleton_ReusesTrialDraftPR(t *testing.T) {
	eng, client, state, p, m := singletonReuseFixture(t, "merge-train-main-1-t3",
		gh.PRDetails{Number: 1602, State: "open", Draft: true})

	eng.landSingleton(context.Background(), state, p, m, "merge-train-main-1-t3")

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.createPRCalls) != 0 {
		t.Errorf("CreatePR must not be called when the trial's draft CI PR exists, got %d call(s)", len(client.createPRCalls))
	}
	if len(client.markPRReadyCalls) != 1 || client.markPRReadyCalls[0].prNumber != 1602 {
		t.Errorf("expected the draft CI PR #1602 to be marked ready once, got %+v", client.markPRReadyCalls)
	}
	if len(client.mergePRCalls) != 1 || client.mergePRCalls[0].prNumber != 1602 {
		t.Fatalf("expected PR #1602 to be merged, got %+v", client.mergePRCalls)
	}
	if len(client.updateStatusCalls) == 0 {
		t.Error("expected the member to be advanced to Done after the merge")
	}
}

// An already-merged trial PR (restart mid-landing) completes the landing without merging again.
func TestLandSingleton_TrialPRAlreadyMerged_CompletesLanding(t *testing.T) {
	eng, client, state, p, m := singletonReuseFixture(t, "merge-train-main-1-t4",
		gh.PRDetails{Number: 1603, State: "closed", Merged: true})

	eng.landSingleton(context.Background(), state, p, m, "merge-train-main-1-t4")

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.createPRCalls) != 0 || len(client.mergePRCalls) != 0 {
		t.Errorf("expected no CreatePR/MergePR for an already-merged trial PR, got %d/%d", len(client.createPRCalls), len(client.mergePRCalls))
	}
	if len(client.updateStatusCalls) == 0 {
		t.Error("expected the member to be advanced to Done")
	}
}

// A closed-unmerged trial PR is a failed trial: nothing is merged and the member stays Queued.
func TestLandSingleton_TrialPRClosedUnmerged_LeavesQueued(t *testing.T) {
	eng, client, state, p, m := singletonReuseFixture(t, "merge-train-main-1-t5",
		gh.PRDetails{Number: 1604, State: "closed"})

	eng.landSingleton(context.Background(), state, p, m, "merge-train-main-1-t5")

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.createPRCalls) != 0 || len(client.mergePRCalls) != 0 || len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no PR creation, merge or status change, got create=%d merge=%d status=%d",
			len(client.createPRCalls), len(client.mergePRCalls), len(client.updateStatusCalls))
	}
}
