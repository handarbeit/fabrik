package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

func TestCheckMergeabilityGate_WaitForCIFalse_ClearsImmediately(t *testing.T) {
	called := false
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			called = true
			return nil, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate"} // WaitForCI is nil

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if blocked || conflict {
		t.Errorf("expected clear when wait_for_ci not set, got blocked=%v conflict=%v", blocked, conflict)
	}
	if called {
		t.Error("should not call FetchLinkedPR when wait_for_ci is not set")
	}
}

func TestCheckMergeabilityGate_NoPR_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if blocked || conflict {
		t.Errorf("expected clear when no PR found, got blocked=%v conflict=%v", blocked, conflict)
	}
}

func TestCheckMergeabilityGate_Mergeable_ClearsGate_RemovesStaleLabel(t *testing.T) {
	tr := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha"}, nil
		},
		fetchPRMergeableFn: func(owner, repo string, prNumber int) (*bool, error) {
			return &tr, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:rebase-needed"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if blocked || conflict {
		t.Errorf("expected clear when mergeable=true, got blocked=%v conflict=%v", blocked, conflict)
	}
	found := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			found = true
		}
	}
	if !found {
		t.Error("expected stale fabrik:rebase-needed label to be removed when PR becomes mergeable")
	}
}

func TestCheckMergeabilityGate_NilMergeable_BlockedNoConflict(t *testing.T) {
	tr := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha"}, nil
		},
		fetchPRMergeableFn: func(owner, repo string, prNumber int) (*bool, error) {
			return nil, nil // GitHub hasn't computed yet
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if !blocked || conflict {
		t.Errorf("expected blocked=true conflict=false for mergeable=null, got blocked=%v conflict=%v", blocked, conflict)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("should not add fabrik:rebase-needed when mergeable is unknown")
		}
	}
}

func TestCheckMergeabilityGate_FalseMergeable_AppliesLabelAndSignalsConflict(t *testing.T) {
	fa := false
	tr := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha"}, nil
		},
		fetchPRMergeableFn: func(owner, repo string, prNumber int) (*bool, error) {
			return &fa, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if !blocked || !conflict {
		t.Errorf("expected blocked=true conflict=true for mergeable=false, got blocked=%v conflict=%v", blocked, conflict)
	}
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			found = true
		}
	}
	if !found {
		t.Error("expected fabrik:rebase-needed to be added on confirmed conflict")
	}
}

func TestCheckMergeabilityGate_FalseMergeable_LabelIdempotent(t *testing.T) {
	fa := false
	tr := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha"}, nil
		},
		fetchPRMergeableFn: func(owner, repo string, prNumber int) (*bool, error) {
			return &fa, nil
		},
	}
	eng := testEngineForMerge(client)
	// Label already present — should not be re-added.
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:rebase-needed"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	_, conflict := eng.checkMergeabilityGate(item, stage)
	if !conflict {
		t.Error("expected conflict=true")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("should not re-add fabrik:rebase-needed when already present")
		}
	}
}

func TestCheckMergeabilityGate_FetchPRError_BlocksForRetry(t *testing.T) {
	tr := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, errStubTransient
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if !blocked || conflict {
		t.Errorf("expected blocked=true conflict=false on transient FetchLinkedPR error, got blocked=%v conflict=%v", blocked, conflict)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("should not add fabrik:rebase-needed on transient API error (no confirmed conflict)")
		}
	}
}

func TestCheckMergeabilityGate_FetchMergeableError_BlocksForRetry(t *testing.T) {
	tr := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha"}, nil
		},
		fetchPRMergeableFn: func(owner, repo string, prNumber int) (*bool, error) {
			return nil, errStubTransient
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if !blocked || conflict {
		t.Errorf("expected blocked=true conflict=false on transient mergeable-fetch error, got blocked=%v conflict=%v", blocked, conflict)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("should not add fabrik:rebase-needed on transient API error (no confirmed conflict)")
		}
	}
}

