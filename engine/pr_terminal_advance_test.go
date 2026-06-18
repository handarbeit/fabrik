package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// terminalAdvanceStages returns a pipeline with Review (wait_for_reviews),
// Validate (wait_for_ci), and Done (cleanup), matching the standard Fabrik pipeline.
func terminalAdvanceStages() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1},
		{Name: "Review", Order: 2, WaitForReviews: &tr},
		{Name: "Validate", Order: 3, WaitForCI: &tr},
		{Name: "Done", Order: 4, CleanupWorktree: true},
	}
}

// TestValidatePRTerminalAdvance_TableDriven tests the single-owner function
// across all gate label × PR state combinations required by the acceptance criteria.
// Verifies single dispatch, no double-advance, no strand, and paused items handled.
func TestValidatePRTerminalAdvance_TableDriven(t *testing.T) {
	stgs := terminalAdvanceStages()

	cases := []struct {
		name          string
		gateLabel     string // "" for no gate label
		prMerged      bool
		prState       string // "open" or "closed"
		alreadyPaused bool
		wantAdvanced  bool
		wantPaused    bool // expect pauseForPRClosedNotMerged to fire
		wantSkipped   bool // neither advanced nor paused
	}{
		// No gate label
		{name: "none/merged", gateLabel: "", prMerged: true, prState: "closed", wantAdvanced: true},
		{name: "none/closed-unmerged", gateLabel: "", prMerged: false, prState: "closed", wantPaused: true},
		{name: "none/open", gateLabel: "", prMerged: false, prState: "open", wantSkipped: true},

		// fabrik:awaiting-ci gate
		{name: "awaiting-ci/merged", gateLabel: "fabrik:awaiting-ci", prMerged: true, prState: "closed", wantAdvanced: true},
		{name: "awaiting-ci/closed-unmerged", gateLabel: "fabrik:awaiting-ci", prMerged: false, prState: "closed", wantPaused: true},
		{name: "awaiting-ci/open", gateLabel: "fabrik:awaiting-ci", prMerged: false, prState: "open", wantSkipped: true},

		// fabrik:awaiting-review gate
		{name: "awaiting-review/merged", gateLabel: "fabrik:awaiting-review", prMerged: true, prState: "closed", wantAdvanced: true},
		{name: "awaiting-review/closed-unmerged", gateLabel: "fabrik:awaiting-review", prMerged: false, prState: "closed", wantPaused: true},
		{name: "awaiting-review/open", gateLabel: "fabrik:awaiting-review", prMerged: false, prState: "open", wantSkipped: true},

		// fabrik:rebase-needed gate
		{name: "rebase-needed/merged", gateLabel: "fabrik:rebase-needed", prMerged: true, prState: "closed", wantAdvanced: true},
		{name: "rebase-needed/closed-unmerged", gateLabel: "fabrik:rebase-needed", prMerged: false, prState: "closed", wantPaused: true},
		{name: "rebase-needed/open", gateLabel: "fabrik:rebase-needed", prMerged: false, prState: "open", wantSkipped: true},

		// Paused items with merged PR — these were stranded without the single owner
		{name: "awaiting-ci+paused/merged", gateLabel: "fabrik:awaiting-ci", prMerged: true, prState: "closed", alreadyPaused: true, wantAdvanced: true},
		{name: "awaiting-review+paused/merged", gateLabel: "fabrik:awaiting-review", prMerged: true, prState: "closed", alreadyPaused: true, wantAdvanced: true},
		{name: "rebase-needed+paused/merged", gateLabel: "fabrik:rebase-needed", prMerged: true, prState: "closed", alreadyPaused: true, wantAdvanced: true},

		// Paused item with closed PR — already paused; single owner must skip to avoid duplicate comment
		{name: "awaiting-ci+paused/closed-unmerged", gateLabel: "fabrik:awaiting-ci", prMerged: false, prState: "closed", alreadyPaused: true, wantSkipped: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{
				fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
					return &gh.PRDetails{Number: 10, Merged: tc.prMerged, State: tc.prState}, nil
				},
			}
			eng := testEngineWithStages(t, client, stgs)
			board := &gh.ProjectBoard{ProjectID: "PVT_1"}

			labels := []string{"stage:Implement:complete"}
			if tc.gateLabel != "" {
				labels = append(labels, tc.gateLabel)
			}
			if tc.alreadyPaused {
				labels = append(labels, "fabrik:paused")
			}

			item := gh.ProjectItem{
				Number: 42,
				ItemID: "PVTI_42",
				Status: "Validate",
				Labels: labels,
			}
			advancedItems := make(map[string]bool)
			eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, advancedItems)

			iKey := issueKey(item, eng.defaultRepo())

			switch {
			case tc.wantAdvanced:
				if !advancedItems[iKey] {
					t.Errorf("expected item to be marked as advanced in advancedItems")
				}
				if len(client.updateStatusCalls) == 0 {
					t.Errorf("expected advanceToNextStage to call UpdateProjectItemStatus")
				}
				// stage:Validate:complete must be added (gate-checked stage)
				added := addedLabelNames(client.addLabelCalls)
				if !containsLabel(added, "stage:Validate:complete") {
					t.Errorf("expected stage:Validate:complete to be added; got %v", added)
				}
				// Gate label must be removed if it was present
				if tc.gateLabel != "" {
					removed := removedLabelNames(client.removeLabelCalls)
					if !containsLabel(removed, tc.gateLabel) {
						t.Errorf("expected gate label %q to be removed; got %v", tc.gateLabel, removed)
					}
				}
				// fabrik:paused must be removed if the item was paused
				if tc.alreadyPaused {
					removed := removedLabelNames(client.removeLabelCalls)
					if !containsLabel(removed, "fabrik:paused") {
						t.Errorf("expected fabrik:paused to be removed; got %v", removed)
					}
				}

			case tc.wantPaused:
				if advancedItems[iKey] {
					t.Errorf("expected item NOT to be advanced on closed-unmerged PR")
				}
				if len(client.addCommentCalls) == 0 {
					t.Errorf("expected pauseForPRClosedNotMerged to post a comment")
				}
				added := addedLabelNames(client.addLabelCalls)
				if !containsLabel(added, "fabrik:paused") {
					t.Errorf("expected fabrik:paused to be added; got %v", added)
				}

			case tc.wantSkipped:
				if advancedItems[iKey] {
					t.Errorf("expected item NOT to be advanced on open PR")
				}
				if len(client.addCommentCalls) > 0 {
					t.Errorf("expected no comment; got %d comment(s)", len(client.addCommentCalls))
				}
				if len(client.updateStatusCalls) > 0 {
					t.Errorf("expected no status update; got %d call(s)", len(client.updateStatusCalls))
				}
			}
		})
	}
}

