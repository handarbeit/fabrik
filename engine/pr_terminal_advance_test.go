package engine

import (
	"os/exec"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// addedLabelNames returns all label names passed to AddLabelToIssue.
func addedLabelNames(calls []addLabelCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.labelName)
	}
	return out
}

// removedLabelNames returns all label names passed to RemoveLabelFromIssue.
func removedLabelNames(calls []removeLabelCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.labelName)
	}
	return out
}

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
				// pauseForPRClosedNotMerged must clear whichever gate label was
				// present, not just fabrik:awaiting-ci — otherwise fabrik:awaiting-review
				// or fabrik:rebase-needed is permanently stranded on a closed, paused
				// item once cleanupClosedIssueTransientLabels stops sweeping them at
				// Validate (R6, ADR-1387; caught by Pruefer on PR #1388).
				if tc.gateLabel != "" {
					removed := removedLabelNames(client.removeLabelCalls)
					if !containsLabel(removed, tc.gateLabel) {
						t.Errorf("expected gate label %q to be removed on pause; got %v", tc.gateLabel, removed)
					}
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

// terminalAdvanceStagesWithHolding mirrors terminalAdvanceStages but inserts a
// HoldingStage (Queued) between Validate and Done, matching the shipped
// merge-train pipeline shape (Validate → Queued → Done).
func terminalAdvanceStagesWithHolding() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1},
		{Name: "Review", Order: 2, WaitForReviews: &tr},
		{Name: "Validate", Order: 3, WaitForCI: &tr},
		{Name: "Queued", Order: 4, HoldingStage: true},
		{Name: "Done", Order: 5, CleanupWorktree: true},
	}
}

// TestValidatePRTerminalAdvance_HoldingStageSkipped_AdvancesToDone is the
// end-to-end regression guard for issue #1072: a merged Validate-stage PR
// (cruise or human-merge shape — the single owner has no yolo/cruise gate)
// must advance directly to Done even when a HoldingStage (Queued) is
// configured immediately after Validate, never landing in Queued. Before the
// NextStage fix, this path stranded every such item in Queued forever,
// regardless of merge_train state.
func TestValidatePRTerminalAdvance_HoldingStageSkipped_AdvancesToDone(t *testing.T) {
	stgs := terminalAdvanceStagesWithHolding()
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, Merged: true, State: "closed"}, nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{
		Number: 42,
		ItemID: "PVTI_42",
		Status: "Validate",
		Labels: []string{"stage:Implement:complete", "fabrik:cruise"},
	}
	advancedItems := make(map[string]bool)
	eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, advancedItems)

	iKey := issueKey(item, eng.defaultRepo())
	if !advancedItems[iKey] {
		t.Fatalf("expected item to be marked as advanced")
	}
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update call, got %d", len(client.updateStatusCalls))
	}
	if got := client.updateStatusCalls[0].optionID; got != "OPT_Done" {
		t.Errorf("expected item to advance directly to Done (skipping Queued), got option %s", got)
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

// TestValidatePRTerminalAdvance_UnmanagedCleanupStageDoesNotBreakFillLoop is the
// regression guard for a PR review finding on issue #973: the gate-checked
// completion-label fill loop breaks on the first CleanupWorktree stage it
// encounters, to stop before the terminal Done stage. LoadAll sorts stages by
// Order, so a misconfigured stage combining unmanaged: true with
// cleanup_worktree: true at a low Order (e.g. an order: -1 Backlog) would sit
// at index 0 and break the loop immediately — filling zero gate-checked
// completion labels, the exact resolution cleanupStage() was hardened to
// avoid. The loop must skip a CleanupWorktree stage that is also Unmanaged and
// keep going until it reaches the real (non-unmanaged) Done stage.
func TestValidatePRTerminalAdvance_UnmanagedCleanupStageDoesNotBreakFillLoop(t *testing.T) {
	tr := true
	stgs := []*stages.Stage{
		{Name: "Backlog", Order: -1, Unmanaged: true, CleanupWorktree: true},
		{Name: "Implement", Order: 1},
		{Name: "Review", Order: 2, WaitForReviews: &tr},
		{Name: "Validate", Order: 3, WaitForCI: &tr},
		{Name: "Done", Order: 4, CleanupWorktree: true},
	}
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
		t.Errorf("expected stage:Review:complete to be added despite the unmanaged Backlog cleanup stage; got %v", added)
	}
	if !containsLabel(added, "stage:Validate:complete") {
		t.Errorf("expected stage:Validate:complete to be added despite the unmanaged Backlog cleanup stage; got %v", added)
	}
	if !advancedItems[issueKey(item, eng.defaultRepo())] {
		t.Error("expected item to be marked as advanced")
	}
}

