package engine

import (
	"errors"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

// testEngineForMerge returns a minimal engine wired for attemptMergeOnValidate tests.
func testEngineForMerge(client *mockGitHubClient) *Engine {
	stgs := testStagesWithValidate()
	return testEngineWithStages(client, stgs)
}

// TestAttemptMergeOnValidate_NoCheckRuns_MergeProceeds verifies R5: when there are
// no CI check runs at all the gate clears and MergePR is called.
func TestAttemptMergeOnValidate_NoCheckRuns_MergeProceeds(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil // R5: no CI
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	if err := eng.attemptMergeOnValidate(item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(client.mergePRCalls) != 1 {
		t.Fatalf("expected MergePR called once, got %d", len(client.mergePRCalls))
	}
	if client.mergePRCalls[0].prNumber != 10 {
		t.Errorf("MergePR called with pr %d, want 10", client.mergePRCalls[0].prNumber)
	}
}

// TestAttemptMergeOnValidate_AllGreen_MergeProceeds verifies R4: all checks green clears
// the pending timer, removes fabrik:awaiting-ci, and proceeds to merge.
func TestAttemptMergeOnValidate_AllGreen_MergeProceeds(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 11, HeadSHA: "sha2"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "success"},
				{Name: "test", Status: "completed", Conclusion: "success"},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	// Seed a stale pending timer to confirm it gets cleared on green.
	iKey := "owner/repo#1"
	eng.mu.Lock()
	eng.ciMergePendingSince[iKey] = time.Now().Add(-1 * time.Minute)
	eng.mu.Unlock()

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
	if err := eng.attemptMergeOnValidate(item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(client.mergePRCalls) != 1 {
		t.Fatalf("expected MergePR called once, got %d", len(client.mergePRCalls))
	}
	eng.mu.Lock()
	_, still := eng.ciMergePendingSince[iKey]
	eng.mu.Unlock()
	if still {
		t.Error("ciMergePendingSince should be cleared on all-green")
	}
}

// TestAttemptMergeOnValidate_CIFailed_BlocksMerge verifies R3: when any check fails,
// merge is blocked, fabrik:awaiting-ci is added, and error is returned.
func TestAttemptMergeOnValidate_CIFailed_BlocksMerge(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 12, HeadSHA: "sha3"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "success"},
				{Name: "lint", Status: "completed", Conclusion: "failure"},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	err := eng.attemptMergeOnValidate(item)
	if err == nil {
		t.Fatal("expected error when CI failed, got nil")
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR on CI failure, got %d", len(client.mergePRCalls))
	}
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			found = true
		}
	}
	if !found {
		t.Error("expected fabrik:awaiting-ci to be added on CI failure")
	}
}

// TestAttemptMergeOnValidate_CIPending_BlocksMergeNoLabel verifies R2 + R10c: when
// checks are still running, merge is blocked but fabrik:awaiting-ci is NOT applied.
func TestAttemptMergeOnValidate_CIPending_BlocksMergeNoLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 13, HeadSHA: "sha4"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "in_progress", Conclusion: ""},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	err := eng.attemptMergeOnValidate(item)
	if err == nil {
		t.Fatal("expected error when CI pending, got nil")
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR on pending CI, got %d", len(client.mergePRCalls))
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("fabrik:awaiting-ci must NOT be added when CI is only pending (R10c)")
		}
	}
}

// TestAttemptMergeOnValidate_CIPending_TracksTimer verifies R2: on first pending observation
// ciMergePendingSince is populated so the timeout clock starts.
func TestAttemptMergeOnValidate_CIPending_TracksTimer(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 14, HeadSHA: "sha5"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "ci", Status: "queued", Conclusion: ""},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
	iKey := "owner/repo#1"

	eng.mu.Lock()
	_, before := eng.ciMergePendingSince[iKey]
	eng.mu.Unlock()
	if before {
		t.Fatal("expected no pending entry before first call")
	}

	_ = eng.attemptMergeOnValidate(item)

	eng.mu.Lock()
	_, after := eng.ciMergePendingSince[iKey]
	eng.mu.Unlock()
	if !after {
		t.Error("expected ciMergePendingSince to be set after first pending observation")
	}
}

