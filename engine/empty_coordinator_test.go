package engine

import (
	"os/exec"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// fakeOriginMain points refs/remotes/origin/<branch> at dir's current HEAD,
// simulating a fetched remote-tracking ref without a real "origin" remote —
// mirrors the pattern used in context_test.go's writeCodebaseChanges tests.
func fakeOriginRef(t *testing.T, dir, branch string) {
	t.Helper()
	sha, err := gitRevParse(dir, "HEAD")
	if err != nil {
		t.Fatalf("gitRevParse HEAD: %v", err)
	}
	cmd := exec.Command("git", "update-ref", "refs/remotes/origin/"+branch, sha)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref origin/%s: %s: %v", branch, out, err)
	}
}

// TestCommitsAheadOfBase covers commitsAheadOfBase directly against a real git
// worktree: zero commits ahead, one commit ahead, and an unresolvable base ref.
func TestCommitsAheadOfBase(t *testing.T) {
	skipIfNoGit(t)
	dir := initBareRepo(t)
	fakeOriginRef(t, dir, "main")

	ahead, err := commitsAheadOfBase(dir, "main")
	if err != nil {
		t.Fatalf("commitsAheadOfBase: %v", err)
	}
	if ahead != 0 {
		t.Errorf("ahead = %d, want 0", ahead)
	}

	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "extra work")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s: %v", out, err)
	}

	ahead, err = commitsAheadOfBase(dir, "main")
	if err != nil {
		t.Fatalf("commitsAheadOfBase: %v", err)
	}
	if ahead != 1 {
		t.Errorf("ahead = %d, want 1", ahead)
	}

	if _, err := commitsAheadOfBase(dir, "does-not-exist"); err == nil {
		t.Error("expected error for unresolvable base branch, got nil")
	}
}

// coordinatorStages returns an Implement stage (CreateDraftPR: true) followed by
// a cleanup Done stage, for exercising the empty-coordinator completion path.
func coordinatorStages() []*stages.Stage {
	return []*stages.Stage{
		{
			Name:          "Implement",
			Order:         1,
			Prompt:        "implement it",
			CreateDraftPR: true,
			Completion:    stages.CompletionCriteria{Type: "claude"},
		},
		{
			Name:            "Done",
			Order:           99,
			CleanupWorktree: true,
		},
	}
}

// testEngineWithRepoAndStages builds a real-git-backed engine (for worktree/commit
// checks) with a custom stage list and a statusField populated for those stages —
// combining the testEngineWithRepo and testEngineWithStages patterns.
func testEngineWithRepoAndStages(t *testing.T, client *mockGitHubClient, claude *mockClaudeInvoker, stgs []*stages.Stage) (*Engine, string) {
	t.Helper()
	repoDir := initBareRepo(t)
	fakeOriginRef(t, repoDir, "main")
	wm := NewWorktreeManager(repoDir)
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        stgs,
		},
		client,
		claude,
		wm,
	)
	opts := make(map[string]string)
	for _, s := range stgs {
		opts[s.Name] = "OPT_" + s.Name
	}
	eng.statusField = &gh.StatusField{FieldID: "FIELD_1", Options: opts}
	return eng, repoDir
}

// TestFinalizeStageOutcome_EmptyCoordinator_AdvancesToDone verifies that a
// fully-delegated coordinator parent (fabrik:children-spawned, zero commits ahead
// of base after Implement completes) advances straight to Done — no CreateDraftPR
// attempt, no fabrik:paused — instead of hitting the "No commits between" 422.
func TestFinalizeStageOutcome_EmptyCoordinator_AdvancesToDone(t *testing.T) {
	skipIfNoGit(t)

	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Pure coordinator: Claude completes but makes no commits of its own.
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, coordinatorStages())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 80, Title: "Coordinator", Status: "Implement", ItemID: "PVTI_80",
		Labels: []string{"fabrik:children-spawned"},
	}

	if err := eng.processItem(t.Context(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if len(client.createDraftPRCalls) != 0 {
		t.Errorf("expected no CreateDraftPR call for empty coordinator, got %v", client.createDraftPRCalls)
	}

	var sawComplete, sawPaused bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Implement:complete" {
			sawComplete = true
		}
		if c.labelName == "fabrik:paused" {
			sawPaused = true
		}
	}
	if !sawComplete {
		t.Error("expected stage:Implement:complete label to be added")
	}
	if sawPaused {
		t.Error("expected no fabrik:paused label for empty coordinator")
	}

	if len(client.updateStatusCalls) != 1 || client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("expected a single status update to OPT_Done, got %v", client.updateStatusCalls)
	}

	if len(client.closeIssueCalls) != 1 || client.closeIssueCalls[0].issueNumber != 80 {
		t.Errorf("expected issue #80 to be closed, got %v", client.closeIssueCalls)
	}
}