// errStubTransient is a sentinel used in merge-gate tests to simulate a
// transient GitHub API error.
var errStubTransient = &stubError{"simulated transient error"}

type stubError struct{ msg string }

func (e *stubError) Error() string { return e.msg }

// ── checkAutoMergeConvergence tests ──────────────────────────────────────────

func TestCheckAutoMergeConvergence_PRMerged_RemovesLabelAndAdvances(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "closed", Merged: true, AutoMergeEnabled: true}, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage)

	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:auto-merge-enabled to be removed when PR merges")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("should not pause when PR merges normally")
		}
	}
	// Verify Done advancement was attempted: advanceToNextStage calls UpdateProjectItemStatus.
	client.mu.Lock()
	advanceCalls := len(client.updateStatusCalls)
	client.mu.Unlock()
	if advanceCalls == 0 {
		t.Error("expected UpdateProjectItemStatus to be called to advance to Done when PR merges")
	}
}

func TestCheckAutoMergeConvergence_UserDisabledAutoMerge_PostsCommentRemovesLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			// AutoMergeEnabled=false simulates user clicking "Disable auto-merge".
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: false}, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled", "fabrik:yolo"}}
	stage := &stages.Stage{Name: "Validate"}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{}, item, stage)

	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:auto-merge-enabled to be removed when user disables auto-merge")
	}
	if len(client.addCommentCalls) == 0 {
		t.Error("expected a comment to be posted when user disables auto-merge")
	}
	for _, c := range client.addCommentCalls {
		if !strings.Contains(c.body, "🏭 **Fabrik") {
			t.Errorf("comment body should start with 🏭 **Fabrik, got: %q", c.body)
		}
	}
	// fabrik:paused + fabrik:awaiting-input must be applied to prevent Phase 2 from
	// re-enabling auto-merge on the next poll cycle.
	wantAdded := map[string]bool{"fabrik:paused": false, "fabrik:awaiting-input": false}
	for _, c := range client.addLabelCalls {
		wantAdded[c.labelName] = true
	}
	for label, found := range wantAdded {
		if !found {
			t.Errorf("expected label %q to be added when user disables auto-merge", label)
		}
	}
}

func TestCheckAutoMergeConvergence_BudgetExhausted_PausesIssue(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "blocked"}, nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			// Report label applied 2 hours ago (exceeds any budget in test).
			return time.Now().Add(-2 * time.Hour), nil
		},
	}
	eng := testEngineForMerge(client)
	eng.cfg.ConvergenceBudget = 30 * time.Minute
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{}, item, stage)

	foundPaused := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			foundPaused = true
		}
	}
	if !foundPaused {
		t.Error("expected fabrik:paused to be applied when convergence budget exhausts")
	}

	foundRemoveAutoMerge := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundRemoveAutoMerge = true
		}
	}
	if !foundRemoveAutoMerge {
		t.Error("expected fabrik:auto-merge-enabled to be removed on budget exhaustion")
	}

	if len(client.addCommentCalls) == 0 {
		t.Error("expected convergence-failed pause comment to be posted")
	}
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "convergence budget exhausted") {
			return // found it
		}
	}
	t.Error("pause comment should mention 'convergence budget exhausted'")
}

func TestCheckAutoMergeConvergence_BudgetDisabled_NoPause(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "blocked"}, nil
		},
	}
	eng := testEngineForMerge(client)
	eng.cfg.ConvergenceBudget = 0 // disabled
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{}, item, stage)

	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("should not pause when convergence budget is disabled")
		}
	}
}

func TestCheckAutoMergeConvergence_UnknownMergeability_Waits(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "unknown"}, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{}, item, stage)

	// No labels changed, no comments posted — just waiting.
	for _, c := range client.addLabelCalls {
		t.Errorf("unexpected AddLabel call for %q when mergeability is unknown", c.labelName)
	}
	for _, c := range client.removeLabelCalls {
		t.Errorf("unexpected RemoveLabel call for %q when mergeability is unknown", c.labelName)
	}
}