// TestValidatePRTerminalAdvance_UnmanagedGateCheckedStage_NoSpuriousLabel is the
// regression guard for a PR review finding on issue #973: the prior fix for
// TestValidatePRTerminalAdvance_UnmanagedCleanupStageDoesNotBreakFillLoop only
// excluded Unmanaged from the CleanupWorktree break condition
// (`s.CleanupWorktree && !s.Unmanaged`), so the loop still fell through to the
// gate-checked fill logic for an Unmanaged stage that ALSO carries a gate flag
// (wait_for_ci/wait_for_reviews) — a combination loadOne permits, same as
// unmanaged + cleanup_worktree. An Unmanaged stage must never receive a
// stage:<name>:complete label here regardless of what other flags it carries;
// it was never dispatched to.
func TestValidatePRTerminalAdvance_UnmanagedGateCheckedStage_NoSpuriousLabel(t *testing.T) {
	tr := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1},
		{Name: "OnHold", Order: 2, Unmanaged: true, WaitForCI: &tr},
		{Name: "Validate", Order: 3, WaitForCI: &tr},
		{Name: "Done", Order: 4, CleanupWorktree: true},
	}
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 8, Merged: true}, nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 51,
		ItemID: "PVTI_51",
		Status: "Validate",
		Labels: []string{
			"stage:Implement:complete",
			"fabrik:awaiting-ci",
			// Missing stage:Validate:complete; OnHold (unmanaged) never had one to begin with.
		},
	}
	advancedItems := make(map[string]bool)
	eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, advancedItems)

	added := addedLabelNames(client.addLabelCalls)
	if containsLabel(added, "stage:OnHold:complete") {
		t.Errorf("unmanaged gate-checked stage OnHold must never receive a completion label; got %v", added)
	}
	if !containsLabel(added, "stage:Validate:complete") {
		t.Errorf("expected stage:Validate:complete to be added; got %v", added)
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

// testEngineWithStagesAndRealWM is like testEngineWithStages but registers a
// real git-backed WorktreeManager at "owner/repo" (default branch "main")
// instead of the placeholder non-git one, so baseBranchForItem/DefaultBaseBranch
// resolve against real git state — required to exercise
// closeIssueIfNonDefaultBase's base:<branch> resolution.
func testEngineWithStagesAndRealWM(t *testing.T, client *mockGitHubClient, stgs []*stages.Stage) *Engine {
	t.Helper()
	skipIfNoGit(t)
	_, _, worktreeRoot, wm := setupTrainRepo(t)

	shaCmd := exec.Command("git", "rev-parse", "HEAD")
	shaCmd.Dir = wm.baseDir
	shaOut, err := shaCmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	mustGitDir(t, wm.baseDir, "update-ref", "refs/remotes/origin/develop", strings.TrimSpace(string(shaOut)))

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
		&mockClaudeInvoker{},
		nil,
	)
	eng.registerWorktrees("owner/repo", wm.baseDir, worktreeRoot)
	opts := make(map[string]string)
	for _, s := range stgs {
		opts[s.Name] = "OPT_" + s.Name
	}
	eng.statusField = &gh.StatusField{FieldID: "FIELD_1", Options: opts}
	return eng
}

// TestValidatePRTerminalAdvance_NonDefaultBase_ClosesIssue verifies the #1096
// cruise-path explicit close: a merged PR whose item carries a base:develop
// label (default branch main) must trigger CloseIssue, since GitHub's
// Closes #N auto-close is inert for a non-default-base merge.
func TestValidatePRTerminalAdvance_NonDefaultBase_ClosesIssue(t *testing.T) {
	stgs := terminalAdvanceStages()
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, Merged: true, State: "closed"}, nil
		},
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	eng := testEngineWithStagesAndRealWM(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 42,
		ItemID: "PVTI_42",
		Repo:   "owner/repo",
		Status: "Validate",
		Labels: []string{"stage:Implement:complete", "base:develop"},
	}
	advancedItems := make(map[string]bool)
	eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, advancedItems)

	if len(client.closeIssueCalls) != 1 {
		t.Fatalf("expected CloseIssue to be called once for a non-default-base merge, got %v", client.closeIssueCalls)
	}
	c := client.closeIssueCalls[0]
	if c.owner != "owner" || c.repo != "repo" || c.issueNumber != 42 {
		t.Errorf("unexpected CloseIssue args: %+v", c)
	}
}