// TestValidatePRTerminalAdvance_SyntheticGateLabel is the structural regression
// test that closes the #874 bug class: the single owner advances a Validate-stage
// item regardless of which gate label is present, including a synthetic label the
// engine has never seen before. No label negation is required.
func TestValidatePRTerminalAdvance_SyntheticGateLabel(t *testing.T) {
	stgs := terminalAdvanceStages()
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, Merged: true}, nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	// Item with a synthetic gate label the engine code has never handled.
	// The single owner must still advance it when the PR is merged.
	item := gh.ProjectItem{
		Number: 99,
		ItemID: "PVTI_99",
		Status: "Validate",
		Labels: []string{
			"stage:Implement:complete",
			"fabrik:paused",
			"fabrik:synthetic-gate", // unknown gate label — previously caused stranding
		},
	}
	advancedItems := make(map[string]bool)
	eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, advancedItems)

	if !advancedItems[issueKey(item, eng.defaultRepo())] {
		t.Error("expected item with synthetic gate label to be advanced on merged PR")
	}
	if len(client.updateStatusCalls) == 0 {
		t.Error("expected advanceToNextStage to call UpdateProjectItemStatus")
	}
	// fabrik:paused must be removed despite the synthetic gate label
	removed := removedLabelNames(client.removeLabelCalls)
	if !containsLabel(removed, "fabrik:paused") {
		t.Errorf("expected fabrik:paused to be removed; got %v", removed)
	}
}

