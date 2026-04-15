package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
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

	_, err := eng.poll(context.Background())
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

	_, err := eng.poll(context.Background())
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
	_, err := eng.poll(context.Background())
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

	_, _ = eng.poll(context.Background())
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

	_, _ = eng.poll(context.Background())
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
	if _, err := eng.poll(context.Background()); err != nil {
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

	if _, err := eng.poll(context.Background()); err != nil {
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
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: testStages()},
		client, claude, NewWorktreeManager("/nonexistent"),
	)

	// poll should not return error even when processItem fails
	_, err := eng.poll(context.Background())
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

	if _, err := eng.poll(context.Background()); err != nil {
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

// TestYoloCatchup_AdvancesClosedIssue verifies that the yolo catch-up loop
// DOES advance a closed issue whose current stage is marked complete — this
// is the common "PR merge closes issue sitting in Validate, need to move to
// Done" path. Without this, closed issues get stuck forever.
func TestYoloCatchup_AdvancesClosedIssue(t *testing.T) {
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
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	n := len(client.updateStatusCalls)
	client.mu.Unlock()
	if n != 1 {
		t.Errorf("expected 1 UpdateProjectItemStatus call to advance closed issue, got %d", n)
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
	if _, err := eng.poll(ctx); err != nil {
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

	if _, err := eng.poll(context.Background()); err != nil {
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

// TestArchiveDoneCompleteItems_ArchivesCompleteItems verifies that items in a
// CleanupWorktree stage with the stage:<Name>:complete label are archived.
func TestArchiveDoneCompleteItems_ArchivesCompleteItems(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.Stages = testStagesWithCleanup()

	board := &gh.ProjectBoard{
		ProjectID: "PVT_test",
		Items: []gh.ProjectItem{
			{
				Number:    10,
				ItemID:    "PVTI_10",
				Status:    "Done",
				Labels:    []string{"stage:Done:complete"},
				UpdatedAt: time.Now().Add(-48 * time.Hour), // older than 24h grace period
			},
		},
	}

	eng.archiveDoneCompleteItems(board.ProjectID, board.Items)

	if len(client.archiveProjectItemCalls) != 1 {
		t.Fatalf("expected 1 ArchiveProjectItem call, got %d", len(client.archiveProjectItemCalls))
	}
	got := client.archiveProjectItemCalls[0]
	if got.projectID != "PVT_test" || got.itemID != "PVTI_10" {
		t.Errorf("ArchiveProjectItem(%q, %q), want (PVT_test, PVTI_10)", got.projectID, got.itemID)
	}
}

// TestArchiveDoneCompleteItems_SkipsIncompleteItems verifies that Done items
// without the stage:Done:complete label are not archived.
func TestArchiveDoneCompleteItems_SkipsIncompleteItems(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.Stages = testStagesWithCleanup()

	board := &gh.ProjectBoard{
		ProjectID: "PVT_test",
		Items: []gh.ProjectItem{
			{
				Number: 20,
				ItemID: "PVTI_20",
				Status: "Done",
				Labels: []string{"enhancement"}, // no complete label
			},
		},
	}

	eng.archiveDoneCompleteItems(board.ProjectID, board.Items)

	if len(client.archiveProjectItemCalls) != 0 {
		t.Errorf("expected no ArchiveProjectItem calls for incomplete item, got %d", len(client.archiveProjectItemCalls))
	}
}

// TestYoloCatchUpMergesBeforeAdvance verifies that when an item sits in the
// Validate column with stage:Validate:complete + fabrik:yolo, the catch-up loop
// calls MergePR before calling UpdateProjectItemStatus (advancing to Done).
func TestYoloCatchUpMergesBeforeAdvance(t *testing.T) {
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
						Labels: []string{"stage:Validate:complete", "fabrik:yolo"},
					},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "FIELD_1",
				Options: map[string]string{
					"Research":  "OPT_Research",
					"Plan":      "OPT_Plan",
					"Implement": "OPT_Implement",
					"Validate":  "OPT_Validate",
					"Done":      "OPT_Done",
				},
			}, nil
		},
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 99, nil
		},
	}
	// Set after construction so the closure can reference client.
	client.updateProjectItemStatusFn = func(projectID, itemID, statusFieldID, statusOptionID string) error {
		// Ordering assertion: MergePR must have been called before UpdateProjectItemStatus.
		client.mu.Lock()
		mergedBefore := len(client.mergePRCalls) > 0
		client.mu.Unlock()
		if !mergedBefore {
			t.Error("UpdateProjectItemStatus called before MergePR — ordering violated")
		}
		return nil
	}
	eng := testEngineWithStages(client, testStagesWithValidate())

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	merges := len(client.mergePRCalls)
	advances := len(client.updateStatusCalls)
	client.mu.Unlock()

	if merges == 0 {
		t.Fatal("expected MergePR to be called")
	}
	if advances == 0 {
		t.Fatal("expected UpdateProjectItemStatus to be called after merge")
	}
	if client.mergePRCalls[0].prNumber != 99 {
		t.Errorf("MergePR called with prNumber %d, want 99", client.mergePRCalls[0].prNumber)
	}
}

