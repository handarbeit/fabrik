package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

func TestPoll_FetchesBoardAndProcessesItems(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{Number: 1, Title: "Test", Status: "Unknown"},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "F1",
				Options: map[string]string{"Research": "OPT_1"},
			}, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	err := eng.poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	// Status field should be fetched
	if eng.statusField == nil {
		t.Error("statusField should be set after poll")
	}
}

func TestPoll_Error(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return nil, fmt.Errorf("network error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	err := eng.poll(context.Background())
	if err == nil {
		t.Fatal("expected error from poll")
	}
}

func TestPoll_StatusFieldFetchError(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: "PVT_1", Items: nil}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return nil, fmt.Errorf("status field error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	// Should not error — status field failure is a warning
	err := eng.poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if eng.statusField != nil {
		t.Error("statusField should remain nil on fetch error")
	}
}

func TestPoll_StatusFieldAlreadySet(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: "PVT_1"}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			t.Error("should not fetch status field again")
			return nil, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.statusField = &gh.StatusField{FieldID: "already-set"}

	eng.poll(context.Background())
}

func TestPoll_EmptyProjectID(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: ""}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			t.Error("should not fetch status field when projectID is empty")
			return nil, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	eng.poll(context.Background())
}

func TestPoll_RateLimitLogging(t *testing.T) {
	resetTime := time.Now().Add(time.Hour)
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: "PVT_1"}, nil
		},
		rateLimitStatsFn: func() (gh.RateLimitStats, gh.RateLimitStats) {
			rest := gh.RateLimitStats{Limit: 5000, Remaining: 4800, Used: 200, Reset: resetTime}
			gql := gh.RateLimitStats{Limit: 5000, Remaining: 4950, Used: 50, Reset: resetTime}
			return rest, gql
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	// poll() must succeed and not panic when rate limit stats are non-zero.
	if err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

func TestPoll_RateLimitLogging_ZeroReset(t *testing.T) {
	// Verify poll() handles a zero Reset (header absent) gracefully — no panic, no "00:00 UTC".
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: "PVT_1"}, nil
		},
		rateLimitStatsFn: func() (gh.RateLimitStats, gh.RateLimitStats) {
			rest := gh.RateLimitStats{Limit: 60, Remaining: 0} // Reset is zero
			return rest, gh.RateLimitStats{}
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	if err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

func TestPoll_ProcessItemError(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{Number: 1, Title: "Test", Status: "Research", ItemID: "PVTI_1"},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{FieldID: "F1", Options: map[string]string{}}, nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: testStages()},
		client, claude, NewWorktreeManager("/nonexistent"),
	)

	// poll should not return error even when processItem fails
	err := eng.poll(context.Background())
	if err != nil {
		t.Fatalf("poll should not error from processItem failures: %v", err)
	}
	eng.wg.Wait()
}

// TestPoll_CleanupStageItemNotDeepFetched verifies that items in cleanup stages
// are never passed to FetchItemDetails even when itemMayNeedWork returns true
// (i.e. a worktree directory exists for the item).
func TestPoll_CleanupStageItemNotDeepFetched(t *testing.T) {
	var fetchDetailsCalled bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{Number: 42, Title: "Old done item", Status: "Done"},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{FieldID: "F1", Options: map[string]string{}}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			fetchDetailsCalled = true
			return nil
		},
	}

	// Create a real worktree directory so itemMayNeedWork's os.Stat check passes.
	rootDir := t.TempDir()
	worktreeDir := filepath.Join(rootDir, "issue-42")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}
	wm := NewWorktreeManagerWithRoot(t.TempDir(), rootDir)

	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 1,
			Stages:        testStagesWithCleanup(),
		},
		client,
		&mockClaudeInvoker{},
		wm,
	)

	if err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	if fetchDetailsCalled {
		t.Error("FetchItemDetails must not be called for cleanup-stage items")
	}
}

// TestItemNeedsWork_CleanupStage_NoWorktree verifies that itemNeedsWork returns
// false for a cleanup-stage item when no worktree directory exists for the issue.
// This guards against the "repeating Done cleanup loop" where items with no
// worktree get repeatedly dispatched because the worktree guard was missing.
func TestItemNeedsWork_CleanupStage_NoWorktree(t *testing.T) {
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 1,
			Stages:        testStagesWithCleanup(),
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		// WorktreeManager points to a temp dir with no issue-42 subdirectory.
		NewWorktreeManagerWithRoot(t.TempDir(), t.TempDir()),
	)

	item := gh.ProjectItem{
		Number: 42,
		Title:  "Old done item",
		Status: "Done",
		// No labels — no stage:Done:complete, no fabrik:paused
	}

	if eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork must return false for cleanup item with no worktree directory")
	}
}

// TestCleanupClosedIssueLocks_RemovesLockFromClosedIssue verifies that a
// closed issue with fabrik:locked:<user> gets the lock label removed.
func TestCleanupClosedIssueLocks_RemovesLockFromClosedIssue(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number:   42,
				IsClosed: true,
				Labels:   []string{"fabrik:locked:testuser"},
			},
		},
	}

	eng.cleanupClosedIssueLocks(board)

	if len(client.removeLabelCalls) != 1 {
		t.Fatalf("expected 1 RemoveLabelFromIssue call, got %d", len(client.removeLabelCalls))
	}
	call := client.removeLabelCalls[0]
	if call.issueNumber != 42 {
		t.Errorf("issueNumber = %d, want 42", call.issueNumber)
	}
	if call.labelName != "fabrik:locked:testuser" {
		t.Errorf("labelName = %q, want %q", call.labelName, "fabrik:locked:testuser")
	}
}

