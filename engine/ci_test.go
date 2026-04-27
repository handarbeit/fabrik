package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// ── checkCIGate ──────────────────────────────────────────────────────────────

func TestCheckCIGate_WaitForCIFalse_ClearsImmediately(t *testing.T) {
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

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected all false when wait_for_ci not set, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	if called {
		t.Error("should not call FetchLinkedPR when wait_for_ci is not set")
	}
}

func TestCheckCIGate_NoPR_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected clear when no PR, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
}

func TestCheckCIGate_NoCheckRuns_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha1"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected clear for no check runs (R5), got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
}

func TestCheckCIGate_AllGreen_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha2"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "success"},
				{Name: "test", Status: "completed", Conclusion: "success"},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected clear for all-green CI, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
}

func TestCheckCIGate_Pending_BlocksNoLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha3"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "ci", Status: "in_progress"},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if !blocked {
		t.Error("expected blocked=true for pending CI")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false for pending, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	// checkCIGate must not add fabrik:awaiting-ci when CI is only pending;
	// it was already applied by handleStageComplete when the stage completed.
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("checkCIGate must NOT add fabrik:awaiting-ci when CI is only pending")
		}
	}
	// stage:X:complete must not be added while CI is pending
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when CI is pending")
		}
	}
}

func TestCheckCIGate_Failed_BlocksAndAddsLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha4"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "lint", Status: "completed", Conclusion: "failure"},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if !blocked || !ciFailure {
		t.Errorf("expected blocked=true ciFailure=true for failed CI, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
	if timedOut {
		t.Error("expected timedOut=false for failed CI without timeout")
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

func TestCheckCIGate_Failed_AlreadyLabeledWithTimeout_TimesOut(t *testing.T) {
	appliedAt := time.Now().Add(-2 * time.Hour) // well past any timeout
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha5"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "lint", Status: "completed", Conclusion: "failure"},
			}, nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return appliedAt, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)
	eng.cfg.CIWaitTimeout = 1 * time.Millisecond // tiny timeout

	tr := true
	// Item already has fabrik:awaiting-ci
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure {
		t.Errorf("expected blocked=false ciFailure=false on timeout, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
	if !timedOut {
		t.Error("expected timedOut=true when timeout elapses")
	}
	// fabrik:awaiting-ci should be removed
	found := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			found = true
		}
	}
	if !found {
		t.Error("expected fabrik:awaiting-ci to be removed on timeout")
	}
}

func TestCheckCIGate_Failed_AlreadyLabeledNotYetTimedOut_Blocked(t *testing.T) {
	appliedAt := time.Now().Add(-1 * time.Minute) // within a 30-min window
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha6"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "lint", Status: "completed", Conclusion: "failure"},
			}, nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return appliedAt, nil
		},
	}
	eng := testEngineForMerge(client) // CIWaitTimeout = 0 → defaults to 30 min

	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if !blocked || !ciFailure {
		t.Errorf("expected blocked=true ciFailure=true when not yet timed out, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
	if timedOut {
		t.Error("expected timedOut=false when timeout has not elapsed")
	}
}

// ── checkCIGate adds stage:X:complete on gate clear ──────────────────────────

// TestCheckCIGate_AllGreen_AddsCompleteLabel verifies that checkCIGate adds
// stage:X:complete when all CI checks pass (R5 — gate cleared).
func TestCheckCIGate_AllGreen_AddsCompleteLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha10"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "success"},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected gate cleared, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	foundComplete := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Error("expected stage:Validate:complete to be added when all CI checks pass")
	}
	// fabrik:awaiting-ci should also be removed
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed when gate clears")
	}
}

// TestCheckCIGate_NoCheckRuns_AddsCompleteLabel verifies that checkCIGate adds
// stage:X:complete when no check runs exist (no CI configured).
func TestCheckCIGate_NoCheckRuns_AddsCompleteLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha11"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil // no check runs (R5)
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, _, _ := eng.checkCIGate(nil, item, stage)
	if blocked {
		t.Error("expected gate cleared for no check runs (R5)")
	}
	foundComplete := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Error("expected stage:Validate:complete to be added when no CI is configured (R5)")
	}
}