// TestYoloCatchUpSkipsAdvanceOnMergeError verifies that when MergePR returns an
// error in the catch-up loop, UpdateProjectItemStatus is NOT called (advance is
// skipped).
func TestYoloCatchUpSkipsAdvanceOnMergeError(t *testing.T) {
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
						Labels: []string{"stage:Validate:complete", "fabrik:yolo"},
					},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "FIELD_1",
				Options: map[string]string{
					"Research":  "OPT_Research",
					"Plan":      "OPT_Plan",
					"Implement": "OPT_Implement",
					"Validate":  "OPT_Validate",
					"Done":      "OPT_Done",
				},
			}, nil
		},
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 99, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return gh.ErrNotMergeable
		},
	}
	eng := testEngineWithStages(client, testStagesWithValidate())

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	advances := len(client.updateStatusCalls)
	client.mu.Unlock()

	if advances != 0 {
		t.Errorf("expected no UpdateProjectItemStatus when merge fails, got %d", advances)
	}
}

// TestYoloCatchUpSkipsAdvanceOnUnprocessedComment verifies that when an item has
// unprocessed comments, the catch-up loop does NOT advance the item — leaving it
// for the dispatch loop to handle via processItem (which processes comments first).
func TestYoloCatchUpSkipsAdvanceOnUnprocessedComment(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 42,
						ItemID: "PVTI_42",
						Status: "Implement",
						Repo:   "owner/repo",
						Labels: []string{"stage:Implement:complete", "fabrik:yolo"},
					},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "FIELD_1",
				Options: map[string]string{
					"Research":  "OPT_Research",
					"Plan":      "OPT_Plan",
					"Implement": "OPT_Implement",
					"Validate":  "OPT_Validate",
					"Done":      "OPT_Done",
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.Comments = []gh.Comment{{
				ID:        "C1",
				Body:      "Please reconsider the approach",
				Reactions: nil,
			}}
			return nil
		},
	}
	eng := testEngineWithStages(client, testStagesWithValidate())

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	advances := len(client.updateStatusCalls)
	client.mu.Unlock()

	if advances != 0 {
		t.Errorf("expected no UpdateProjectItemStatus when unprocessed comment exists, got %d", advances)
	}
}

// TestArchiveDoneCompleteItems_SkipsNonCleanupStages verifies that items in
// non-cleanup stages are not archived even if they have complete labels.
func TestArchiveDoneCompleteItems_SkipsNonCleanupStages(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.Stages = testStagesWithCleanup()

	board := &gh.ProjectBoard{
		ProjectID: "PVT_test",
		Items: []gh.ProjectItem{
			{
				Number: 30,
				ItemID: "PVTI_30",
				Status: "Research",
				Labels: []string{"stage:Research:complete"},
			},
		},
	}

	eng.archiveDoneCompleteItems(board.ProjectID, board.Items)

	if len(client.archiveProjectItemCalls) != 0 {
		t.Errorf("expected no ArchiveProjectItem calls for non-cleanup stage, got %d", len(client.archiveProjectItemCalls))
	}
}

