package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
)

// TestLabelWriteThrough verifies that successful label mutations are immediately
// reflected in the in-memory cache without waiting for webhook echo or Reconcile.
func TestLabelWriteThrough(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(client, &mockClaudeInvoker{})

	// addFailedLabel → cache should immediately contain "stage:Research:failed"
	eng.addFailedLabel("owner", "repo", 1, "Research")

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if !containsLabel(labels, "stage:Research:failed") {
		t.Errorf("expected 'stage:Research:failed' in cache after addFailedLabel, got %v", labels)
	}

	// removeFailedLabel → cache should immediately drop "stage:Research:failed"
	eng.removeFailedLabel("owner", "repo", 1, "Research")

	labels, err = cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels after remove: %v", err)
	}
	if containsLabel(labels, "stage:Research:failed") {
		t.Errorf("expected 'stage:Research:failed' absent from cache after removeFailedLabel, got %v", labels)
	}
}

// TestLockLabelWriteThrough verifies both the add and remove write-through paths
// for lock-style labels.
func TestLockLabelWriteThrough(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	stage := testStages()[0]

	// Acquisition path: blockOnInput calls AddLabelToIssue for fabrik:paused and
	// fabrik:awaiting-input using the same write-through pattern as processItem's
	// lock acquire (direct AddLabelToIssue → cache.ApplyLabelAdded on success).
	eng.blockOnInput(item, stage)

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels after blockOnInput: %v", err)
	}
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("add write-through: expected fabrik:paused in cache after blockOnInput, got %v", labels)
	}

	// Removal path: seed the lock label, then verify removeLockLabel updates the cache.
	lockLabel := "fabrik:locked:testuser"
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), lockLabel)

	labels, err = cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels before removeLockLabel: %v", err)
	}
	if !containsLabel(labels, lockLabel) {
		t.Fatalf("precondition: expected lock label in cache, got %v", labels)
	}

	eng.removeLockLabel("owner", "repo", 1, lockLabel)

	labels, err = cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels after removeLockLabel: %v", err)
	}
	if containsLabel(labels, lockLabel) {
		t.Errorf("removal write-through: expected lock label absent after removeLockLabel, got %v", labels)
	}
}

// TestCommentWriteThrough verifies that a successful issue comment post is immediately
// reflected in the cache so dispatch can see the new comment without a Reconcile round-trip.
func TestCommentWriteThrough(t *testing.T) {
	client := &mockGitHubClient{
		// Return a fixed database ID so we can verify write-through stored the right comment.
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 9001, nil
		},
	}
	eng, cache := testEngineWithCache(client, &mockClaudeInvoker{})

	// Warm up the deep-fetch cache so FetchItemDetails returns from cache (not fallback).
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	if err := cache.FetchItemDetails(&item); err != nil {
		t.Fatalf("FetchItemDetails warm-up: %v", err)
	}

	// postOutputToPR with no open PR falls back to posting on the issue.
	// No PR → FindPRForIssue returns 0 (mock default).
	eng.postOutputToPR(
		gh.ProjectItem{Number: 1, Repo: "owner/repo", Title: "Test"},
		"Research",
		"Claude output here",
		"",
		"fabrik/issue-1",
		"abc1234",
		"",
		time.Now().Format(time.RFC3339),
	)

	// FetchItemDetails should now return from cache and include the written comment.
	item2 := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	if err := cache.FetchItemDetails(&item2); err != nil {
		t.Fatalf("FetchItemDetails after postOutputToPR: %v", err)
	}
	if !containsCommentByID(item2.Comments, 9001) {
		t.Errorf("expected comment with DatabaseID 9001 in cache, got %v", item2.Comments)
	}
}

// TestWriteThroughOnFailure verifies that a failed GitHub mutation leaves the cache unchanged.
func TestWriteThroughOnFailure(t *testing.T) {
	apiErr := errors.New("github: rate limit exceeded")
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return apiErr
		},
	}
	eng, cache := testEngineWithCache(client, &mockClaudeInvoker{})

	// addFailedLabel will call AddLabelToIssue which returns an error.
	eng.addFailedLabel("owner", "repo", 1, "Implement")

	// Cache should NOT contain the label since the API call failed.
	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if containsLabel(labels, "stage:Implement:failed") {
		t.Errorf("expected cache unchanged on API failure, but found 'stage:Implement:failed' in %v", labels)
	}
}

// TestAdvanceLoopRegression verifies the core advance-loop bug class: after
// advanceToNextStage succeeds, FetchProjectBoard immediately returns the new
// Status — so the catch-up loop on the next poll sees the updated column and
// does not re-dispatch. Without write-through, the cache would still show the
// old Status and the catch-up loop would fire again every 15 seconds.
func TestAdvanceLoopRegression(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(client, &mockClaudeInvoker{})
	eng.statusField = &gh.StatusField{
		FieldID: "FIELD_1",
		Options: map[string]string{
			"Research": "OPT_1",
			"Plan":     "OPT_2",
		},
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_001", Repo: "owner/repo"}

	// Precondition: cache shows item in "Research".
	b0, err := cache.FetchProjectBoard("owner", "repo", 1, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard (before): %v", err)
	}
	if len(b0.Items) == 0 || b0.Items[0].Status != "Research" {
		t.Fatalf("precondition: expected Status=Research, got %+v", b0.Items)
	}

	from := testStages()[0] // Research
	if err := eng.advanceToNextStage(board, item, from); err != nil {
		t.Fatalf("advanceToNextStage: %v", err)
	}

	// The cache must immediately reflect the new Status. If it did not, the
	// catch-up loop would still see Status="Research" and re-advance, looping
	// every poll cycle — the advance-loop bug observed on issues #501 and #506.
	b1, err := cache.FetchProjectBoard("owner", "repo", 1, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard (after): %v", err)
	}
	var found bool
	for _, pi := range b1.Items {
		if pi.Number == 1 {
			found = true
			if pi.Status != "Plan" {
				t.Errorf("advance-loop regression: expected Status=Plan after advanceToNextStage, got %q — stale cache would cause re-dispatch loop", pi.Status)
			}
		}
	}
	if !found {
		t.Error("item not found in FetchProjectBoard result after advance")
	}
}

func containsLabel(labels []string, target string) bool {
	for _, l := range labels {
		if l == target {
			return true
		}
	}
	return false
}

func containsCommentByID(comments []gh.Comment, dbID int) bool {
	for _, c := range comments {
		if c.DatabaseID == dbID {
			return true
		}
	}
	return false
}
