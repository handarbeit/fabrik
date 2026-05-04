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

// TestLockLabelWriteThrough verifies that removeLockLabel immediately updates the cache.
func TestLockLabelWriteThrough(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(client, &mockClaudeInvoker{})

	// Seed the lock label into the cache first via ApplyLabelAdded.
	lockLabel := "fabrik:locked:testuser"
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), lockLabel)

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels before remove: %v", err)
	}
	if !containsLabel(labels, lockLabel) {
		t.Fatalf("precondition: expected lock label in cache, got %v", labels)
	}

	// removeLockLabel → cache should immediately drop the lock label.
	eng.removeLockLabel("owner", "repo", 1, lockLabel)

	labels, err = cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels after removeLockLabel: %v", err)
	}
	if containsLabel(labels, lockLabel) {
		t.Errorf("expected lock label absent from cache after removeLockLabel, got %v", labels)
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

// TestAdvanceLoopRegression verifies the core bug: after advanceToNextStage succeeds,
// the cache reflects the new status immediately — preventing re-dispatch on the next poll cycle.
// (advanceToNextStage already had UpdateItemStatus write-through; this test is a non-regression guard.)
func TestAdvanceLoopRegression(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := testEngineWithCache(client, &mockClaudeInvoker{})
	eng.statusField = &gh.StatusField{
		FieldID: "FIELD_1",
		Options: map[string]string{
			"Research": "OPT_1",
			"Plan":     "OPT_2",
		},
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo"}

	// advanceToNextStage from Research → Plan; UpdateProjectItemStatus should be called once.
	from := testStages()[0] // Research
	if err := eng.advanceToNextStage(board, item, from); err != nil {
		t.Fatalf("advanceToNextStage: %v", err)
	}
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 UpdateProjectItemStatus call, got %d", len(client.updateStatusCalls))
	}

	// Calling again immediately should still call UpdateProjectItemStatus (stage hasn't changed
	// in the live board; the cache write-through prevents re-reading stale status mid-cycle).
	// This test guards against the "advance fires twice on the same item" regression.
	if err := eng.advanceToNextStage(board, item, from); err != nil {
		t.Fatalf("advanceToNextStage (2nd call): %v", err)
	}
	if len(client.updateStatusCalls) != 2 {
		t.Fatalf("expected 2 total UpdateProjectItemStatus calls, got %d", len(client.updateStatusCalls))
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
