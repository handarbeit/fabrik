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
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate"} // WaitForCI is nil
	settle := PRSettleResult{Status: PRMergeNoPR}

	blocked, conflict := eng.checkMergeabilityGate(item, stage, settle)
	if blocked || conflict {
		t.Errorf("expected clear when wait_for_ci not set, got blocked=%v conflict=%v", blocked, conflict)
	}
	if len(client.addLabelCalls) > 0 || len(client.removeLabelCalls) > 0 {
		t.Error("should not modify any labels when wait_for_ci is not set")
	}
}

func TestCheckMergeabilityGate_NoPR_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{Status: PRMergeNoPR}

	blocked, conflict := eng.checkMergeabilityGate(item, stage, settle)
	if blocked || conflict {
		t.Errorf("expected clear when no PR found, got blocked=%v conflict=%v", blocked, conflict)
	}
}

func TestCheckMergeabilityGate_Mergeable_ClearsGate_RemovesStaleLabel(t *testing.T) {
	tr := true
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:rebase-needed"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{Status: PRMergeReady, PR: &gh.PRDetails{Number: 42}}

	blocked, conflict := eng.checkMergeabilityGate(item, stage, settle)
	if blocked || conflict {
		t.Errorf("expected clear when PRMergeReady, got blocked=%v conflict=%v", blocked, conflict)
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
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	// mergeable=null surfaces as PRMergeUnsettled from the primitive.
	settle := PRSettleResult{Status: PRMergeUnsettled, Reason: "mergeable=null (GitHub computing)"}

	blocked, conflict := eng.checkMergeabilityGate(item, stage, settle)
	if !blocked || conflict {
		t.Errorf("expected blocked=true conflict=false for Unsettled, got blocked=%v conflict=%v", blocked, conflict)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("should not add fabrik:rebase-needed when settle is Unsettled")
		}
	}
}

func TestCheckMergeabilityGate_FalseMergeable_AppliesLabelAndSignalsConflict(t *testing.T) {
	tr := true
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{Status: PRMergeConflicting, Reason: "mergeable=false", PR: &gh.PRDetails{Number: 42}}

	blocked, conflict := eng.checkMergeabilityGate(item, stage, settle)
	if !blocked || !conflict {
		t.Errorf("expected blocked=true conflict=true for Conflicting, got blocked=%v conflict=%v", blocked, conflict)
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
	tr := true
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	// Label already present — should not be re-added.
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:rebase-needed"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{Status: PRMergeConflicting, PR: &gh.PRDetails{Number: 42}}

	_, conflict := eng.checkMergeabilityGate(item, stage, settle)
	if !conflict {
		t.Error("expected conflict=true")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("should not re-add fabrik:rebase-needed when already present")
		}
	}
}

func TestCheckMergeabilityGate_CIFailed_ClearsGate(t *testing.T) {
	// PRMergeBlocked (CI failed) must NOT block the merge gate — it must clear so
	// checkCIGate can classify the failure and dispatch the CI-fix reinvoke. If the
	// merge gate returned (true, false) here, poll.go would skip the CI gate and the
	// issue would get stuck with no CI-fix dispatch.
	tr := true
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{
		Status: PRMergeBlocked,
		Reason: "CI checks failed",
		PR:     &gh.PRDetails{Number: 42},
		CheckRuns: []gh.CheckRun{
			{Name: "test", Status: "completed", Conclusion: "failure"},
		},
	}

	blocked, conflict := eng.checkMergeabilityGate(item, stage, settle)
	if blocked || conflict {
		t.Errorf("expected clear when PRMergeBlocked (CI failed), got blocked=%v conflict=%v", blocked, conflict)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("should not add fabrik:rebase-needed on CI failure (no base conflict)")
		}
	}
}