// TestValidatePRTerminalAdvance_NoDoubleAdvance verifies that an item already
// present in advancedItems is not advanced a second time.
func TestValidatePRTerminalAdvance_NoDoubleAdvance(t *testing.T) {
	stgs := terminalAdvanceStages()
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, Merged: true}, nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 20,
		ItemID: "PVTI_20",
		Status: "Validate",
		Labels: []string{"stage:Implement:complete"},
	}
	// Pre-mark item as already advanced
	advancedItems := map[string]bool{
		issueKey(item, eng.defaultRepo()): true,
	}
	eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, advancedItems)

	if len(client.updateStatusCalls) > 0 {
		t.Error("expected no status update for already-advanced item")
	}
}

// TestValidatePRTerminalAdvance_AutoMergeExcluded verifies that items with
// fabrik:auto-merge-enabled are excluded from the single owner.
// These items are handled exclusively by checkAutoMergeConvergence (Phase 1).
func TestValidatePRTerminalAdvance_AutoMergeExcluded(t *testing.T) {
	stgs := terminalAdvanceStages()
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 3, Merged: true}, nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 30,
		ItemID: "PVTI_30",
		Status: "Validate",
		Labels: []string{"stage:Implement:complete", "fabrik:auto-merge-enabled"},
	}
	advancedItems := make(map[string]bool)
	eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, advancedItems)

	if len(client.updateStatusCalls) > 0 {
		t.Error("expected no status update for auto-merge-enabled item")
	}
}

// TestValidatePRTerminalAdvance_FillBothGateCheckedLabels verifies that when
// a Validate-stage item is missing both Review:complete and Validate:complete
// (paused after Implement before either gate ran), both are added in order.
func TestValidatePRTerminalAdvance_FillBothGateCheckedLabels(t *testing.T) {
	stgs := terminalAdvanceStages()
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 8, Merged: true}, nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 50,
		ItemID: "PVTI_50",
		Status: "Validate",
		Labels: []string{
			"stage:Implement:complete",
			"fabrik:paused",
			"fabrik:awaiting-ci",
			// Missing stage:Review:complete and stage:Validate:complete
		},
	}
	advancedItems := make(map[string]bool)
	eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, advancedItems)

	added := addedLabelNames(client.addLabelCalls)
	if !containsLabel(added, "stage:Review:complete") {
		t.Errorf("expected stage:Review:complete to be added; got %v", added)
	}
	if !containsLabel(added, "stage:Validate:complete") {
		t.Errorf("expected stage:Validate:complete to be added; got %v", added)
	}
	if !advancedItems[issueKey(item, eng.defaultRepo())] {
		t.Error("expected item to be marked as advanced")
	}
}

// TestValidatePRTerminalAdvance_NonValidateSkipped verifies that items at
// non-Validate stages are ignored by the single owner.
func TestValidatePRTerminalAdvance_NonValidateSkipped(t *testing.T) {
	stgs := terminalAdvanceStages()
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 2, Merged: true}, nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	// Item at Review stage (not Validate) with a merged PR
	item := gh.ProjectItem{
		Number: 15,
		ItemID: "PVTI_15",
		Status: "Review",
		Labels: []string{"stage:Implement:complete", "fabrik:paused", "fabrik:awaiting-review"},
	}
	advancedItems := make(map[string]bool)
	eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, advancedItems)

	if len(client.updateStatusCalls) > 0 {
		t.Error("expected no status update for non-Validate item")
	}
}