// TestValidatePRTerminalAdvance_DefaultBase_DoesNotCloseIssue verifies the
// no-double-close guard: a merged PR whose item carries no base: label (so its
// resolved base equals the repo default) must NOT trigger an explicit
// CloseIssue — GitHub's own Closes #N auto-close already handles that case.
func TestValidatePRTerminalAdvance_DefaultBase_DoesNotCloseIssue(t *testing.T) {
	stgs := terminalAdvanceStages()
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 11, Merged: true, State: "closed"}, nil
		},
		closeIssueFn: func(owner, repo string, n int) error {
			t.Fatalf("CloseIssue should not be called when base == default")
			return nil
		},
	}
	eng := testEngineWithStagesAndRealWM(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 43,
		ItemID: "PVTI_43",
		Repo:   "owner/repo",
		Status: "Validate",
		Labels: []string{"stage:Implement:complete"},
	}
	advancedItems := make(map[string]bool)
	eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, advancedItems)

	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue call when base == default, got %v", client.closeIssueCalls)
	}
}

// TestValidatePRTerminalAdvance_ClosedItemSkipped_NoFetch (ADR-1387) verifies
// that runValidatePRTerminalAdvance — now the open-item-only owner — never
// touches a closed item: no FetchLinkedPR call, no label mutation, no advance.
// Closed items at Validate are the exclusive responsibility of the
// board-sourced sibling, settleClosedValidateAdvance.
func TestValidatePRTerminalAdvance_ClosedItemSkipped_NoFetch(t *testing.T) {
	stgs := terminalAdvanceStages()
	fetchCalls := 0
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			fetchCalls++
			return &gh.PRDetails{Number: 10, Merged: true, State: "closed"}, nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:   42,
		ItemID:   "PVTI_42",
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"stage:Implement:complete", "fabrik:awaiting-ci"},
	}
	advancedItems := make(map[string]bool)
	eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, advancedItems)

	if fetchCalls != 0 {
		t.Errorf("expected FetchLinkedPR to never be called for a closed item, got %d call(s)", fetchCalls)
	}
	if len(client.updateStatusCalls) != 0 {
		t.Error("expected no status update for a closed item")
	}
	if len(client.addLabelCalls) != 0 {
		t.Error("expected no label mutation for a closed item")
	}
	if advancedItems[issueKey(item, eng.defaultRepo())] {
		t.Error("expected closed item to not be marked as advanced by the open-item owner")
	}
}

