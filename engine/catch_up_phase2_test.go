package engine

import (
	"context"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestRunCatchUpPhase2_YoloRemovedMidRun_NoAutoMergeNoAdvance is Acceptance
// Criterion 3: fabrik:yolo removed while a wait_for_ci: true stage (Validate)
// is running must be observed by runCatchUpPhase2 (D2's path) — no advance
// and no auto-merge enablement once the CI gate clears.
//
// item.Labels here simulates the dispatch-time-stale snapshot the catch-up
// loop still holds after a wait_for_ci: true stage's handleStageComplete
// deferred to the catch-up loop (the item still shows fabrik:yolo from
// before CI cleared, but the operator has since removed it). fetchLabelsFn
// simulates the operator's mid-run removal by returning the current,
// yolo-free label set on a live re-fetch.
func TestRunCatchUpPhase2_YoloRemovedMidRun_NoAutoMergeNoAdvance(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{}, nil // fabrik:yolo has been removed
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "abc123"}, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(t, client, stgs)
	// cfg.Yolo stays false — only the (stale) item-level label claims yolo.

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.runCatchUpPhase2(context.Background(), board, item, validateStage, map[string]bool{})

	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("expected no EnablePullRequestAutoMerge call after yolo was removed mid-run, got %d", len(client.enablePullRequestAutoMergeCalls))
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR call after yolo was removed mid-run, got %d", len(client.mergePRCalls))
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no board-status advance after yolo was removed mid-run, got %d", len(client.updateStatusCalls))
	}
}

// TestRunCatchUpPhase2_YoloStillPresent_AutoMergeEnabled is the non-vacuousness
// sibling of the above: with the same stale-yolo snapshot but a fetchLabelsFn
// confirming yolo is still present, runCatchUpPhase2 must still enable
// auto-merge — proving the no-op above is because yolo was actually observed
// as removed, not because runCatchUpPhase2 never reaches the merge branch at
// all in this test setup.
func TestRunCatchUpPhase2_YoloStillPresent_AutoMergeEnabled(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{"fabrik:yolo"}, nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "abc123"}, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(t, client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.runCatchUpPhase2(context.Background(), board, item, validateStage, map[string]bool{})

	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected 1 EnablePullRequestAutoMerge call when yolo is still present, got %d", len(client.enablePullRequestAutoMergeCalls))
	}
	if client.enablePullRequestAutoMergeCalls[0].prNumber != 99 {
		t.Errorf("EnablePullRequestAutoMerge called with prNumber %d, want 99", client.enablePullRequestAutoMergeCalls[0].prNumber)
	}
}