func TestCheckMergeabilityGate_CIFailed_RemovesStaleRebaseLabel(t *testing.T) {
	// When CI has failed (PRMergeBlocked) but a stale fabrik:rebase-needed label
	// exists from a prior rebase cycle that was resolved, the label should be cleared.
	tr := true
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:rebase-needed"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{Status: PRMergeBlocked, PR: &gh.PRDetails{Number: 42}}

	blocked, conflict := eng.checkMergeabilityGate(item, stage, settle)
	if blocked || conflict {
		t.Errorf("expected clear for PRMergeBlocked, got blocked=%v conflict=%v", blocked, conflict)
	}
	found := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			found = true
		}
	}
	if !found {
		t.Error("expected stale fabrik:rebase-needed label to be removed when PRMergeBlocked (CI failure, no conflict)")
	}
}

func TestCheckMergeabilityGate_FetchPRError_BlocksForRetry(t *testing.T) {
	tr := true
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	// Transient API errors in settlePRMergeState surface as PRMergeUnsettled.
	settle := PRSettleResult{Status: PRMergeUnsettled, Reason: "FetchLinkedPR error: simulated transient error"}

	blocked, conflict := eng.checkMergeabilityGate(item, stage, settle)
	if !blocked || conflict {
		t.Errorf("expected blocked=true conflict=false on Unsettled (transient error), got blocked=%v conflict=%v", blocked, conflict)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("should not add fabrik:rebase-needed on transient API error (no confirmed conflict)")
		}
	}
}

func TestCheckMergeabilityGate_FetchMergeableError_BlocksForRetry(t *testing.T) {
	tr := true
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	// FetchPRMergeableFields error surfaces as PRMergeUnsettled from the primitive.
	settle := PRSettleResult{Status: PRMergeUnsettled, Reason: "FetchPRMergeableFields error: simulated transient error"}

	blocked, conflict := eng.checkMergeabilityGate(item, stage, settle)
	if !blocked || conflict {
		t.Errorf("expected blocked=true conflict=false on Unsettled (transient error), got blocked=%v conflict=%v", blocked, conflict)
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
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{Status: PRMergeTerminal, PR: &gh.PRDetails{Number: 10}}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle)

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
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled", "fabrik:yolo"}}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{Status: PRMergeReady, PR: &gh.PRDetails{Number: 10}}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{}, item, stage, settle)

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

// TestCheckAutoMergeConvergence_IsInMergeQueue_NoPause verifies that when the PR is
// actively in the merge queue (IsInMergeQueue=true in settle), checkAutoMergeConvergence
// does not trigger the "user disabled auto-merge" pause even though AutoMergeEnabled
// is false (EnqueuePullRequest does not set auto_merge on the PR).
func TestCheckAutoMergeConvergence_IsInMergeQueue_NoPause(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			// AutoMergeEnabled=false because EnqueuePullRequest was used (not EnablePullRequestAutoMerge).
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: false}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled", "fabrik:yolo"}}
	stage := &stages.Stage{Name: "Validate"}
	// settle.PR.IsInMergeQueue=true indicates the PR is waiting in the merge queue.
	settle := PRSettleResult{Status: PRMergeReady, PR: &gh.PRDetails{Number: 10, IsInMergeQueue: true}}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{}, item, stage, settle)

	// The pause path must NOT fire.
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" || c.labelName == "fabrik:awaiting-input" {
			t.Errorf("expected no pause when IsInMergeQueue=true, but label %q was added", c.labelName)
		}
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no comment when IsInMergeQueue=true, got %d comment(s)", len(client.addCommentCalls))
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			t.Error("expected fabrik:auto-merge-enabled NOT to be removed when PR is in merge queue")
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
	eng := testEngineForMerge(t, client)
	eng.cfg.ConvergenceBudget = 30 * time.Minute
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{Status: PRMergeBlocked, PR: &gh.PRDetails{Number: 10, MergeableState: "blocked"}}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{}, item, stage, settle)

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
	eng := testEngineForMerge(t, client)
	eng.cfg.ConvergenceBudget = 0 // disabled
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	// PRMergeBlocked (CI failure, no conflict): falls through to "waiting for GitHub" log.
	settle := PRSettleResult{Status: PRMergeBlocked, PR: &gh.PRDetails{Number: 10, MergeableState: "blocked"}}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{}, item, stage, settle)

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
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	// PRMergeUnsettled drives the "wait" branch, replacing pr.MergeableState=="unknown".
	settle := PRSettleResult{Status: PRMergeUnsettled, Reason: "mergeable_state=unknown"}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{}, item, stage, settle)

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
	eng := testEngineForMerge(t, client)
	eng.cfg.MaxRebaseCycles = 3 // allow dispatch (default testEngine sets 0, which would immediately pause)
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	// PRMergeConflicting drives the rebase reinvoke path, replacing pr.MergeableState=="dirty".
	settle := PRSettleResult{Status: PRMergeConflicting, PR: &gh.PRDetails{Number: 10}}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle)

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
	eng := testEngineForMerge(t, client)
	// Simulate in-flight worker.
	eng.store.Apply(itemstate.LocalLockAcquired{
		Repo: "owner/repo", Number: 42, User: "testuser", AcquiredAt: time.Now(),
		Worker: &itemstate.WorkerHandle{StageName: "Validate", StartedAt: time.Now()},
	})
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{Status: PRMergeConflicting, PR: &gh.PRDetails{Number: 10}}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle)

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
// a second rebase reinvoke is dispatched. MaxRebaseCycles is set to 3 so the
// second cycle (count=1) is below the limit and dispatch proceeds.
func TestCheckAutoMergeConvergence_SecondConflict_DispatchesAgain(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "dirty"}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.MaxRebaseCycles = 3 // allow second dispatch (count=1 < limit=3)
	// Simulate prior rebase cycle having completed (no in-flight worker).
	eng.store.Apply(itemstate.RebaseCycleIncremented{Repo: "owner/repo", Number: 42, StageName: "Validate"})

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{Status: PRMergeConflicting, PR: &gh.PRDetails{Number: 10}}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle)

	snap, err := eng.store.Get("owner/repo", 42)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	// Cycle count should now be 2; dispatch fires because count(1) < MaxRebaseCycles(3).
	if snap.RebaseCycles("Validate") != 2 {
		t.Errorf("expected RebaseCycles=2 after second dispatch, got %d", snap.RebaseCycles("Validate"))
	}
}