// TestFinalizeStageOutcome_CoordinatorWithOwnCommits_CreatesPR verifies that a
// parent carrying fabrik:children-spawned but WITH its own commits still goes
// through the normal draft-PR creation path — the guard must key off the actual
// commit count, not just the label.
func TestFinalizeStageOutcome_CoordinatorWithOwnCommits_CreatesPR(t *testing.T) {
	skipIfNoGit(t)

	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })
	origPR := ensureDraftPRRetryDelay
	ensureDraftPRRetryDelay = 0
	t.Cleanup(func() { ensureDraftPRRetryDelay = origPR })

	const createdPRNum = 555
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil // no existing PR yet
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			return createdPRNum, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			// Simulate the just-created PR being linked, so verifyAndHealLinkage
			// short-circuits instead of attempting a body-heal.
			item.LinkedPRNumber = createdPRNum
			return nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Hybrid parent: own implementation work, committed during this run.
			cmd := exec.Command("git", "commit", "--allow-empty", "-m", "own work")
			cmd.Dir = workDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git commit in mock failed: %s: %v", out, err)
			}
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, coordinatorStages())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 81, Title: "Hybrid coordinator", Status: "Implement", ItemID: "PVTI_81",
		Labels: []string{"fabrik:children-spawned"},
	}

	if err := eng.processItem(t.Context(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if len(client.createDraftPRCalls) != 1 {
		t.Fatalf("expected 1 CreateDraftPR call for hybrid parent with own commits, got %v", client.createDraftPRCalls)
	}

	var sawComplete bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Implement:complete" {
			sawComplete = true
		}
	}
	if !sawComplete {
		t.Error("expected stage:Implement:complete label to be added")
	}

	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no Done-path issue close for a normal PR-creating stage completion, got %v", client.closeIssueCalls)
	}
}

// TestRunStage_R5_EmptyCoordinatorSelfHeals verifies the R5 retry-without-Claude
// precheck: an issue already carrying PRCreationFailed for Implement (from a
// pre-fix retry cycle), with fabrik:children-spawned and zero commits ahead of
// base, self-heals to Done on its next poll instead of hammering ensureDraftPR.
func TestRunStage_R5_EmptyCoordinatorSelfHeals(t *testing.T) {
	skipIfNoGit(t)

	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })
	origPR := ensureDraftPRRetryDelay
	ensureDraftPRRetryDelay = 0
	t.Cleanup(func() { ensureDraftPRRetryDelay = origPR })

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			t.Error("Claude should not be re-invoked on the R5 self-heal path")
			return "", false, TokenUsage{}, nil
		},
	}
	eng, repoDir := testEngineWithRepoAndStages(t, client, claude, coordinatorStages())

	// Pre-create the worktree/branch (as a prior Implement attempt would have)
	// with zero commits ahead of base, and record PRCreationFailed for Implement —
	// the state a pre-fix issue would already be stuck in.
	wm := NewWorktreeManager(repoDir)
	if _, err := wm.EnsureWorktree(82, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	eng.store.Apply(itemstate.PRCreationFailedRecorded{Repo: "owner/repo", Number: 82, StageName: "Implement"})

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 82, Title: "Coordinator", Status: "Implement", ItemID: "PVTI_82",
		Labels: []string{"fabrik:children-spawned"},
	}

	if err := eng.processItem(t.Context(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if len(client.createDraftPRCalls) != 0 {
		t.Errorf("expected no CreateDraftPR call — self-heal must route to Done instead, got %v", client.createDraftPRCalls)
	}
	if len(client.updateStatusCalls) != 1 || client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("expected a single status update to OPT_Done, got %v", client.updateStatusCalls)
	}
	if len(client.closeIssueCalls) != 1 || client.closeIssueCalls[0].issueNumber != 82 {
		t.Errorf("expected issue #82 to be closed, got %v", client.closeIssueCalls)
	}
}
