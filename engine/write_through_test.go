package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestLabelWriteThrough verifies that successful label mutations are immediately
// reflected in the in-memory cache without waiting for webhook echo or Reconcile.
func TestLabelWriteThrough(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})

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
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	stage := testStages()[0]

	// Acquisition path: blockOnInput calls AddLabelToIssue for fabrik:paused and
	// fabrik:awaiting-input using the same write-through pattern as processItem's
	// lock acquire (direct AddLabelToIssue → cache.ApplyLabelAdded on success).
	eng.blockOnInput(item, stage, "")

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels after blockOnInput: %v", err)
	}
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("add write-through: expected fabrik:paused in cache after blockOnInput, got %v", labels)
	}

	// blockOnInput should also post exactly one notification comment.
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 AddComment call from blockOnInput, got %d", len(client.addCommentCalls))
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
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})

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
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})

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
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
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

// ── fix #2 regression tests (issue #1024) ───────────────────────────────────
//
// Several engine/item.go call sites keyed the board-cache write-through on the
// raw item.Repo field while the adjacent webhook-echo registration correctly
// used the resolved owner+"/"+repo. Since item.Repo is empty for default-repo
// items, the two beats produced different cache keys and the write-through
// silently landed on a key nothing ever reads. Every item below is constructed
// with an empty Repo (relying on defaultRepo() fallback) so the pre-fix bug is
// actually observable — a test using an explicit non-empty item.Repo would not
// catch this.

func TestEscalatePRCreationFailure_CacheKeyUsesResolvedRepo(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	item := gh.ProjectItem{Number: 1} // empty Repo — must fall back to defaultRepo() "owner/repo"
	stage := &stages.Stage{Name: "Implement", Order: 3, Prompt: "implement"}

	eng.escalatePRCreationFailure(item, stage, "main")

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("expected fabrik:paused to be write-through applied to the cache under the resolved owner/repo key, got %v", labels)
	}
}

func TestEscalateFailedStage_CacheKeyUsesResolvedRepo(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Implement", Order: 3, Prompt: "implement"}

	eng.escalateFailedStage(item, stage)

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("expected fabrik:paused to be write-through applied to the cache under the resolved owner/repo key, got %v", labels)
	}
}

func TestBlockOnInputAndUnblock_CacheKeyUsesResolvedRepo(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Implement", Order: 3, Prompt: "implement"}

	eng.blockOnInput(item, stage, "need input please")

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("expected fabrik:paused to be write-through applied to the cache, got %v", labels)
	}
	if !containsLabel(labels, "fabrik:awaiting-input") {
		t.Errorf("expected fabrik:awaiting-input to be write-through applied to the cache, got %v", labels)
	}

	// unblockAwaitingInput removes both labels; verify the removal also lands on
	// the resolved owner/repo key (not item.Repo) so the cache actually clears.
	eng.unblockAwaitingInput(item, stage)

	labels, err = cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if containsLabel(labels, "fabrik:paused") || containsLabel(labels, "fabrik:awaiting-input") {
		t.Errorf("expected pause labels removed from the cache, got %v", labels)
	}
}

func TestHandleBoundaryViolation_CacheKeyUsesResolvedRepo(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Implement", Order: 3, Prompt: "implement"}

	eng.handleBoundaryViolation("owner", "repo", "owner/repo", item, stage, []string{"pushed to origin/main"}, func() {})

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("expected fabrik:paused to be write-through applied to the cache under the resolved owner/repo key, got %v", labels)
	}
}

// ── fix #3 regression test (issue #1024) ────────────────────────────────────
//
// addPausedLabelToItem (spawn.go) did the label add + cache write-through but
// never called RegisterEcho, so a stale inbound webhook could re-clear the
// cached paused state. It also had the same item.Repo-vs-resolved-owner/repo
// key mismatch as fix #2, fixed in the same edit since both live in the exact
// same few lines.
func TestAddPausedLabelToItem_CacheKeyAndEcho(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	item := gh.ProjectItem{Number: 1} // empty Repo — must fall back to the passed-in owner/repo

	eng.addPausedLabelToItem("owner", "repo", item)

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("expected fabrik:paused to be write-through applied to the cache under the resolved owner/repo key, got %v", labels)
	}

	wm.mu.Lock()
	_, gotEcho := wm.pendingEchoes[echoKey("issues", "labeled", boardcache.ItemKey("owner/repo", 1)+"+"+"fabrik:paused")]
	wm.mu.Unlock()
	if !gotEcho {
		t.Error("expected fabrik:paused webhook echo to be registered")
	}
}
