package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
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

func TestCheckCIGate_PostPushDelay_BlocksGate(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha-new"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil // no checks yet for the new SHA
		},
	}
	eng := testEngineForMerge(client)
	// Pre-seed: this issue has previously had check runs registered.
	eng.store.Apply(itemstate.PRChecksObserved{Repo: "owner/repo", Number: 1})

	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if !blocked {
		t.Error("expected blocked=true when zero check runs after previously seeing checks (post-push delay)")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false for post-push delay, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("fabrik:awaiting-ci must NOT be removed during post-push registration delay")
		}
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

// TestCheckCIGate_Pending_TimedOut verifies that when CI checks are stuck in
// pending indefinitely, CIWaitTimeout fires (R7 — covers the full CI-await window).
// Under ADR 032, fabrik:awaiting-ci is present from handleStageComplete so the
// timeout tracks the whole pending window, not just confirmed-failure windows.
func TestCheckCIGate_Pending_TimedOut(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha_pending_timeout"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "slow-ci", Status: "in_progress"},
			}, nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			// Simulate fabrik:awaiting-ci applied over 1 hour ago — well past any timeout.
			return time.Now().Add(-2 * time.Hour), nil
		},
	}
	eng := testEngineForMerge(client)
	eng.cfg.CIWaitTimeout = 30 * time.Minute
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if !timedOut {
		t.Error("expected timedOut=true when CI is pending and CIWaitTimeout elapsed")
	}
	if blocked || ciFailure {
		t.Errorf("expected blocked=false ciFailure=false on timeout, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
	// fabrik:awaiting-ci must be removed on timeout
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed on timeout")
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

// ── addCompleteLabelAndRemoveCI atomic-ish behavior ──────────────────────────

// TestAddCompleteLabelAndRemoveCI_AddLabelFails_PreservesAwaitingCI verifies
// that fabrik:awaiting-ci is NOT removed when AddLabelToIssue fails.
// This preserves R3 — the in-flight marker must stay while CI is still pending,
// so the dispatcher continues to suppress re-invocation on the next poll.
func TestAddCompleteLabelAndRemoveCI_AddLabelFails_PreservesAwaitingCI(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			// Simulate a transient GitHub API failure.
			if labelName == fmt.Sprintf("stage:%s:complete", "Validate") {
				return fmt.Errorf("GitHub API 503")
			}
			return nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	eng.addCompleteLabelAndRemoveCI("owner", "repo", item, stage)

	// fabrik:awaiting-ci must NOT be removed — AddLabelToIssue failed.
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("fabrik:awaiting-ci must NOT be removed when AddLabelToIssue fails (R3 preservation)")
		}
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

// ── R1/R2/R3 — merged/closed PR and required-never-running check ──────────────

// TestCheckCIGate_MergedPR_ClearsGate verifies R1: when the linked PR is
// merged, checkCIGate clears the CI gate and adds stage:X:complete without
// requiring check runs.
func TestCheckCIGate_MergedPR_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha-merged", Merged: true, State: "closed"}, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected (false,false,false) for merged PR, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	foundComplete := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Error("expected stage:Validate:complete to be added when PR is merged")
	}
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed when PR is merged")
	}
}

// TestCheckCIGate_ClosedNotMergedPR_Pauses verifies R2: when the linked PR is
// closed without merging, checkCIGate pauses the issue with fabrik:paused +
// fabrik:awaiting-input and removes fabrik:awaiting-ci. stage:X:complete must
// NOT be added.
func TestCheckCIGate_ClosedNotMergedPR_Pauses(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha-closed", Merged: false, State: "closed"}, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected (false,false,false) for closed-not-merged PR, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	foundPaused := false
	foundAwaitingInput := false
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:paused":
			foundPaused = true
		case "fabrik:awaiting-input":
			foundAwaitingInput = true
		case "stage:Validate:complete":
			t.Error("stage:Validate:complete must NOT be added for closed-not-merged PR")
		}
	}
	if !foundPaused {
		t.Error("expected fabrik:paused to be added for closed-not-merged PR")
	}
	if !foundAwaitingInput {
		t.Error("expected fabrik:awaiting-input to be added for closed-not-merged PR")
	}
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed for closed-not-merged PR")
	}
}