// TestCruiseCatchUp_NonValidate_Advances verifies that an item with fabrik:cruise
// sitting in a completed non-Validate stage is advanced by the catch-up loop.
func TestCruiseCatchUp_NonValidate_Advances(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 10,
						ItemID: "PVTI_10",
						Status: "Research",
						Repo:   "owner/repo",
						Labels: []string{"stage:Research:complete", "fabrik:cruise"},
					},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "FIELD_1",
				Options: map[string]string{
					"Research":  "OPT_Research",
					"Plan":      "OPT_Plan",
					"Implement": "OPT_Implement",
					"Validate":  "OPT_Validate",
					"Done":      "OPT_Done",
				},
			}, nil
		},
	}
	eng := testEngineWithStages(client, testStagesWithValidate())

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	advances := len(client.updateStatusCalls)
	merges := len(client.mergePRCalls)
	client.mu.Unlock()

	if advances != 1 {
		t.Errorf("expected 1 advance via cruise catch-up, got %d", advances)
	}
	if merges != 0 {
		t.Errorf("expected no MergePR for non-Validate stage, got %d", merges)
	}
}

// TestCruiseCatchUp_Validate_NoMergeNoAdvance verifies that an item with fabrik:cruise
// sitting at stage:Validate:complete is NOT merged and NOT advanced by the catch-up loop.
func TestCruiseCatchUp_Validate_NoMergeNoAdvance(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 11,
						ItemID: "PVTI_11",
						Status: "Validate",
						Repo:   "owner/repo",
						Labels: []string{"stage:Validate:complete", "fabrik:cruise"},
					},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "FIELD_1",
				Options: map[string]string{
					"Research":  "OPT_Research",
					"Plan":      "OPT_Plan",
					"Implement": "OPT_Implement",
					"Validate":  "OPT_Validate",
					"Done":      "OPT_Done",
				},
			}, nil
		},
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 55, nil
		},
	}
	eng := testEngineWithStages(client, testStagesWithValidate())

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	advances := len(client.updateStatusCalls)
	merges := len(client.mergePRCalls)
	client.mu.Unlock()

	if merges != 0 {
		t.Errorf("expected no MergePR for cruise+Validate catch-up, got %d", merges)
	}
	if advances != 0 {
		t.Errorf("expected no advance for cruise+Validate catch-up, got %d", advances)
	}
}

// TestCruiseCatchUp_BothCruiseAndYolo_YoloWins verifies that when both fabrik:cruise
// and fabrik:yolo are present, yolo takes precedence: the PR is merged and the item
// advances past Validate in the catch-up loop.
func TestCruiseCatchUp_BothCruiseAndYolo_YoloWins(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 12,
						ItemID: "PVTI_12",
						Status: "Validate",
						Repo:   "owner/repo",
						Labels: []string{"stage:Validate:complete", "fabrik:yolo", "fabrik:cruise"},
					},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "FIELD_1",
				Options: map[string]string{
					"Research":  "OPT_Research",
					"Plan":      "OPT_Plan",
					"Implement": "OPT_Implement",
					"Validate":  "OPT_Validate",
					"Done":      "OPT_Done",
				},
			}, nil
		},
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 66, nil
		},
	}
	eng := testEngineWithStages(client, testStagesWithValidate())

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	advances := len(client.updateStatusCalls)
	merges := len(client.mergePRCalls)
	client.mu.Unlock()

	if merges != 1 {
		t.Errorf("expected MergePR to fire when yolo wins over cruise, got %d", merges)
	}
	if advances != 1 {
		t.Errorf("expected advance when yolo wins over cruise, got %d", advances)
	}
}