func TestCheckAutoMergeConvergence_DirtyConflict_IncrementsCycleCount(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "dirty"}, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage)

	snap, err := eng.store.Get("owner/repo", 42)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if snap.RebaseCycles("Validate") != 1 {
		t.Errorf("expected RebaseCycles=1 after dirty dispatch, got %d", snap.RebaseCycles("Validate"))
	}
	// Should not pause — only dispatch.
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("should not pause on first conflict within budget")
		}
	}
}

// TestCheckAutoMergeConvergence_DirtyConflict_InFlight_SkipsDispatch verifies
// that a second rebase dispatch is skipped when one is already in-flight
// (existing worker set in store).
func TestCheckAutoMergeConvergence_DirtyConflict_InFlight_SkipsDispatch(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "dirty"}, nil
		},
	}
	eng := testEngineForMerge(client)
	// Simulate in-flight worker.
	eng.store.Apply(itemstate.LocalLockAcquired{
		Repo: "owner/repo", Number: 42, User: "testuser", AcquiredAt: time.Now(),
		Worker: &itemstate.WorkerHandle{StageName: "Validate", StartedAt: time.Now()},
	})
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage)

	snap, err := eng.store.Get("owner/repo", 42)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	// Cycle count should NOT be incremented when dispatch is skipped.
	if snap.RebaseCycles("Validate") != 0 {
		t.Errorf("expected RebaseCycles=0 when dispatch skipped (in-flight), got %d", snap.RebaseCycles("Validate"))
	}
}

// TestCheckAutoMergeConvergence_SecondConflict_DispatchesAgain verifies SC-008:
// after a first rebase reinvoke, if main moves again causing another conflict,
// a second rebase reinvoke is dispatched. Cycle count is not a gate.
func TestCheckAutoMergeConvergence_SecondConflict_DispatchesAgain(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "dirty"}, nil
		},
	}
	eng := testEngineForMerge(client)
	// Simulate prior rebase cycle having completed (no in-flight worker).
	eng.store.Apply(itemstate.RebaseCycleIncremented{Repo: "owner/repo", Number: 42, StageName: "Validate"})

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage)

	snap, err := eng.store.Get("owner/repo", 42)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	// Cycle count should now be 2; dispatch was not gated by MaxRebaseCycles.
	if snap.RebaseCycles("Validate") != 2 {
		t.Errorf("expected RebaseCycles=2 after second dispatch, got %d", snap.RebaseCycles("Validate"))
	}
}

// TestPauseForConvergenceFailed_PostsComment_AppliesLabels verifies that the
// convergence-failed pause comment contains the expected fields and that
// fabrik:paused, fabrik:awaiting-input are applied and fabrik:auto-merge-enabled removed.
func TestPauseForConvergenceFailed_PostsComment_AppliesLabels(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", MergeableState: "dirty", HeadSHA: "abc123"}, nil
		},
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) {
			return 3, nil
		},
	}
	eng := testEngineForMerge(client)
	eng.cfg.ConvergenceBudget = 30 * time.Minute
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	elapsed := 45 * time.Minute

	eng.pauseForConvergenceFailed(context.Background(), &gh.ProjectBoard{}, item, stage, elapsed)

	// Verify pause comment posted.
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected a comment to be posted")
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "convergence budget exhausted") {
		t.Errorf("comment should mention 'convergence budget exhausted', got: %q", body)
	}
	if !strings.Contains(body, "dirty") {
		t.Errorf("comment should include mergeable_state 'dirty', got: %q", body)
	}

	// Verify labels.
	wantAdded := map[string]bool{"fabrik:paused": false, "fabrik:awaiting-input": false}
	for _, c := range client.addLabelCalls {
		wantAdded[c.labelName] = true
	}
	for label, found := range wantAdded {
		if !found {
			t.Errorf("expected label %q to be added", label)
		}
	}
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:auto-merge-enabled to be removed on convergence failure")
	}
}