// TestCheckCIGate_OpenBlockedNoChecks_DwellNotElapsed_StaysBlocked verifies
// the false-positive guard for R3: when the PR is OPEN+BLOCKED with no check
// runs ever observed but fabrik:awaiting-ci was applied recently (< CIWaitTimeout),
// checkCIGate must return (true, false, false) without pausing. This prevents
// spurious R3 pauses on first push before checks have registered.
func TestCheckCIGate_OpenBlockedNoChecks_DwellNotElapsed_StaysBlocked(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha-blocked", Merged: false, State: "open"}, nil
		},
		fetchPRMergeableStateFn: func(owner, repo string, prNumber int) (string, error) {
			return "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return time.Now().Add(-1 * time.Minute), nil // well within the 30-min default timeout
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if !blocked {
		t.Error("expected blocked=true when OPEN+BLOCKED with no check runs and dwell not elapsed (R3 false-positive guard)")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must NOT be added when dwell has not elapsed (R3 false-positive guard)")
		}
	}
}

// TestCheckCIGate_OpenBlockedNoChecks_DwellElapsed_Pauses verifies R3: when
// the PR is OPEN+BLOCKED with no check runs ever observed and fabrik:awaiting-ci
// has been present for ≥ CIWaitTimeout, checkCIGate pauses with a distinct
// "required check never runs on PR" message.
func TestCheckCIGate_OpenBlockedNoChecks_DwellElapsed_Pauses(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha-blocked-old", Merged: false, State: "open"}, nil
		},
		fetchPRMergeableStateFn: func(owner, repo string, prNumber int) (string, error) {
			return "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return time.Now().Add(-2 * time.Hour), nil // well past the 30-min default timeout
		},
	}
	eng := testEngineForMerge(client)
	eng.cfg.CIWaitTimeout = 30 * time.Minute
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected (false,false,false) for R3 dwell-elapsed pause, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	foundPaused := false
	foundAwaitingInput := false
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:paused":
			foundPaused = true
		case "fabrik:awaiting-input":
			foundAwaitingInput = true
		}
	}
	if !foundPaused {
		t.Error("expected fabrik:paused to be added for R3 required-never-running pause")
	}
	if !foundAwaitingInput {
		t.Error("expected fabrik:awaiting-input to be added for R3 required-never-running pause")
	}
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed for R3 required-never-running pause")
	}
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected a comment to be posted for R3 required-never-running pause")
	}
	if !strings.Contains(client.addCommentCalls[0].body, "PR #5") {
		t.Errorf("expected R3 comment to mention PR #5, got: %q", client.addCommentCalls[0].body[:min(200, len(client.addCommentCalls[0].body))])
	}
	if !strings.Contains(client.addCommentCalls[0].body, "required check") {
		t.Errorf("expected R3 comment to mention 'required check', got: %q", client.addCommentCalls[0].body[:min(200, len(client.addCommentCalls[0].body))])
	}
}

// TestCheckCIGate_OpenBlockedNoChecks_HadChecks_Waits verifies that R5 is
// preserved when mergeableState is "blocked" but hadChecks is true: the engine
// must treat this as a post-push registration delay and return (true, false, false)
// without triggering R3's "required check never runs" pause.
func TestCheckCIGate_OpenBlockedNoChecks_HadChecks_Waits(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha-blocked-hadchecks", Merged: false, State: "open"}, nil
		},
		fetchPRMergeableStateFn: func(owner, repo string, prNumber int) (string, error) {
			return "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(client)
	// Pre-seed: this issue has previously had check runs registered.
	eng.store.Apply(itemstate.PRChecksObserved{Repo: "owner/repo", Number: 1})
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if !blocked {
		t.Error("expected blocked=true when OPEN+BLOCKED with no check runs but hadChecks=true (R5 preserved)")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	// R3 must not fire when hadChecks=true
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must NOT be added when hadChecks=true (R5 preserved — post-push registration delay, not R3)")
		}
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

// TestCheckCIGate_MergeableStateClean_ClearsGate verifies that when GitHub
// reports mergeable_state=clean, the gate clears regardless of raw check_runs
// state. The raw check_runs gate was over-aggressive (any run with
// conclusion=failure blocked, even non-required workflow jobs). When GitHub
// itself says the PR is ready to merge, trust that.
func TestCheckCIGate_MergeableStateClean_ClearsGate(t *testing.T) {
	addCalls := []string{}
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "shaA"}, nil
		},
		fetchPRMergeableStateFn: func(owner, repo string, prNumber int) (string, error) {
			return "clean", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			t.Error("FetchCheckRuns must NOT be called when mergeable_state=clean")
			return nil, nil
		},
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			addCalls = append(addCalls, labelName)
			return nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected gate clear, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	// addCompleteLabelAndRemoveCI should have applied stage:Validate:complete.
	foundComplete := false
	for _, l := range addCalls {
		if l == "stage:Validate:complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Errorf("expected stage:Validate:complete to be added, got addLabelToIssue calls: %v", addCalls)
	}
}