// TestSettleClosedValidateAdvance_TableDriven mirrors
// TestValidatePRTerminalAdvance_TableDriven's gate-label × PR-state matrix, but
// for closed items sourced from board.Items via settleClosedValidateAdvance
// (ADR-1387, R2/R4). Confirms #874-class healing continues to work now that
// dispatch admission no longer feeds the settle-owner.
func TestSettleClosedValidateAdvance_TableDriven(t *testing.T) {
	stgs := terminalAdvanceStages()

	cases := []struct {
		name          string
		gateLabel     string
		prMerged      bool
		prState       string
		alreadyPaused bool
		wantAdvanced  bool
		wantPaused    bool
		wantSkipped   bool
	}{
		{name: "none/merged", gateLabel: "", prMerged: true, prState: "closed", wantAdvanced: true},
		{name: "none/closed-unmerged", gateLabel: "", prMerged: false, prState: "closed", wantPaused: true},
		{name: "none/open", gateLabel: "", prMerged: false, prState: "open", wantSkipped: true},

		{name: "awaiting-ci/merged", gateLabel: "fabrik:awaiting-ci", prMerged: true, prState: "closed", wantAdvanced: true},
		{name: "awaiting-ci/closed-unmerged", gateLabel: "fabrik:awaiting-ci", prMerged: false, prState: "closed", wantPaused: true},

		{name: "awaiting-review/merged", gateLabel: "fabrik:awaiting-review", prMerged: true, prState: "closed", wantAdvanced: true},
		{name: "awaiting-review/paused/merged", gateLabel: "fabrik:awaiting-review", prMerged: true, prState: "closed", alreadyPaused: true, wantAdvanced: true},
		// Regression case for the label-hygiene gap Pruefer caught on PR #1388:
		// pauseForPRClosedNotMerged must clear fabrik:awaiting-review, not just
		// fabrik:awaiting-ci — cleanupClosedIssueTransientLabels no longer sweeps
		// it at Validate (R6), so this is the only place left to clear it.
		{name: "awaiting-review/closed-unmerged", gateLabel: "fabrik:awaiting-review", prMerged: false, prState: "closed", wantPaused: true},

		{name: "paused-only/merged", gateLabel: "", prMerged: true, prState: "closed", alreadyPaused: true, wantAdvanced: true},

		{name: "rebase-needed/merged", gateLabel: "fabrik:rebase-needed", prMerged: true, prState: "closed", wantAdvanced: true},
		// Same regression case as above, for fabrik:rebase-needed.
		{name: "rebase-needed/closed-unmerged", gateLabel: "fabrik:rebase-needed", prMerged: false, prState: "closed", wantPaused: true},

		// Already paused + closed-unmerged — must skip to avoid a duplicate comment.
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

			labels := []string{"stage:Implement:complete"}
			if tc.gateLabel != "" {
				labels = append(labels, tc.gateLabel)
			}
			if tc.alreadyPaused {
				labels = append(labels, "fabrik:paused")
			}

			item := gh.ProjectItem{
				Number:   42,
				ItemID:   "PVTI_42",
				Status:   "Validate",
				IsClosed: true,
				Labels:   labels,
			}
			board := &gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{item}}
			advancedItems := make(map[string]bool)
			eng.settleClosedValidateAdvance(board, advancedItems)

			iKey := issueKey(item, eng.defaultRepo())

			switch {
			case tc.wantAdvanced:
				if !advancedItems[iKey] {
					t.Errorf("expected closed item to be marked as advanced in advancedItems")
				}
				if len(client.updateStatusCalls) == 0 {
					t.Errorf("expected advanceToNextStage to call UpdateProjectItemStatus")
				}
				added := addedLabelNames(client.addLabelCalls)
				if !containsLabel(added, "stage:Validate:complete") {
					t.Errorf("expected stage:Validate:complete to be added; got %v", added)
				}
				if tc.gateLabel != "" {
					removed := removedLabelNames(client.removeLabelCalls)
					if !containsLabel(removed, tc.gateLabel) {
						t.Errorf("expected gate label %q to be removed; got %v", tc.gateLabel, removed)
					}
				}
				if tc.alreadyPaused {
					removed := removedLabelNames(client.removeLabelCalls)
					if !containsLabel(removed, "fabrik:paused") {
						t.Errorf("expected fabrik:paused to be removed; got %v", removed)
					}
				}

			case tc.wantPaused:
				if advancedItems[iKey] {
					t.Errorf("expected closed item NOT to be advanced on closed-unmerged PR")
				}
				if len(client.addCommentCalls) == 0 {
					t.Errorf("expected pauseForPRClosedNotMerged to post a comment")
				}
				added := addedLabelNames(client.addLabelCalls)
				if !containsLabel(added, "fabrik:paused") {
					t.Errorf("expected fabrik:paused to be added; got %v", added)
				}
				// pauseForPRClosedNotMerged must clear whichever gate label was
				// present — cleanupClosedIssueTransientLabels no longer sweeps it
				// independently for a closed item at Validate (R6, ADR-1387).
				if tc.gateLabel != "" {
					removed := removedLabelNames(client.removeLabelCalls)
					if !containsLabel(removed, tc.gateLabel) {
						t.Errorf("expected gate label %q to be removed on pause; got %v", tc.gateLabel, removed)
					}
				}

			case tc.wantSkipped:
				if advancedItems[iKey] {
					t.Errorf("expected closed item NOT to be advanced")
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

// TestSettleClosedValidateAdvance_OpenItemSkipped verifies the IsClosed
// partition holds in the other direction: settleClosedValidateAdvance never
// touches an open item, even one sourced from board.Items.
func TestSettleClosedValidateAdvance_OpenItemSkipped(t *testing.T) {
	stgs := terminalAdvanceStages()
	fetchCalls := 0
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			fetchCalls++
			return &gh.PRDetails{Number: 10, Merged: true, State: "closed"}, nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)
	item := gh.ProjectItem{
		Number:   43,
		ItemID:   "PVTI_43",
		Status:   "Validate",
		IsClosed: false,
		Labels:   []string{"stage:Implement:complete"},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{item}}
	advancedItems := make(map[string]bool)
	eng.settleClosedValidateAdvance(board, advancedItems)

	if fetchCalls != 0 {
		t.Errorf("expected FetchLinkedPR to never be called for an open item, got %d call(s)", fetchCalls)
	}
	if advancedItems[issueKey(item, eng.defaultRepo())] {
		t.Error("expected open item to not be marked as advanced by settleClosedValidateAdvance")
	}
}

// TestSettleClosedValidateAdvance_NoDoubleAdvance verifies dedup via
// advancedItems holds for the closed-item owner too, mirroring
// TestValidatePRTerminalAdvance_NoDoubleAdvance.
func TestSettleClosedValidateAdvance_NoDoubleAdvance(t *testing.T) {
	stgs := terminalAdvanceStages()
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, Merged: true}, nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)
	item := gh.ProjectItem{
		Number:   20,
		ItemID:   "PVTI_20",
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"stage:Implement:complete"},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{item}}
	advancedItems := map[string]bool{
		issueKey(item, eng.defaultRepo()): true,
	}
	eng.settleClosedValidateAdvance(board, advancedItems)

	if len(client.updateStatusCalls) > 0 {
		t.Error("expected no status update for already-advanced closed item")
	}
}