// TestPauseForConvergenceFailed_PostsComment_AppliesLabels verifies that the
// convergence-failed pause comment contains the expected fields and that
// fabrik:paused, fabrik:awaiting-input are applied and fabrik:auto-merge-enabled removed.
// PR diagnostic state (mergeableState, headSHA) now comes from settle.PR rather
// than a separate FetchLinkedPR call.
func TestPauseForConvergenceFailed_PostsComment_AppliesLabels(t *testing.T) {
	client := &mockGitHubClient{
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) {
			return 3, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.ConvergenceBudget = 30 * time.Minute
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	elapsed := 45 * time.Minute
	settle := PRSettleResult{
		Status: PRMergeConflicting,
		PR:     &gh.PRDetails{Number: 10, State: "open", MergeableState: "dirty", HeadSHA: "abc123"},
	}

	eng.pauseForConvergenceFailed(context.Background(), &gh.ProjectBoard{}, item, stage, settle, elapsed)

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

// TestCheckAutoMergeConvergence_ConflictAtCycleLimit_PausesInsteadOfDispatch
// verifies that when RebaseCycles reaches MaxRebaseCycles in the convergence path,
// pauseForRebaseCycleLimit is called instead of dispatching a new rebase reinvoke.
// This closes the previously unbounded livelock on unresolvable conflicts.
func TestCheckAutoMergeConvergence_ConflictAtCycleLimit_PausesInsteadOfDispatch(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "dirty"}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.MaxRebaseCycles = 2
	// Pre-fill RebaseCycles to the limit.
	for i := 0; i < eng.cfg.MaxRebaseCycles; i++ {
		eng.store.Apply(itemstate.RebaseCycleIncremented{Repo: "owner/repo", Number: 42, StageName: "Validate"})
	}

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{Status: PRMergeConflicting, PR: &gh.PRDetails{Number: 10}}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle)

	eng.wg.Wait()
	foundPaused := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			foundPaused = true
		}
	}
	if !foundPaused {
		t.Error("expected fabrik:paused when convergence rebase cycle limit reached")
	}
	if len(client.addCommentCalls) == 0 {
		t.Error("expected a cycle-limit comment to be posted")
	}
	// Verify cycle count was not incremented beyond the limit.
	snap, err := eng.store.Get("owner/repo", 42)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if snap.RebaseCycles("Validate") != 2 {
		t.Errorf("expected RebaseCycles=2 (unchanged at limit), got %d", snap.RebaseCycles("Validate"))
	}
}