// TestCheckCIGate_MergeableStateUnstable_ClearsGate verifies that
// mergeable_state=unstable (non-required checks failing) also clears the gate.
// This is the "Cleanup artifacts failed but PR is otherwise mergeable" case.
func TestCheckCIGate_MergeableStateUnstable_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "shaB"}, nil
		},
		fetchPRMergeableStateFn: func(owner, repo string, prNumber int) (string, error) {
			return "unstable", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			t.Error("FetchCheckRuns must NOT be called when mergeable_state=unstable")
			return nil, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected gate clear for unstable, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
}

// TestCheckCIGate_MergeableStateBlocked_FallsThroughToCheckRuns verifies that
// mergeable_state=blocked does NOT shortcut — instead the existing per-check
// classification runs to distinguish failure vs pending and apply the right
// label/dispatch.
func TestCheckCIGate_MergeableStateBlocked_FallsThroughToCheckRuns(t *testing.T) {
	checkRunsCalled := false
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "shaC"}, nil
		},
		fetchPRMergeableStateFn: func(owner, repo string, prNumber int) (string, error) {
			return "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			checkRunsCalled = true
			return []gh.CheckRun{{Name: "ci", Status: "in_progress"}}, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, _ := eng.checkCIGate(nil, item, stage)
	if !checkRunsCalled {
		t.Error("FetchCheckRuns must be called when mergeable_state=blocked (fall through)")
	}
	if !blocked || ciFailure {
		t.Errorf("expected blocked-pending for mergeable_state=blocked + in_progress checks, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
}

// TestCheckCIGate_EmptyHeadSHA_StaysBlocked is a regression test for the
// original symptom of issue #779: when the boardcache layer returned a non-nil
// PRDetails with HeadSHA=="" (due to Bugs 1/2/3 in the cache layer), checkCIGate
// was clearing the CI gate as if no PR existed, silently disarming the safety
// mechanism. After the fix, a non-nil PR with an empty HeadSHA is treated as
// "data incomplete — block until SHA is populated" rather than "no PR — gate clears."
func TestCheckCIGate_EmptyHeadSHA_StaysBlocked(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			// Simulate the stale-cache scenario: PR is known but HeadSHA is empty.
			return &gh.PRDetails{Number: 5, Title: "My PR", HeadSHA: ""}, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if !blocked {
		t.Error("expected blocked=true when PR exists but HeadSHA is empty")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false for incomplete HeadSHA, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	// addCompleteLabelAndRemoveCI must NOT have been called.
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when HeadSHA is empty (CI gate must stay armed)")
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("fabrik:awaiting-ci must NOT be removed when HeadSHA is empty")
		}
	}
}

// TestRemoveAwaitingCILabel_ErrNotFound verifies that a 404 from
// RemoveLabelFromIssue is treated as success (label already absent) — exactly
// one call, no warning logged, and cache write-through applied.
func TestRemoveAwaitingCILabel_ErrNotFound(t *testing.T) {
	var calls int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:awaiting-ci" {
				calls++
				return fmt.Errorf("GitHub API returned 404: label not found: %w", gh.ErrNotFound)
			}
			return nil
		},
	}
	eng, cache := testEngineWithCache(client, &mockClaudeInvoker{})
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), "fabrik:awaiting-ci")

	eventsCh := make(chan tui.Event, 16)
	eng.events = eventsCh

	item := gh.ProjectItem{
		Number: 1,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-ci"},
	}

	eng.removeAwaitingCILabel("owner", "repo", item)

	if calls != 1 {
		t.Errorf("expected exactly 1 RemoveLabelFromIssue call for ErrNotFound, got %d", calls)
	}

	// No warn log should be emitted when ErrNotFound is returned.
	close(eventsCh)
	for ev := range eventsCh {
		if le, ok := ev.(tui.LogEvent); ok && le.Tag == "warn" {
			t.Errorf("unexpected warn log: %q", le.Message)
		}
	}

	// Cache write-through applied: fabrik:awaiting-ci must be absent from cache.
	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	for _, l := range labels {
		if l == "fabrik:awaiting-ci" {
			t.Error("expected fabrik:awaiting-ci to be removed from cache after ErrNotFound")
		}
	}
}

