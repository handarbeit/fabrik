package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

func testStagesForSHAInvalidation() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Implement", Order: 3, Prompt: "implement"},
		{Name: "Validate", Order: 4, Prompt: "validate"},
	}
}

// shaInvalidationBoardFn builds a fetchProjectBoardFn that returns a single
// Validate-status item with the given labels.
func shaInvalidationBoardFn(itemNum int, labels []string) func(string, string, int, string) (*gh.ProjectBoard, error) {
	return func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
		return &gh.ProjectBoard{
			ProjectID: "PVT_1",
			Items: []gh.ProjectItem{
				{
					Number: itemNum,
					ItemID: fmt.Sprintf("PVTI_%d", itemNum),
					Status: "Validate",
					Repo:   "owner/repo",
					Labels: labels,
				},
			},
		}, nil
	}
}

// TestSHAInvalidation_ClearsLabelsOnSHAMismatch verifies SC-1: when an item
// carries stage:Validate:complete, a recorded completion SHA ("sha-N"), and the
// linked PR's current HEAD SHA is "sha-M" (different), the SHA-invalidation
// scan removes all four FR-3 labels from GitHub.
func TestSHAInvalidation_ClearsLabelsOnSHAMismatch(t *testing.T) {
	stgs := testStagesForSHAInvalidation()
	allLabels := []string{
		"stage:Validate:complete",
		"fabrik:auto-merge-enabled",
		"fabrik:awaiting-ci",
		"fabrik:awaiting-review",
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: shaInvalidationBoardFn(42, allLabels),
	}
	eng := testEngineWithStages(t, client, stgs)

	// Record current HEAD SHA = "sha-M" and completion SHA = "sha-N" (mismatch).
	eng.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        "owner/repo",
		Number:      42,
		LinkedPRNum: 101,
		SHA:         "sha-M",
	})
	eng.store.Apply(itemstate.ValidateCompletedAtSHA{
		Repo:   "owner/repo",
		Number: 42,
		SHA:    "sha-N",
	})

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
			t.Errorf("expected label %q to be removed on SHA mismatch, but it was not", lbl)
		}
	}
}

// TestSHAInvalidation_NoopOnSameSHA verifies SC-2 convergence: when the linked
// PR's HEAD SHA equals the recorded completion SHA, the scan is a no-op.
func TestSHAInvalidation_NoopOnSameSHA(t *testing.T) {
	stgs := testStagesForSHAInvalidation()
	labels := []string{
		"stage:Validate:complete",
		"fabrik:auto-merge-enabled",
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: shaInvalidationBoardFn(43, labels),
	}
	eng := testEngineWithStages(t, client, stgs)

	// Same SHA in both fields — no mismatch.
	eng.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        "owner/repo",
		Number:      43,
		LinkedPRNum: 102,
		SHA:         "sha-N",
	})
	eng.store.Apply(itemstate.ValidateCompletedAtSHA{
		Repo:   "owner/repo",
		Number: 43,
		SHA:    "sha-N",
	})

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 43 {
			t.Errorf("expected no label removals when SHA matches completion SHA, got removal of %q", c.labelName)
		}
	}
}

// TestSHAInvalidation_NoopOnEmptySHA verifies SC-3 (FR-5): when
// stage:Validate:complete is present but ValidateCompletedSHA is empty
// (pre-feature or worktree-HEAD-unavailable item), the scan leaves the item
// untouched even when the linked PR's HEAD SHA changes.
func TestSHAInvalidation_NoopOnEmptySHA(t *testing.T) {
	stgs := testStagesForSHAInvalidation()
	labels := []string{
		"stage:Validate:complete",
		"fabrik:auto-merge-enabled",
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: shaInvalidationBoardFn(44, labels),
	}
	eng := testEngineWithStages(t, client, stgs)

	// Set HeadSHA but apply no ValidateCompletedAtSHA mutation → completion SHA is "".
	eng.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        "owner/repo",
		Number:      44,
		LinkedPRNum: 103,
		SHA:         "sha-M",
	})

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 44 {
			t.Errorf("expected no label removals for legacy item with empty completion SHA, got removal of %q", c.labelName)
		}
	}
}

// TestSHAInvalidation_NoopWhenWorkerInFlight verifies SC-4 (FR-6): when a
// Validate worker is registered in the store, the scan defers and makes no
// label changes, even when a SHA mismatch is present.
func TestSHAInvalidation_NoopWhenWorkerInFlight(t *testing.T) {
	stgs := testStagesForSHAInvalidation()
	labels := []string{
		"stage:Validate:complete",
		"fabrik:auto-merge-enabled",
		"fabrik:awaiting-ci",
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: shaInvalidationBoardFn(45, labels),
	}
	eng := testEngineWithStages(t, client, stgs)

	// SHA mismatch would normally trigger invalidation...
	eng.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        "owner/repo",
		Number:      45,
		LinkedPRNum: 104,
		SHA:         "sha-M",
	})
	eng.store.Apply(itemstate.ValidateCompletedAtSHA{
		Repo:   "owner/repo",
		Number: 45,
		SHA:    "sha-N",
	})
	// ...but a Validate worker is in-flight (FR-6 guard).
	eng.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     45,
		User:       "testuser",
		AcquiredAt: time.Now(),
		Worker:     &itemstate.WorkerHandle{StageName: "Validate", StartedAt: time.Now()},
	})

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 45 {
			t.Errorf("expected no label removals while Validate worker in-flight, got removal of %q", c.labelName)
		}
	}
}