func TestIdleBackoffMultiplier(t *testing.T) {
	cases := []struct {
		idle time.Duration
		want int
	}{
		{0, 1},
		{2 * time.Minute, 1},
		{4*time.Minute + 59*time.Second, 1},
		{5 * time.Minute, 2},
		{7 * time.Minute, 2},
		{9*time.Minute + 59*time.Second, 2},
		{10 * time.Minute, 4},
		{15 * time.Minute, 4},
		{19*time.Minute + 59*time.Second, 4},
		{20 * time.Minute, 0},
		{60 * time.Minute, 0},
	}
	for _, tc := range cases {
		got := idleBackoffMultiplier(tc.idle)
		if got != tc.want {
			t.Errorf("idleBackoffMultiplier(%v) = %d, want %d", tc.idle, got, tc.want)
		}
	}
}

func TestComputeEffectiveInterval(t *testing.T) {
	base := 30 * time.Second

	cases := []struct {
		name         string
		idle         time.Duration
		rateLimitLow bool
		want         time.Duration
	}{
		{"no idle no rateLimit", 0, false, 30 * time.Second},
		{"3min idle no rateLimit", 3 * time.Minute, false, 30 * time.Second},
		{"6min idle (2x)", 6 * time.Minute, false, 60 * time.Second},
		{"12min idle (4x)", 12 * time.Minute, false, 2 * time.Minute},
		{"25min idle (max)", 25 * time.Minute, false, 5 * time.Minute},
		{"rateLimit only", 0, true, 60 * time.Second},
		{"idle 2x wins over rateLimit 2x", 6 * time.Minute, true, 60 * time.Second},
		{"idle 4x wins over rateLimit 2x", 12 * time.Minute, true, 2 * time.Minute},
		{"rateLimit 2x wins over idle 1x", 3 * time.Minute, true, 60 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeEffectiveInterval(base, tc.idle, tc.rateLimitLow)
			if got != tc.want {
				t.Errorf("computeEffectiveInterval(%v, %v, %v) = %v, want %v",
					base, tc.idle, tc.rateLimitLow, got, tc.want)
			}
		})
	}
}

func TestComputeEffectiveInterval_CapAt5Min(t *testing.T) {
	// With a large configured interval (e.g. 3 minutes), 4x would be 12min,
	// but we cap at 5 minutes.
	base := 3 * time.Minute
	got := computeEffectiveInterval(base, 12*time.Minute, false)
	if got != 5*time.Minute {
		t.Errorf("expected cap at 5m, got %v", got)
	}

	// Even 2x of 3 minutes (= 6min) should cap at 5min.
	got = computeEffectiveInterval(base, 6*time.Minute, false)
	if got != 5*time.Minute {
		t.Errorf("expected cap at 5m for 2x of 3min base, got %v", got)
	}
}

func TestComputeEffectiveInterval_MaxIdleRateLimit(t *testing.T) {
	// Both backoffs active: idle at max (5min) and rate limit at 2x.
	// max(5min, 2*30s=1min) = 5min.
	base := 30 * time.Second
	got := computeEffectiveInterval(base, 25*time.Minute, true)
	if got != 5*time.Minute {
		t.Errorf("expected 5m, got %v", got)
	}
}

func TestComputeEffectiveInterval_RateLimitExceeds5Min(t *testing.T) {
	// Rate-limit backoff alone can exceed 5 minutes (the idle cap doesn't apply).
	// With 3min base and rateLimitLow=true, rate-limit interval = 6min.
	// Idle is not active (0 duration), so idleInterval = 3min.
	// max(3min, 6min) = 6min — the 5min idle cap must NOT clamp this.
	base := 3 * time.Minute
	got := computeEffectiveInterval(base, 0, true)
	if got != 6*time.Minute {
		t.Errorf("expected 6m (rate-limit 2x of 3min base), got %v", got)
	}
}