// TestCheckCIGate_BehindNoChecks_Blocks verifies SC-2: when mergeable_state="behind"
// (branch is behind the base) and check_runs=[] and hadChecks=false, the new guard
// must return (true, false, false) without clearing the gate or adding
// stage:Validate:complete. The "behind" state signals that branch protection is
// blocking via a signal Fabrik cannot see via check_runs (e.g. up-to-date policy).
func TestCheckCIGate_BehindNoChecks_Blocks(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha-behind", Merged: false, State: "open"}, nil
		},
		fetchPRMergeableStateFn: func(owner, repo string, prNumber int) (string, error) {
			return "behind", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if !blocked {
		t.Error("expected blocked=true when mergeable_state=behind with no check_runs and hadChecks=false (new guard)")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when new guard blocks (mergeable_state=behind, no check_runs)")
		}
	}
}

// TestCheckCIGate_DirtyNoChecks_Blocks verifies SC-2: when mergeable_state="dirty"
// (merge conflict) and check_runs=[] and hadChecks=false, the new guard must
// return (true, false, false) without clearing the gate or adding
// stage:Validate:complete. The "dirty" state signals that branch protection is
// blocking due to a merge conflict — a signal Fabrik cannot see via check_runs.
func TestCheckCIGate_DirtyNoChecks_Blocks(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha-dirty", Merged: false, State: "open"}, nil
		},
		fetchPRMergeableStateFn: func(owner, repo string, prNumber int) (string, error) {
			return "dirty", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if !blocked {
		t.Error("expected blocked=true when mergeable_state=dirty with no check_runs and hadChecks=false (new guard)")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when new guard blocks (mergeable_state=dirty, no check_runs)")
		}
	}
}

// TestCheckCIGate_BehindNoChecks_TimeoutElapsed_TimesOut verifies that the new guard
// returns (false, false, true) and removes fabrik:awaiting-ci when mergeable_state
// is "behind", check_runs=[] and fabrik:awaiting-ci has been present for >= CIWaitTimeout.
// This guards against indefinite blocking when branch protection signals "behind" via a
// signal Fabrik cannot see via check_runs.
func TestCheckCIGate_BehindNoChecks_TimeoutElapsed_TimesOut(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, HeadSHA: "sha-behind-timeout", Merged: false, State: "open"}, nil
		},
		fetchPRMergeableStateFn: func(owner, repo string, prNumber int) (string, error) {
			return "behind", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return time.Now().Add(-2 * time.Hour), nil // well past the 30-min default timeout
		},
	}
	eng := testEngineForMerge(client)
	eng.cfg.CIWaitTimeout = 30 * time.Minute
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, ciFailure, timedOut := eng.checkCIGate(nil, item, stage)
	if blocked || ciFailure {
		t.Errorf("expected blocked=false ciFailure=false for timed-out new guard, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
	if !timedOut {
		t.Error("expected timedOut=true when fabrik:awaiting-ci elapsed >= CIWaitTimeout and mergeable_state=behind with no check_runs")
	}
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed when new guard times out")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when new guard times out")
		}
	}
}