// TestCleanupClosedIssueLocks_IgnoresOpenIssues verifies that open issues
// with a lock label are left untouched.
func TestCleanupClosedIssueLocks_IgnoresOpenIssues(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number:   10,
				IsClosed: false,
				Labels:   []string{"fabrik:locked:testuser"},
			},
		},
	}

	eng.cleanupClosedIssueLocks(board)

	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no RemoveLabelFromIssue calls for open issue, got %d", len(client.removeLabelCalls))
	}
}

// TestCleanupClosedIssueLocks_IgnoresOtherUsersLocks verifies that lock labels
// belonging to other users are not removed.
func TestCleanupClosedIssueLocks_IgnoresOtherUsersLocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number:   55,
				IsClosed: true,
				Labels:   []string{"fabrik:locked:otheruser"},
			},
		},
	}

	eng.cleanupClosedIssueLocks(board)

	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no RemoveLabelFromIssue calls for other user's lock, got %d", len(client.removeLabelCalls))
	}
}

// TestCleanupClosedIssueLocks_NoLock verifies that a closed issue without
// any lock label produces no API call.
func TestCleanupClosedIssueLocks_NoLock(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number:   7,
				IsClosed: true,
				Labels:   []string{"some-other-label"},
			},
		},
	}

	eng.cleanupClosedIssueLocks(board)

	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no RemoveLabelFromIssue calls when no lock label, got %d", len(client.removeLabelCalls))
	}
}

// TestYoloCatchup_SkipsClosedIssue verifies that the yolo catch-up loop does
// not call UpdateProjectItemStatus for a closed issue that has a stage-complete
// label, even when yolo mode is active.
func TestYoloCatchup_SkipsClosedIssue(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number:   77,
						ItemID:   "PVTI_77",
						Status:   "Research",
						IsClosed: true,
						Labels:   []string{"stage:Research:complete", "fabrik:yolo"},
					},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "FIELD_1",
				Options: map[string]string{"Research": "OPT_R", "Plan": "OPT_P"},
			}, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.Yolo = true

	ctx := context.Background()
	if err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	n := len(client.updateStatusCalls)
	client.mu.Unlock()
	if n != 0 {
		t.Errorf("expected no UpdateProjectItemStatus calls for closed issue, got %d", n)
	}
}

// TestYoloCatchup_SkipsNotDeepFetched verifies that the yolo catch-up loop does
// not advance an item that was not deep-fetched this poll cycle. This enforces
// the "shallow = filter only, never act" principle (ADR 017): items skipped by
// itemMayNeedWork are not in deepFetchedIDs and must not be mutated.
func TestYoloCatchup_SkipsNotDeepFetched(t *testing.T) {
	fixedTime := time.Now().Add(-time.Hour)
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number:    55,
						ItemID:    "PVTI_55",
						Status:    "Research",
						Repo:      "owner/repo",
						UpdatedAt: fixedTime,
						Labels:    []string{"stage:Research:complete", "fabrik:yolo"},
					},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "FIELD_1",
				Options: map[string]string{"Research": "OPT_R", "Plan": "OPT_P"},
			}, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.Yolo = true
	// Pre-seed lastUpdatedAt so itemMayNeedWork sees this item as unchanged →
	// no deep-fetch → not in deepFetchedIDs → yolo catch-up must skip it.
	eng.lastUpdatedAt["owner/repo#55"] = fixedTime

	ctx := context.Background()
	if err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	n := len(client.updateStatusCalls)
	client.mu.Unlock()
	if n != 0 {
		t.Errorf("expected no UpdateProjectItemStatus calls for non-deep-fetched item, got %d", n)
	}
}

// TestProcessedSetConcurrency verifies that concurrent access to processedSet
// via the mutex-protected methods does not cause data races.

// TestPoll_RateLimitWarning verifies that a distinct warning is logged when the
// GraphQL remaining/limit ratio falls below rateLimitBackoffThreshold (20%).
func TestPoll_RateLimitWarning(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: "PVT_1", Items: nil}, nil
		},
		// Remaining=100, Limit=1000 → 10%, below 20% threshold
		rateLimitStatsFn: func() (gh.RateLimitStats, gh.RateLimitStats) {
			gql := gh.RateLimitStats{Limit: 1000, Remaining: 100}
			return gh.RateLimitStats{}, gql
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	// Use events channel to capture log output without hitting stdout.
	events := make(chan tui.Event, 64)
	eng.events = events

	if err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	close(events)

	var warnSeen bool
	for ev := range events {
		le, ok := ev.(tui.LogEvent)
		if !ok {
			continue
		}
		if le.Tag == "warn" && strings.Contains(le.Message, "rate limit") {
			warnSeen = true
		}
	}
	if !warnSeen {
		t.Error("expected a warn log event about GraphQL rate limit, but none was found")
	}
}