// TestCheckAutoMergeConvergence_BudgetZero_ConflictAtCycleLimit_Pauses verifies
// that MaxRebaseCycles bounds rebase reinvokes even when FABRIK_CONVERGENCE_BUDGET=0
// (budget disabled). This resolves the unbounded rebase-loop livelock flagged for
// the BUDGET=0 + unresolvable-conflict case.
func TestCheckAutoMergeConvergence_BudgetZero_ConflictAtCycleLimit_Pauses(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "dirty"}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.ConvergenceBudget = 0 // disabled — no time-based pause
	eng.cfg.MaxRebaseCycles = 1
	// Pre-fill RebaseCycles to the limit.
	eng.store.Apply(itemstate.RebaseCycleIncremented{Repo: "owner/repo", Number: 42, StageName: "Validate"})

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{Status: PRMergeConflicting, PR: &gh.PRDetails{Number: 10}}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle)

	eng.wg.Wait()
	foundPaused := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			foundPaused = true
		}
	}
	if !foundPaused {
		t.Error("expected fabrik:paused when convergence rebase cycle limit reached (BUDGET=0 case)")
	}
}

// TestCheckAutoMergeConvergence_UnregisteredRepo_NoPanic is a regression test for
// the case where Phase 1 of the catch-up loop hits a fabrik:auto-merge-enabled
// item before processItem has had a chance to register the WorktreeManager for
// item.Repo. Before the ensureRepoReady guard at the top of
// checkAutoMergeConvergence, pauseForConvergenceFailed → worktreesFor would
// panic. With the guard, the issue is paused via the standard clone-failure
// path instead.
func TestCheckAutoMergeConvergence_UnregisteredRepo_NoPanic(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "blocked"}, nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return time.Now().Add(-2 * time.Hour), nil // budget exhausted
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.ConvergenceBudget = 30 * time.Minute
	// ENAMETOOLONG: fails os.MkdirAll before any network call — deterministic, no git subprocess.
	eng.fabrikDir = strings.Repeat("a", 10000)

	// item.Repo points at a repo with no registered WorktreeManager. The pre-fix
	// behavior would be: convergence budget exhausted → pauseForConvergenceFailed
	// → worktreesFor("fail-test/unregistered") → panic.
	item := gh.ProjectItem{
		Number: 42,
		Repo:   "fail-test/unregistered",
		Labels: []string{"fabrik:auto-merge-enabled"},
	}
	stage := &stages.Stage{Name: "Validate"}

	// Should not panic. Should pause via the clone-failure path.
	// settle is not reached (ensureRepoReady fails first); pass zero value.
	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, PRSettleResult{})

	// Verify the clone-failure path ran: a comment was posted and fabrik:paused applied.
	if len(client.addCommentCalls) == 0 {
		t.Error("expected clone-failure comment to be posted on unregistered repo")
	}
	var pausedAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused label to be added when clone fails")
	}
}