// TestAttemptMergeOnValidate_CIPendingTimeout_PausesIssue verifies R6: when CI has
// been pending longer than CIWaitTimeout the issue is paused with a comment.
func TestAttemptMergeOnValidate_CIPendingTimeout_PausesIssue(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 15, HeadSHA: "sha6"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "slow-ci", Status: "in_progress", Conclusion: ""},
			}, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)
	eng.cfg.CIWaitTimeout = 1 * time.Millisecond // tiny timeout for test

	// Pre-seed as if first observed 1 second ago (well past 1ms timeout).
	iKey := "owner/repo#1"
	eng.mu.Lock()
	eng.ciMergePendingSince[iKey] = time.Now().Add(-1 * time.Second)
	eng.mu.Unlock()

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
	err := eng.attemptMergeOnValidate(item)
	if err == nil {
		t.Fatal("expected error on CI timeout, got nil")
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR on timeout, got %d", len(client.mergePRCalls))
	}
	if len(client.addCommentCalls) == 0 {
		t.Error("expected timeout comment to be posted")
	}
	foundPaused := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			foundPaused = true
		}
	}
	if !foundPaused {
		t.Error("expected fabrik:paused label on CI timeout")
	}
	// ciMergePendingSince entry should be cleared after timeout.
	eng.mu.Lock()
	_, still := eng.ciMergePendingSince[iKey]
	eng.mu.Unlock()
	if still {
		t.Error("ciMergePendingSince should be deleted after timeout fires")
	}
}

// TestAttemptMergeOnValidate_NoPR_ReturnsNil verifies that when FetchLinkedPR finds
// no PR, attemptMergeOnValidate returns nil (no error, no merge).
func TestAttemptMergeOnValidate_NoPR_ReturnsNil(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	if err := eng.attemptMergeOnValidate(item); err != nil {
		t.Fatalf("expected nil when no PR, got %v", err)
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR when no PR, got %d", len(client.mergePRCalls))
	}
}

// TestAttemptMergeOnValidate_ErrNotMergeable_PostsComment verifies that
// ErrNotMergeable results in a comment and fabrik:paused, but no advance.
func TestAttemptMergeOnValidate_ErrNotMergeable_PostsComment(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 20, HeadSHA: "sha7"}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return gh.ErrNotMergeable
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	err := eng.attemptMergeOnValidate(item)
	if err == nil {
		t.Fatal("expected error for ErrNotMergeable")
	}
	if len(client.addCommentCalls) == 0 {
		t.Error("expected unmergeable comment")
	}
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			found = true
		}
	}
	if !found {
		t.Error("expected fabrik:paused on ErrNotMergeable")
	}
}

// TestHandleStageComplete_WaitForCI_SkipsMergeAndReturns verifies Approach A: when
// wait_for_ci is true, handleStageComplete adds the completion label and returns
// without calling attemptMergeOnValidate.
func TestHandleStageComplete_WaitForCI_SkipsMergeAndReturns(t *testing.T) {
	merged := false
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "sha8"}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			merged = true
			return nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	tr := true
	validateStage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}

	eng.handleStageComplete(board, item, validateStage)

	if merged {
		t.Error("MergePR must not be called when wait_for_ci is true (Approach A)")
	}
	// Completion label should still be added.
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			found = true
		}
	}
	if !found {
		t.Error("expected stage:Validate:complete label even with wait_for_ci=true")
	}
}

// TestAttemptMergeOnValidate_FetchLinkedPRError_ReturnsError verifies that a
// transient FetchLinkedPR API error returns an error (retriable) rather than nil
// (which would allow the caller to advance past Validate without merging the PR).
func TestAttemptMergeOnValidate_FetchLinkedPRError_ReturnsError(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, errors.New("network error")
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	err := eng.attemptMergeOnValidate(item)
	if err == nil {
		t.Fatal("expected error when FetchLinkedPR fails, got nil (would incorrectly allow advancement)")
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR on FetchLinkedPR error, got %d", len(client.mergePRCalls))
	}
}

// TestAttemptMergeOnValidate_FetchCheckRunsError_ReturnsError verifies that a
// transient FetchCheckRuns API error returns an error (retriable) rather than
// proceeding to merge with unknown CI status.
func TestAttemptMergeOnValidate_FetchCheckRunsError_ReturnsError(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, errors.New("GitHub API 503")
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	err := eng.attemptMergeOnValidate(item)
	if err == nil {
		t.Fatal("expected error when FetchCheckRuns fails, got nil (would proceed to merge with unknown CI status)")
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR on FetchCheckRuns error, got %d", len(client.mergePRCalls))
	}
}