// TestCheckCIGate_NoPR_AddsCompleteLabel verifies that checkCIGate adds
// stage:X:complete when there is no linked PR (gate clears — no PR, no CI).
// Regression test: before the fix, fabrik:awaiting-ci was never removed and
// stage:X:complete was never added when FetchLinkedPR returns nil.
func TestCheckCIGate_NoPR_AddsCompleteLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil // no linked PR
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected gate cleared for no PR, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	foundComplete := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Error("expected stage:Validate:complete to be added when no linked PR (R5 equivalent)")
	}
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed when gate clears (no linked PR)")
	}
}

// TestCheckCIGate_Failed_DoesNotAddCompleteLabel verifies that checkCIGate does
// NOT add stage:X:complete when CI checks have failed.
func TestCheckCIGate_Failed_DoesNotAddCompleteLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha12"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "lint", Status: "completed", Conclusion: "failure"},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	_, ciFailure, _ := eng.checkCIGate(nil, item, stage)
	if !ciFailure {
		t.Error("expected ciFailure=true for failed CI")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when CI failed")
		}
	}
}

// TestCheckCIGate_NonValidateStage_AddsCorrectCompleteLabel verifies that
// checkCIGate uses the correct stage name when adding the completion label
// (not hard-coded to "Validate").
func TestCheckCIGate_NonValidateStage_AddsCorrectCompleteLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha13"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "success"},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	// Use a non-Validate stage name
	stage := &stages.Stage{Name: "Review", WaitForCI: &tr}

	blocked, _, _ := eng.checkCIGate(nil, item, stage)
	if blocked {
		t.Error("expected gate cleared")
	}
	foundComplete := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Review:complete" {
			foundComplete = true
		}
		if c.labelName == "stage:Validate:complete" {
			t.Error("wrong completion label added — should be stage:Review:complete")
		}
	}
	if !foundComplete {
		t.Errorf("expected stage:Review:complete, got add calls: %v", func() []string {
			var names []string
			for _, c := range client.addLabelCalls {
				names = append(names, c.labelName)
			}
			return names
		}())
	}
}

// ── buildCIFixComment ─────────────────────────────────────────────────────────

func TestBuildCIFixComment_IncludesFailedChecks(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha7"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "failure"},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1}
	tr := true
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	comment := eng.buildCIFixComment(item, stage, "/tmp")
	if comment.DatabaseID != 0 {
		t.Error("synthetic comment should have DatabaseID=0")
	}
	if !strings.Contains(comment.Body, "build") {
		t.Error("expected failed check name 'build' in comment body")
	}
	if !strings.Contains(comment.Body, "CI Fix Required") {
		t.Error("expected CI Fix Required header in comment body")
	}
}

// TestCheckCIGate_FetchLinkedPRError_BlocksGate verifies that a transient
// FetchLinkedPR API error returns blocked=true rather than clearing the gate,
// preventing auto-advance when CI status is unknown.
func TestCheckCIGate_FetchLinkedPRError_BlocksGate(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, fmt.Errorf("transient network error")
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if !blocked {
		t.Error("expected blocked=true when FetchLinkedPR returns an error")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false on API error, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
}

// TestCheckCIGate_FetchCheckRunsError_BlocksGate verifies that a transient
// FetchCheckRuns API error returns blocked=true rather than clearing the gate.
func TestCheckCIGate_FetchCheckRunsError_BlocksGate(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha1"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, fmt.Errorf("GitHub API 503")
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if !blocked {
		t.Error("expected blocked=true when FetchCheckRuns returns an error")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false on API error, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
}

func TestBuildCIFixComment_SyntheticHasDatabaseIDZero(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 42}
	stage := &stages.Stage{Name: "Validate"}

	comment := eng.buildCIFixComment(item, stage, "/tmp")
	if comment.DatabaseID != 0 {
		t.Errorf("DatabaseID = %d, want 0 (synthetic)", comment.DatabaseID)
	}
	if comment.Author != "fabrik" {
		t.Errorf("Author = %q, want %q", comment.Author, "fabrik")
	}
}
