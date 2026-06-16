package engine

import (
	"context"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

func testStagesForRevalidate() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Implement", Order: 3, Prompt: "implement"},
		{Name: "Validate", Order: 4, Prompt: "validate"},
	}
}

// TestRevalidate_ClearsLabelsOnValidateStage verifies SC-1: an item at Validate
// with all gate/completion labels + fabrik:revalidate has all 7 labels removed
// by the revalidate-scan loop in a single poll cycle.
func TestRevalidate_ClearsLabelsOnValidateStage(t *testing.T) {
	stgs := testStagesForRevalidate()
	allLabels := []string{
		"stage:Validate:complete",
		"stage:Validate:failed",
		"fabrik:paused",
		"fabrik:awaiting-input",
		"fabrik:awaiting-ci",
		"fabrik:auto-merge-enabled",
		"fabrik:revalidate",
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 42,
						ItemID: "PVTI_42",
						Status: "Validate",
						Repo:   "owner/repo",
						Labels: allLabels,
					},
				},
			}, nil
		},
	}
	eng := testEngineWithStages(client, stgs)

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	removed := make(map[string]bool)
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 42 {
			removed[c.labelName] = true
		}
	}
	for _, lbl := range allLabels {
		if !removed[lbl] {
			t.Errorf("expected label %q to be removed, but it was not", lbl)
		}
	}
}

// TestRevalidate_NonValidateStageWarnsAndRemovesLabel verifies SC-2: applying
// fabrik:revalidate to a non-Validate issue removes only the trigger label and
// dispatches no work.
func TestRevalidate_NonValidateStageWarnsAndRemovesLabel(t *testing.T) {
	stgs := testStagesForRevalidate()
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 99,
						ItemID: "PVTI_99",
						Status: "Implement",
						Repo:   "owner/repo",
						Labels: []string{"fabrik:revalidate"},
					},
				},
			}, nil
		},
	}
	claude := &mockClaudeInvoker{}
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
		NewWorktreeManager("/tmp/test-revalidate-sc2"),
	)
	opts := make(map[string]string)
	for _, s := range stgs {
		opts[s.Name] = "OPT_" + s.Name
	}
	eng.statusField = &gh.StatusField{FieldID: "FIELD_1", Options: opts}

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	client.mu.Lock()
	removed := make(map[string]bool)
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 99 {
			removed[c.labelName] = true
		}
	}
	client.mu.Unlock()

	if !removed["fabrik:revalidate"] {
		t.Error("expected fabrik:revalidate to be removed from non-Validate item")
	}

	// The revalidate scan must not remove any Validate-stage labels.
	for _, lbl := range []string{
		"stage:Validate:complete", "stage:Validate:failed", "fabrik:paused",
		"fabrik:awaiting-input", "fabrik:awaiting-ci", "fabrik:auto-merge-enabled",
	} {
		if removed[lbl] {
			t.Errorf("revalidate scan must not remove %q from a non-Validate issue", lbl)
		}
	}

	// Claude must not be invoked for Validate on this non-Validate issue.
	claude.mu.Lock()
	validateInvoked := false
	for _, c := range claude.calls {
		if c.issueNum == 99 && c.stageName == "Validate" {
			validateInvoked = true
		}
	}
	claude.mu.Unlock()
	if validateInvoked {
		t.Error("Claude must not be invoked for Validate on a non-Validate issue")
	}
}

// TestRevalidate_InFlightWorkerDefersProcessing verifies SC-3: when a Validate
// worker is in-flight, the revalidate scan defers; after the worker exits, the
// next poll removes all labels.
func TestRevalidate_InFlightWorkerDefersProcessing(t *testing.T) {
	stgs := testStagesForRevalidate()
	allLabels := []string{
		"stage:Validate:complete",
		"fabrik:paused",
		"fabrik:revalidate",
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 77,
						ItemID: "PVTI_77",
						Status: "Validate",
						Repo:   "owner/repo",
						Labels: allLabels,
					},
				},
			}, nil
		},
	}
	eng := testEngineWithStages(client, stgs)

	// Simulate an in-flight Validate worker.
	eng.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     77,
		User:       "testuser",
		AcquiredAt: time.Now(),
		Worker:     &itemstate.WorkerHandle{StageName: "Validate", StartedAt: time.Now()},
	})

	// First poll: worker is in-flight — revalidate scan defers, no labels removed.
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll 1: %v", err)
	}

	client.mu.Lock()
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("poll 1: expected no label removals while worker in-flight, got %d", len(client.removeLabelCalls))
	}
	client.mu.Unlock()

	// Simulate worker exiting.
	eng.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 77})

	// Second poll: no worker in store; revalidate scan fires and removes all labels.
	// Item still has fabrik:revalidate in board (mock returns same data), so
	// hasAwaitingLabel bypass ensures it passes the pre-filter.
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll 2: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	removed := make(map[string]bool)
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 77 {
			removed[c.labelName] = true
		}
	}
	for _, lbl := range allLabels {
		if !removed[lbl] {
			t.Errorf("poll 2: expected label %q to be removed after worker exit, but it was not", lbl)
		}
	}
}

// TestRevalidate_ItemNeedsWorkAfterLabelClear verifies SC-4: after the revalidate
// scan fires and clears all blocking labels, itemNeedsWork returns true — meaning
// the dispatch loop would invoke processItem on the next poll.
func TestRevalidate_ItemNeedsWorkAfterLabelClear(t *testing.T) {
	stgs := testStagesForRevalidate()
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 55,
						ItemID: "PVTI_55",
						Status: "Validate",
						Repo:   "owner/repo",
						Labels: []string{
							"stage:Validate:complete",
							"fabrik:paused",
							"fabrik:awaiting-ci",
							"fabrik:revalidate",
						},
					},
				},
			}, nil
		},
	}
	eng := testEngineWithStages(client, stgs)

	// First poll: revalidate scan fires and removes all blocking labels.
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// Verify labels were removed from GitHub.
	client.mu.Lock()
	removedRevalidate := false
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 55 && c.labelName == "fabrik:revalidate" {
			removedRevalidate = true
		}
	}
	client.mu.Unlock()
	if !removedRevalidate {
		t.Fatal("expected fabrik:revalidate to be removed by revalidate scan")
	}

	// Now verify that itemNeedsWork returns true for the cleaned-up item —
	// i.e., the dispatch gate is open. StageLastAttemptCleared was applied to the
	// store by handleRevalidateLabel, so no cooldown suppresses re-dispatch.
	cleanedItem := gh.ProjectItem{
		Number: 55,
		ItemID: "PVTI_55",
		Status: "Validate",
		Repo:   "owner/repo",
		Labels: []string{}, // all blocking labels cleared
	}
	if !eng.itemNeedsWork(cleanedItem) {
		t.Error("expected itemNeedsWork to return true after revalidate scan cleared labels")
	}
}
