package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
	"github.com/handarbeit/fabrik/warnings"
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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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
	eng := testEngine(t, client, &mockClaudeInvoker{})
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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.Yolo = true
	// Seed item into cycleSet so the pre-filter admits it (simulates observer firing).
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#77"] = true
	eng.mayNeedWorkMu.Unlock()

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
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.Yolo = true
	// Seed the store so the pre-filter sees this as a known (previously-processed) item.
	eng.store.Apply(itemstate.InvocationRecorded{Repo: "owner/repo", Number: 55, Completed: true})
	// Not in cycleSet (mayNeedWork is empty), in store but no CooldownAt →
	// no deep-fetch → not in deepFetchedIDs → yolo catch-up must skip it.

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

// TestProcessedSetConcurrency verifies that concurrent access to item state
// via the store does not cause data races.

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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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
	eng := testEngine(t, client, &mockClaudeInvoker{})
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

// ── Bug A: catch-up loop review-gate guard (#617) ────────────────────────────

// TestCatchupLoop_SkipsReviewGate_WhenAwaitingCIWithoutComplete verifies that
// checkReviewGate is NOT invoked when an item is in the CI-await window
// (fabrik:awaiting-ci present, stage:X:complete absent). Without the guard,
// stale board data could re-apply fabrik:awaiting-review even though Review
// already cleared it (issue #617, Bug A).
func TestCatchupLoop_SkipsReviewGate_WhenAwaitingCIWithoutComplete(t *testing.T) {
	trueVal := true
	stgs := []*stages.Stage{
		{Name: "Validate", Order: 1, Prompt: "validate", WaitForCI: &trueVal, WaitForReviews: &trueVal},
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 10,
						ItemID: "PVTI_10",
						Status: "Validate",
						Repo:   "owner/repo",
						Labels: []string{"fabrik:awaiting-ci"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			// Simulate an outstanding review request. If checkReviewGate were
			// called, it would add fabrik:awaiting-review (gate blocks with no
			// reviews submitted). The guard must prevent this call.
			item.LinkedPRReviewRequests = []gh.ReviewRequest{{Login: "copilot-pull-request-reviewer", IsBot: true}}
			return nil
		},
		// No linked PR → CI gate clears immediately (R5, no CI configured).
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-review" {
			t.Errorf("checkReviewGate must not run during CI-await window: spuriously added fabrik:awaiting-review")
		}
	}
}

// TestCatchupLoop_RunsReviewGate_WhenHasComplete verifies that checkReviewGate
// IS invoked when an item has stage:X:complete (i.e., after the CI gate has
// already cleared). Outstanding reviewers must still block advancement in that
// path, confirming the guard does not permanently disable the review gate.
func TestCatchupLoop_RunsReviewGate_WhenHasComplete(t *testing.T) {
	trueVal := true
	stgs := []*stages.Stage{
		{Name: "Validate", Order: 1, Prompt: "validate", WaitForCI: &trueVal, WaitForReviews: &trueVal},
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 20,
						ItemID: "PVTI_20",
						Status: "Validate",
						Repo:   "owner/repo",
						Labels: []string{"stage:Validate:complete"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRReviewRequests = []gh.ReviewRequest{{Login: "copilot-pull-request-reviewer", IsBot: true}}
			return nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-review" {
			found = true
		}
	}
	if !found {
		t.Error("expected checkReviewGate to add fabrik:awaiting-review for item with stage:Validate:complete and outstanding reviewers")
	}
}

// ── Bug B: transient label sweep on closed issues (#617) ─────────────────────

// TestCleanupClosedIssueTransientLabels_RemovesAllTransientLabels verifies that
// each of the five transient lifecycle labels is removed from a closed issue.
func TestCleanupClosedIssueTransientLabels_RemovesAllTransientLabels(t *testing.T) {
	for _, label := range transientLifecycleLabels {
		label := label
		t.Run(label, func(t *testing.T) {
			client := &mockGitHubClient{}
			eng := testEngine(t, client, &mockClaudeInvoker{})

			board := &gh.ProjectBoard{
				Items: []gh.ProjectItem{
					{
						Number:   99,
						IsClosed: true,
						Labels:   []string{label, "some-other-label"},
					},
				},
			}

			eng.cleanupClosedIssueTransientLabels(board)

			if len(client.removeLabelCalls) != 1 {
				t.Fatalf("expected 1 RemoveLabelFromIssue call for %q, got %d", label, len(client.removeLabelCalls))
			}
			if got := client.removeLabelCalls[0].labelName; got != label {
				t.Errorf("labelName = %q, want %q", got, label)
			}
			if got := client.removeLabelCalls[0].issueNumber; got != 99 {
				t.Errorf("issueNumber = %d, want 99", got)
			}
		})
	}
}

// TestCleanupClosedIssueTransientLabels_SkipsOpenIssues verifies that open
// issues with transient labels are left untouched.
func TestCleanupClosedIssueTransientLabels_SkipsOpenIssues(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number:   5,
				IsClosed: false,
				Labels:   []string{"fabrik:awaiting-review", "fabrik:awaiting-ci"},
			},
		},
	}

	eng.cleanupClosedIssueTransientLabels(board)

	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no RemoveLabelFromIssue calls for open issue, got %d", len(client.removeLabelCalls))
	}
}

// TestCleanupClosedIssueTransientLabels_SkipsCleanIssues verifies that closed
// issues without any transient labels produce no API call.
func TestCleanupClosedIssueTransientLabels_SkipsCleanIssues(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number:   7,
				IsClosed: true,
				Labels:   []string{"bug", "stage:Validate:complete"},
			},
		},
	}

	eng.cleanupClosedIssueTransientLabels(board)

	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no RemoveLabelFromIssue calls for clean closed issue, got %d", len(client.removeLabelCalls))
	}
}

// TestCleanupClosedIssueTransientLabels_ErrNotFoundIsIdempotent verifies that
// an ErrNotFound response from RemoveLabelFromIssue is treated as success
// (idempotent — the label was already absent).
func TestCleanupClosedIssueTransientLabels_ErrNotFoundIsIdempotent(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return gh.ErrNotFound
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number:   11,
				IsClosed: true,
				Labels:   []string{"fabrik:awaiting-review"},
			},
		},
	}

	eng.cleanupClosedIssueTransientLabels(board)

	// Should have attempted one removal call (ErrNotFound is OK, not a panic).
	if len(client.removeLabelCalls) != 1 {
		t.Errorf("expected 1 RemoveLabelFromIssue call, got %d", len(client.removeLabelCalls))
	}
}

// TestCleanupClosedIssueTransientLabels_APIErrorContinues verifies that a
// non-ErrNotFound API error is logged as a warning and processing continues
// to the next label / issue without returning an error.
func TestCleanupClosedIssueTransientLabels_APIErrorContinues(t *testing.T) {
	var removeCallCount int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			removeCallCount++
			return fmt.Errorf("simulated API error")
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number:   22,
				IsClosed: true,
				Labels:   []string{"fabrik:awaiting-review", "fabrik:awaiting-ci"},
			},
		},
	}

	eng.cleanupClosedIssueTransientLabels(board)

	// Both labels should have been attempted despite the API error on the first.
	if removeCallCount != 2 {
		t.Errorf("expected 2 RemoveLabelFromIssue calls (one per transient label present), got %d", removeCallCount)
	}
}

// TestArchiveDoneCompleteItems_SkipsIncompleteItems verifies that Done items
// without the stage:Done:complete label are not archived.
func TestArchiveDoneCompleteItems_SkipsIncompleteItems(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
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

// TestYoloCatchUpEnablesAutoMerge verifies that when an item sits in the
// Validate column with stage:Validate:complete + fabrik:yolo, the catch-up loop
// calls EnablePullRequestAutoMerge (not MergePR) and does NOT immediately advance
// to Done (advancement is deferred to checkAutoMergeConvergence).
func TestYoloCatchUpEnablesAutoMerge(t *testing.T) {
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
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "sha1"}, nil
		},
	}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	// Seed item into cycleSet so the pre-filter admits it (simulates observer firing).
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#42"] = true
	eng.mayNeedWorkMu.Unlock()

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	autoMergeCalls := len(client.enablePullRequestAutoMergeCalls)
	merges := len(client.mergePRCalls)
	advances := len(client.updateStatusCalls)
	client.mu.Unlock()

	if autoMergeCalls == 0 {
		t.Fatal("expected EnablePullRequestAutoMerge to be called")
	}
	if merges != 0 {
		t.Errorf("MergePR must not be called in the new auto-merge path, got %d", merges)
	}
	// Done advancement is deferred to checkAutoMergeConvergence, not immediate.
	if advances != 0 {
		t.Errorf("expected no immediate advance (deferred to convergence monitor), got %d", advances)
	}
	if client.enablePullRequestAutoMergeCalls[0].prNumber != 99 {
		t.Errorf("EnablePullRequestAutoMerge called with prNumber %d, want 99", client.enablePullRequestAutoMergeCalls[0].prNumber)
	}
}

// TestYoloCatchUpSkipsAdvanceOnAutoMergeError verifies that when
// EnablePullRequestAutoMerge returns an error in the catch-up loop,
// UpdateProjectItemStatus is NOT called (advance is skipped) and the engine
// does not dispatch a rebase or pause the issue — it simply retries next poll.
func TestYoloCatchUpSkipsAdvanceOnAutoMergeError(t *testing.T) {
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
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "sha1"}, nil
		},
		enablePullRequestAutoMergeFn: func(owner, repo string, prNumber int, strategy string) error {
			return errors.New("transient API error")
		},
	}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	// Seed item into cycleSet so the pre-filter admits it (simulates observer firing).
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#42"] = true
	eng.mayNeedWorkMu.Unlock()

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	advances := len(client.updateStatusCalls)
	client.mu.Unlock()

	if advances != 0 {
		t.Errorf("expected no UpdateProjectItemStatus when auto-merge enablement fails, got %d", advances)
	}
	// No rebase dispatch — error path in new auto-merge flow just logs and retries.
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must NOT be added for transient auto-merge enablement error")
		}
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("fabrik:rebase-needed must NOT be added for auto-merge enablement error")
		}
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
	eng := testEngineWithStages(t, client, testStagesWithValidate())

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
	eng := testEngine(t, client, &mockClaudeInvoker{})
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
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	// Seed item into cycleSet so the pre-filter admits it (simulates observer firing).
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#10"] = true
	eng.mayNeedWorkMu.Unlock()

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
	eng := testEngineWithStages(t, client, testStagesWithValidate())

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

// TestCruiseCatchUp_BothCruiseAndYolo_CruiseWins verifies that when both fabrik:cruise
// and fabrik:yolo are present, cruise takes precedence at Validate: neither
// EnablePullRequestAutoMerge nor MergePR is called and the item is not advanced
// (FR-003, FR-015: cruise leaves the PR for human merge).
func TestCruiseCatchUp_BothCruiseAndYolo_CruiseWins(t *testing.T) {
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
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 66, HeadSHA: "sha1"}, nil
		},
	}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	// Seed item into cycleSet so the pre-filter admits it (simulates observer firing).
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#12"] = true
	eng.mayNeedWorkMu.Unlock()

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	advances := len(client.updateStatusCalls)
	merges := len(client.mergePRCalls)
	autoMergeCalls := len(client.enablePullRequestAutoMergeCalls)
	client.mu.Unlock()

	if merges != 0 {
		t.Errorf("expected no MergePR when cruise wins over yolo, got %d", merges)
	}
	if autoMergeCalls != 0 {
		t.Errorf("expected no EnablePullRequestAutoMerge when cruise wins over yolo, got %d", autoMergeCalls)
	}
	if advances != 0 {
		t.Errorf("expected no advance when cruise wins at Validate, got %d", advances)
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
		name           string
		idle           time.Duration
		rateLimitRatio float64
		want           time.Duration
	}{
		{"no idle no rateLimit", 0, 1.0, 30 * time.Second},
		{"3min idle no rateLimit", 3 * time.Minute, 1.0, 30 * time.Second},
		{"6min idle (2x)", 6 * time.Minute, 1.0, 60 * time.Second},
		{"12min idle (4x)", 12 * time.Minute, 1.0, 2 * time.Minute},
		{"25min idle (max)", 25 * time.Minute, 1.0, 5 * time.Minute},
		{"rateLimit only (2x tier)", 0, 0.15, 60 * time.Second},
		{"idle 2x wins over rateLimit 2x", 6 * time.Minute, 0.15, 60 * time.Second},
		{"idle 4x wins over rateLimit 2x", 12 * time.Minute, 0.15, 2 * time.Minute},
		{"rateLimit 2x wins over idle 1x", 3 * time.Minute, 0.15, 60 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeEffectiveInterval(base, tc.idle, tc.rateLimitRatio, false)
			if got != tc.want {
				t.Errorf("computeEffectiveInterval(%v, %v, %v, false) = %v, want %v",
					base, tc.idle, tc.rateLimitRatio, got, tc.want)
			}
		})
	}
}

func TestComputeEffectiveInterval_CapAt5Min(t *testing.T) {
	// With a large configured interval (e.g. 3 minutes), 4x would be 12min,
	// but we cap at 5 minutes.
	base := 3 * time.Minute
	got := computeEffectiveInterval(base, 12*time.Minute, 1.0, false)
	if got != 5*time.Minute {
		t.Errorf("expected cap at 5m, got %v", got)
	}

	// Even 2x of 3 minutes (= 6min) should cap at 5min.
	got = computeEffectiveInterval(base, 6*time.Minute, 1.0, false)
	if got != 5*time.Minute {
		t.Errorf("expected cap at 5m for 2x of 3min base, got %v", got)
	}

	// With webhookHealthy=true the cap rises to 60 min.
	got = computeEffectiveInterval(base, 60*time.Minute, 1.0, true)
	if got != webhookIdleCap {
		t.Errorf("expected webhookIdleCap (%v) when webhook healthy, got %v", webhookIdleCap, got)
	}
}

func TestComputeEffectiveInterval_MaxIdleRateLimit(t *testing.T) {
	base := 30 * time.Second

	// Both backoffs active: idle at max (5min) and rate limit at 2× tier (0.15).
	// max(5min, 2*30s=60s) = 5min.
	got := computeEffectiveInterval(base, 25*time.Minute, 0.15, false)
	if got != 5*time.Minute {
		t.Errorf("expected 5m (idle wins over 2× rate-limit), got %v", got)
	}

	// Both backoffs active: idle at max (5min) and rate limit at 10× tier (0.005).
	// 10×30s = 300s = 5min. max(5min, 5min) = 5min (tie — idle cap governs).
	got = computeEffectiveInterval(base, 25*time.Minute, 0.005, false)
	if got != 5*time.Minute {
		t.Errorf("expected 5m (tie: idle cap == 10× rate-limit at 30s base), got %v", got)
	}
}

func TestComputeEffectiveInterval_RateLimitExceeds5Min(t *testing.T) {
	// Rate-limit backoff alone can exceed 5 minutes (the idle cap doesn't apply).
	// Idle is not active (0 duration), so idleInterval = 3min base.
	base := 3 * time.Minute

	// ratio=0.15 (2× tier): rate-limit interval = 2*3min = 6min.
	// max(3min, 6min) = 6min — the 5min idle cap must NOT clamp this.
	got := computeEffectiveInterval(base, 0, 0.15, false)
	if got != 6*time.Minute {
		t.Errorf("expected 6m (2× of 3min base), got %v", got)
	}

	// ratio=0.07 (4× tier): rate-limit interval = 4*3min = 12min.
	got = computeEffectiveInterval(base, 0, 0.07, false)
	if got != 12*time.Minute {
		t.Errorf("expected 12m (4× of 3min base), got %v", got)
	}

	// ratio=0.03 (6× tier): rate-limit interval = 6*3min = 18min.
	got = computeEffectiveInterval(base, 0, 0.03, false)
	if got != 18*time.Minute {
		t.Errorf("expected 18m (6× of 3min base), got %v", got)
	}

	// ratio=0.005 (10× tier): rate-limit interval = 10*3min = 30min.
	got = computeEffectiveInterval(base, 0, 0.005, false)
	if got != 30*time.Minute {
		t.Errorf("expected 30m (10× of 3min base), got %v", got)
	}
}

func TestComputeEffectiveInterval_RateLimitStepwise(t *testing.T) {
	base := 30 * time.Second

	cases := []struct {
		name  string
		ratio float64
		want  time.Duration
	}{
		{"not in backoff (1.0)", 1.0, 30 * time.Second},
		{"sticky zone (0.35)", 0.35, 60 * time.Second},
		{"active 10-20% (0.15)", 0.15, 60 * time.Second},
		{"boundary exactly 10% (0.10)", 0.10, 60 * time.Second}, // >=0.10 → 2× tier
		{"just below 10% (0.099)", 0.099, 2 * time.Minute},
		{"boundary exactly 5% (0.05)", 0.05, 2 * time.Minute}, // >=0.05 → 4× tier
		{"just below 5% (0.049)", 0.049, 3 * time.Minute},
		{"boundary exactly 1% (0.01)", 0.01, 3 * time.Minute}, // >=0.01 → 6× tier
		{"just below 1% (0.009)", 0.009, 5 * time.Minute},
		{"zero remaining (0.0)", 0.0, 5 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeEffectiveInterval(base, 0, tc.ratio, false)
			if got != tc.want {
				t.Errorf("computeEffectiveInterval(30s, 0, %v, false) = %v, want %v",
					tc.ratio, got, tc.want)
			}
		})
	}
}

// TestNextRateLimitLow_ActivatesWhenLow verifies that nextRateLimitLow transitions
// from false to true when the ratio drops below rateLimitBackoffThreshold (20%).
func TestNextRateLimitLow_ActivatesWhenLow(t *testing.T) {
	if nextRateLimitLow(false, 0.15) != true {
		t.Error("expected true: ratio 15% should activate rate-limit backoff from false")
	}
	if nextRateLimitLow(false, 0.20) != false {
		t.Error("expected false: ratio exactly 20% should not activate (threshold is strictly <)")
	}
}

// TestNextRateLimitLow_StickyBetweenThresholds verifies that once rate-limit backoff
// is active, it remains active when the ratio is between the two thresholds (20%–50%).
func TestNextRateLimitLow_StickyBetweenThresholds(t *testing.T) {
	if nextRateLimitLow(true, 0.25) != true {
		t.Error("expected true: ratio 25% is between thresholds — backoff must stay active (sticky)")
	}
	if nextRateLimitLow(true, 0.49) != true {
		t.Error("expected true: ratio 49% is still below healthy threshold — backoff must stay active")
	}
}

// TestNextRateLimitLow_ClearsWhenHealthy verifies that rate-limit backoff clears
// only when the ratio rises above rateLimitHealthyThreshold (50%).
func TestNextRateLimitLow_ClearsWhenHealthy(t *testing.T) {
	if nextRateLimitLow(true, 0.51) != false {
		t.Error("expected false: ratio 51% exceeds healthy threshold — backoff should clear")
	}
	if nextRateLimitLow(true, 0.50) != true {
		t.Error("expected true: ratio exactly 50% should not clear (threshold is strictly >)")
	}
}

// TestNextRateLimitLow_NoActivationAboveThreshold verifies that when rate-limit
// backoff is not active and quota is healthy, it stays inactive.
func TestNextRateLimitLow_NoActivationAboveThreshold(t *testing.T) {
	if nextRateLimitLow(false, 0.80) != false {
		t.Error("expected false: healthy ratio should not activate backoff")
	}
}

// TestIsRateLimitNearZero_AtZero verifies that remaining=0 is always near zero.
func TestIsRateLimitNearZero_AtZero(t *testing.T) {
	if !isRateLimitNearZero(0, 5000) {
		t.Error("expected true: remaining=0 must be near zero")
	}
}

// TestIsRateLimitNearZero_AtBoundary verifies that remaining=50 with limit=5000 (exactly 1%) is near zero.
func TestIsRateLimitNearZero_AtBoundary(t *testing.T) {
	if !isRateLimitNearZero(50, 5000) {
		t.Error("expected true: remaining=50 (1% of 5000) is at the near-zero boundary")
	}
}

// TestIsRateLimitNearZero_JustAboveBoundary verifies that remaining=51 with limit=5000 (>1%) is not near zero.
func TestIsRateLimitNearZero_JustAboveBoundary(t *testing.T) {
	if isRateLimitNearZero(51, 5000) {
		t.Error("expected false: remaining=51 (>1% of 5000) is just above the near-zero boundary")
	}
}

// TestIsRateLimitNearZero_HealthyQuota verifies that a healthy remaining count is not near zero.
func TestIsRateLimitNearZero_HealthyQuota(t *testing.T) {
	if isRateLimitNearZero(1000, 5000) {
		t.Error("expected false: remaining=1000 is well above the near-zero threshold")
	}
}

// TestIsRateLimitNearZero_ZeroLimit verifies that limit=0 returns false (guards invalid/unknown limit).
func TestIsRateLimitNearZero_ZeroLimit(t *testing.T) {
	if isRateLimitNearZero(0, 0) {
		t.Error("expected false: limit=0 must always return false")
	}
}

// TestPollPreFilter_WaitForCI_CompleteLabelOnly_Skipped verifies that an item with
// wait_for_ci: true and ONLY stage:X:complete (no fabrik:awaiting-ci) is filtered by
// the poll pre-filter when not in cycleSet and no expired CooldownAt. In the
// conjunctive gate design (ADR 032):
//   - During CI await: fabrik:awaiting-ci is present → bypasses the pre-filter.
//   - After CI clears: checkCIGate adds stage:X:complete and removes
//     fabrik:awaiting-ci. The label mutation fires an observer → cycleSet on next poll.
//
// The old wait_for_ci + stage:X:complete bypass is therefore no longer needed.
func TestPollPreFilter_WaitForCI_CompleteLabelOnly_Skipped(t *testing.T) {
	waitForCI := true
	stgs := []*stages.Stage{
		{
			Name:       "Validate",
			Order:      3,
			Prompt:     "Validate it",
			WaitForCI:  &waitForCI,
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
	}
	fixedTime := time.Now().Add(-time.Hour)
	var deepFetched bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{
					Number: 99, ItemID: "PVTI_99", Status: "Validate",
					Repo: "owner/repo", UpdatedAt: fixedTime,
					Labels: []string{"stage:Validate:complete"}, // no fabrik:awaiting-ci
				}},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { deepFetched = true; return nil },
	}
	eng := NewWithDeps(Config{
		Owner:         "owner",
		Repo:          "repo",
		ProjectNum:    1,
		User:          "testuser",
		Token:         "token",
		MaxConcurrent: 5,
		Stages:        stgs,
	}, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	// Seed the store so the pre-filter sees this as a known (previously-processed) item.
	eng.store.Apply(itemstate.InvocationRecorded{Repo: "owner/repo", Number: 99, Completed: true})

	// Post-CI-clear: stage:Validate:complete only (no fabrik:awaiting-ci), not in cycleSet,
	// in store but no CooldownAt → must be filtered (stage is already done).
	if _, err := eng.poll(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if deepFetched {
		t.Error("expected item not to be deep-fetched for wait_for_ci stage with only stage:complete (post-CI-clear state)")
	}
}

// TestPollPreFilter_NoWaitForCI_CompleteLabel_Skipped verifies that an item whose
// stage does NOT have wait_for_ci: true is filtered by the poll pre-filter when it
// has only a stage:<name>:complete label (no fabrik:awaiting-ci), is not in cycleSet,
// and has no expired CooldownAt.
func TestPollPreFilter_NoWaitForCI_CompleteLabel_Skipped(t *testing.T) {
	stgs := []*stages.Stage{
		{
			Name:       "Validate",
			Order:      3,
			Prompt:     "Validate it",
			WaitForCI:  nil, // wait_for_ci not set
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
	}
	fixedTime := time.Now().Add(-time.Hour)
	var deepFetched bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{
					Number: 99, ItemID: "PVTI_99", Status: "Validate",
					Repo: "owner/repo", UpdatedAt: fixedTime,
					Labels: []string{"stage:Validate:complete"},
				}},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { deepFetched = true; return nil },
	}
	eng := NewWithDeps(Config{
		Owner:         "owner",
		Repo:          "repo",
		ProjectNum:    1,
		User:          "testuser",
		Token:         "token",
		MaxConcurrent: 5,
		Stages:        stgs,
	}, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	// Seed the store so the pre-filter sees this as a known (previously-processed) item.
	eng.store.Apply(itemstate.InvocationRecorded{Repo: "owner/repo", Number: 99, Completed: true})

	// Not in cycleSet, in store but no CooldownAt → stage-complete item must be skipped.
	if _, err := eng.poll(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if deepFetched {
		t.Error("expected item not to be deep-fetched for non-wait_for_ci stage with only stage:complete")
	}
}

// TestPoll_CruiseValidateComplete_NoRepeatDeepFetch is a regression test for the
// perpetual deep-fetch loop (issue #488). Terminal items — cruise+Validate complete,
// paused+complete, closed-with-stage-complete — would trigger a deep-fetch on every
// poll cycle once the CooldownAt["periodic-re-eval"] window expired, indefinitely.
// This test verifies that at most one deep-fetch occurs across two poll cycles.
func TestPoll_CruiseValidateComplete_NoRepeatDeepFetch(t *testing.T) {
	fixedTime := time.Now().Add(-time.Hour)
	deepFetchCount := 0

	stgs := testStagesWithValidate()
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number:    732,
						ItemID:    "PVTI_732",
						Status:    "Validate",
						Repo:      "owner/repo",
						UpdatedAt: fixedTime,
						Labels:    []string{"stage:Validate:complete", "fabrik:cruise"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCount++
			return nil
		},
	}

	// Multi-repo mode: cfg.Repo == "" so all repos on the board are processed.
	eng := NewWithDeps(Config{
		Owner:         "owner",
		Repo:          "",
		ProjectNum:    1,
		User:          "testuser",
		Token:         "token",
		MaxConcurrent: 5,
		PollSeconds:   1, // 10s cooldown
		Stages:        stgs,
	}, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	// Simulate the perpetual-loop trigger: CooldownAt["periodic-re-eval"] expired.
	eng.store.Apply(itemstate.CooldownRecorded{
		Repo: "owner/repo", Number: 732, Reason: "periodic-re-eval",
		Until: time.Now().Add(-2 * time.Minute),
	})

	ctx := context.Background()

	// Poll 1: with Part 1 fix, stage:Validate:complete suppresses the cooldown re-eval.
	// With Part 2 fix only (label absent), this poll triggers one deep-fetch and then
	// refreshes CooldownAt["periodic-re-eval"] so Poll 2 is suppressed.
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll 1: %v", err)
	}

	// Poll 2: regardless of which part fires, no additional deep-fetch should occur.
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll 2: %v", err)
	}

	if deepFetchCount > 1 {
		t.Errorf("FetchItemDetails called %d times across 2 polls; want at most 1 — perpetual deep-fetch loop must not recur", deepFetchCount)
	}
}

// ---------------------------------------------------------------------------
// Layer 1 — opportunistic per-event Status refresh
// ---------------------------------------------------------------------------

// seedTestCache creates a CacheImpl bootstrapped with one item (PVTI_001, owner/repo#1, "Research").
func seedTestCache(t *testing.T, client *mockGitHubClient) *boardcache.CacheImpl {
	t.Helper()
	cache := boardcache.NewCacheImpl(client, itemstate.NewStore(nil), func(format string, args ...any) {})
	board := &gh.ProjectBoard{
		ProjectID: "PVT_test",
		Items: []gh.ProjectItem{
			{Number: 1, ItemID: "PVTI_001", Status: "Research", Repo: "owner/repo"},
		},
	}
	testBootstrapFromBoard(cache, board)
	return cache
}

func TestLayer1StatusRefresh_UpdatesCacheOnIssueCommentEvent(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectItemStatusFn: func(itemID string) (string, error) {
			return "Plan", nil
		},
	}
	cache := seedTestCache(t, client)
	eng := testEngine(t, client, &mockClaudeInvoker{})

	payload, _ := json.Marshal(map[string]any{
		"issue":      map[string]any{"number": 1},
		"repository": map[string]any{"full_name": "owner/repo"},
	})
	eng.applyLayer1StatusRefresh("issue_comment", payload, cache)

	// FetchProjectItemStatus must have been called for PVTI_001.
	client.mu.Lock()
	calls := append([]string(nil), client.fetchProjectItemStatusCalls...)
	client.mu.Unlock()
	if len(calls) != 1 || calls[0] != "PVTI_001" {
		t.Errorf("want FetchProjectItemStatus called once with %q, got %v", "PVTI_001", calls)
	}

	// Verify the cache now reflects "Plan" — the status returned by FetchProjectItemStatus.
	board, err := cache.FetchProjectBoard("owner", "repo", 1, "")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	var gotStatus string
	for _, item := range board.Items {
		if item.Number == 1 {
			gotStatus = item.Status
		}
	}
	if gotStatus != "Plan" {
		t.Errorf("want cached Status %q after Layer 1 refresh, got %q", "Plan", gotStatus)
	}
}

func TestLayer1StatusRefresh_SkipsWhenCachePaused(t *testing.T) {
	var callCount int32
	client := &mockGitHubClient{
		fetchProjectItemStatusFn: func(itemID string) (string, error) {
			atomic.AddInt32(&callCount, 1)
			return "Plan", nil
		},
	}
	cache := seedTestCache(t, client)
	cache.Pause()
	eng := testEngine(t, client, &mockClaudeInvoker{})

	payload, _ := json.Marshal(map[string]any{
		"issue":      map[string]any{"number": 1},
		"repository": map[string]any{"full_name": "owner/repo"},
	})
	eng.applyLayer1StatusRefresh("issue_comment", payload, cache)

	if n := atomic.LoadInt32(&callCount); n != 0 {
		t.Errorf("FetchProjectItemStatus should not be called when cache is paused; got %d calls", n)
	}
}

func TestLayer1StatusRefresh_SkipsNonIssueEvents(t *testing.T) {
	var callCount int32
	client := &mockGitHubClient{
		fetchProjectItemStatusFn: func(itemID string) (string, error) {
			atomic.AddInt32(&callCount, 1)
			return "Plan", nil
		},
	}
	cache := seedTestCache(t, client)
	eng := testEngine(t, client, &mockClaudeInvoker{})

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{"number": 1},
		"repository":   map[string]any{"full_name": "owner/repo"},
	})
	eng.applyLayer1StatusRefresh("pull_request", payload, cache)

	if n := atomic.LoadInt32(&callCount); n != 0 {
		t.Errorf("FetchProjectItemStatus should not be called for pull_request events; got %d calls", n)
	}
}

// ---------------------------------------------------------------------------
// Layer 2 — updatedAt gate in poll loop
// ---------------------------------------------------------------------------

// TestPollGateMiss verifies that when project.updatedAt is unchanged between
// two poll cycles, FetchProjectItemStatusBatch is called only once (on the
// first cycle when timestamp advances from zero).
func TestPollGateMiss(t *testing.T) {
	t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	client := &mockGitHubClient{
		fetchProjectUpdatedAtFn: func(projectID string) (time.Time, error) {
			return t1, nil // same timestamp on every call
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{ProjectID: "PVT_1", Items: nil})
	eng.readClient = cache

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	eng.wg.Wait()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	eng.wg.Wait()

	client.mu.Lock()
	calls := client.fetchProjectItemStatusBatchCalls
	client.mu.Unlock()
	// Gate fires on cycle 1 (t1 after zero), skips on cycle 2 (t1 == t1).
	if calls != 1 {
		t.Errorf("FetchProjectItemStatusBatch: want 1 call, got %d", calls)
	}
}

// TestPollGateFire verifies that when project.updatedAt advances between two
// poll cycles, FetchProjectItemStatusBatch is called on each cycle.
func TestPollGateFire(t *testing.T) {
	t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	var callIdx int
	timestamps := []time.Time{t1, t2}
	client := &mockGitHubClient{
		fetchProjectUpdatedAtFn: func(projectID string) (time.Time, error) {
			ts := timestamps[callIdx]
			if callIdx < len(timestamps)-1 {
				callIdx++
			}
			return ts, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{ProjectID: "PVT_1", Items: nil})
	eng.readClient = cache

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	eng.wg.Wait()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	eng.wg.Wait()

	client.mu.Lock()
	calls := client.fetchProjectItemStatusBatchCalls
	client.mu.Unlock()
	// Gate fires on cycle 1 (t1 after zero) and cycle 2 (t2 after t1).
	if calls != 2 {
		t.Errorf("FetchProjectItemStatusBatch: want 2 calls, got %d", calls)
	}
}

// TestPollGateFire_AppliesStatusDrift verifies the behavioral contract: when the
// gate fires, ApplyStatusBatch updates the cached item Status and the new Status
// is visible to the subsequent board read in the same poll cycle.
func TestPollGateFire_AppliesStatusDrift(t *testing.T) {
	t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	client := &mockGitHubClient{
		fetchProjectUpdatedAtFn: func(projectID string) (time.Time, error) {
			return t1, nil
		},
		fetchProjectItemStatusBatchFn: func(projectID string) (map[string]string, error) {
			return map[string]string{"PVTI_001": "Implement"}, nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	// Gate fires (t1 > zero); batch returns drifted status; cache must reflect it.
	board, err := cache.FetchProjectBoard("owner", "repo", 1, "")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	var got string
	for _, item := range board.Items {
		if item.Number == 1 {
			got = item.Status
		}
	}
	if got != "Implement" {
		t.Errorf("want cached Status %q after gate-fired batch, got %q", "Implement", got)
	}
}

// TestSeedLabels_multiRepo verifies that in multi-repo mode (cfg.Repo == ""),
// SeedLabels is called exactly once per discovered repo across two poll cycles,
// and never called with an empty repo argument.
func TestSeedLabels_multiRepo(t *testing.T) {
	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 1, Title: "Issue 1", Status: "Done", Repo: "owner/repo1"},
			{Number: 2, Title: "Issue 2", Status: "Done", Repo: "owner/repo2"},
		},
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return board, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "F1",
				Options: map[string]string{"Research": "OPT_1"},
			}, nil
		},
	}

	// Multi-repo mode: Owner set, Repo empty.
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStages(),
		},
		client,
		&mockClaudeInvoker{},
		nil,
	)

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll 2: %v", err)
	}

	client.mu.Lock()
	calls := make([]seedLabelsCall, len(client.seedLabelsCalls))
	copy(calls, client.seedLabelsCalls)
	client.mu.Unlock()

	// No call should have an empty repo.
	for _, c := range calls {
		if c.repo == "" {
			t.Errorf("SeedLabels called with empty repo: %+v", c)
		}
	}

	// Each unique owner/repo should be seeded exactly once across both poll cycles.
	seen := make(map[string]int)
	for _, c := range calls {
		seen[c.owner+"/"+c.repo]++
	}
	for _, ownerRepo := range []string{"owner/repo1", "owner/repo2"} {
		if seen[ownerRepo] != 1 {
			t.Errorf("SeedLabels for %s called %d times, want 1", ownerRepo, seen[ownerRepo])
		}
	}
}

// ---------------------------------------------------------------------------
// Poll log: 'from cache' vs 'from GitHub' wording (Tasks 4-8)
// ---------------------------------------------------------------------------

// collectPollLogs captures log messages emitted during a single poll call.
// Waits for all dispatched worker goroutines to finish (via eng.wg) before
// returning so that callers can safely reassign eng.events for a subsequent poll
// without racing against goroutines that still read the field.
func collectPollLogs(t *testing.T, eng *Engine) []string {
	t.Helper()
	events := make(chan tui.Event, 256)
	eng.events = events
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait() // drain background workers before caller touches eng.events again
	// Non-blocking drain: read what's already buffered.
	var msgs []string
	for {
		select {
		case ev := <-events:
			if le, ok := ev.(tui.LogEvent); ok {
				msgs = append(msgs, le.Message)
			}
		default:
			return msgs
		}
	}
}

// TestPoll_LogBoardFromGitHub verifies the board fetch log says "from GitHub"
// when readClient is a pass-through GitHubAdapter (no-cache mode).
func TestPoll_LogBoardFromGitHub(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: "PVT_1", Items: nil}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	logs := collectPollLogs(t, eng)

	var boardLog string
	for _, m := range logs {
		if strings.Contains(m, "project board") {
			boardLog = m
			break
		}
	}
	if !strings.Contains(boardLog, "from GitHub") {
		t.Errorf("board log = %q; want to contain \"from GitHub\"", boardLog)
	}
	if strings.Contains(boardLog, "from cache") {
		t.Errorf("board log = %q; must not contain \"from cache\"", boardLog)
	}
}

// TestPoll_LogBoardFromCache verifies the board fetch log says "from cache"
// when readClient is a bootstrapped, unpaused CacheImpl.
func TestPoll_LogBoardFromCache(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	logs := collectPollLogs(t, eng)

	var boardLog string
	for _, m := range logs {
		if strings.Contains(m, "project board") {
			boardLog = m
			break
		}
	}
	if !strings.Contains(boardLog, "from cache") {
		t.Errorf("board log = %q; want to contain \"from cache\"", boardLog)
	}
	if strings.Contains(boardLog, "from GitHub") {
		t.Errorf("board log = %q; must not contain \"from GitHub\"", boardLog)
	}
}

// TestPoll_LogBoardBootstrapThenCache verifies the board log says "from GitHub"
// before Bootstrap and "from cache" after Bootstrap.
func TestPoll_LogBoardBootstrapThenCache(t *testing.T) {
	board := &gh.ProjectBoard{
		ProjectID: "PVT_test",
		Items: []gh.ProjectItem{
			{Number: 1, ItemID: "PVTI_001", Status: "Research", Repo: "owner/repo"},
		},
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return board, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	eng.readClient = cache

	// Poll 1: cache not yet bootstrapped — must fetch from GitHub.
	logs1 := collectPollLogs(t, eng)
	var boardLog1 string
	for _, m := range logs1 {
		if strings.Contains(m, "project board") {
			boardLog1 = m
			break
		}
	}
	if !strings.Contains(boardLog1, "from GitHub") {
		t.Errorf("poll 1 board log = %q; want \"from GitHub\"", boardLog1)
	}

	// Simulate a webhook-triggered bootstrap.
	testBootstrapFromBoard(cache, board)

	// Poll 2: cache is now bootstrapped — must serve from cache.
	logs2 := collectPollLogs(t, eng)
	var boardLog2 string
	for _, m := range logs2 {
		if strings.Contains(m, "project board") {
			boardLog2 = m
			break
		}
	}
	if !strings.Contains(boardLog2, "from cache") {
		t.Errorf("poll 2 board log = %q; want \"from cache\"", boardLog2)
	}
}

// TestPoll_LogItemFromGitHub verifies the item deep-fetch log says "from GitHub"
// when the item's deep cache entry is empty, even though the shallow Bootstrap
// has already populated the Store. The first poll's Bootstrap-or-Reconcile fires
// StatusChanged through mayNeedWorkObserver → cycleSet, so the item passes the
// pre-filter; IsItemDeepFetched is still false (deep state never populated), so
// the deep-fetch falls through to GitHub.
func TestPoll_LogItemFromGitHub(t *testing.T) {
	board := &gh.ProjectBoard{
		ProjectID: "PVT_test",
		Items:     []gh.ProjectItem{{Number: 1, ItemID: "PVTI_001", Status: "Research", Repo: "owner/repo"}},
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return board, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.readClient = boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})

	// Register mayNeedWorkObserver before poll(): the per-poll Bootstrap fires
	// StatusChanged which must populate mayNeedWork → cycleSet for the item to
	// reach the deep-fetch section (matching production wiring in Run()).
	eng.store.Subscribe(newMayNeedWorkObserver(&eng.mayNeedWorkMu, &eng.mayNeedWork))

	logs := collectPollLogs(t, eng)

	var itemLog string
	for _, m := range logs {
		if strings.Contains(m, "details for") {
			itemLog = m
			break
		}
	}
	if !strings.Contains(itemLog, "from GitHub") {
		t.Errorf("item log = %q; want to contain \"from GitHub\"", itemLog)
	}
	if strings.Contains(itemLog, "from cache") {
		t.Errorf("item log = %q; must not contain \"from cache\"", itemLog)
	}
}

// TestPoll_LogItemFromCache verifies the item deep-fetch log says "from cache"
// when the item's deep-fetch state is already populated in the store.
// Registers the mayNeedWorkObserver before Bootstrap so Bootstrap's StatusChanged
// notification populates cycleSet, letting the item reach the deep-fetch section.
func TestPoll_LogItemFromCache(t *testing.T) {
	board := &gh.ProjectBoard{
		ProjectID: "PVT_test",
		Items:     []gh.ProjectItem{{Number: 1, ItemID: "PVTI_001", Status: "Research", Repo: "owner/repo"}},
	}
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})

	// Register the mayNeedWorkObserver before Bootstrap so the StatusChanged
	// notification from Bootstrap populates e.mayNeedWork → cycleSet on next poll.
	eng.store.Subscribe(newMayNeedWorkObserver(&eng.mayNeedWorkMu, &eng.mayNeedWork))

	// Bootstrap → store has item #1; StatusChanged fires → "owner/repo#1" in mayNeedWork.
	testBootstrapFromBoard(cache, board)

	// Pre-populate LastDeepFetchAt so IsItemDeepFetched returns true.
	eng.store.Apply(itemstate.ItemDeepFetched{
		Repo:       "owner/repo",
		Number:     1,
		FreshState: gh.ProjectItem{Number: 1, Repo: "owner/repo", Status: "Research"},
	})

	eng.readClient = cache

	logs := collectPollLogs(t, eng)

	var itemLog string
	for _, m := range logs {
		if strings.Contains(m, "details for") {
			itemLog = m
			break
		}
	}
	if !strings.Contains(itemLog, "from cache") {
		t.Errorf("item log = %q; want to contain \"from cache\"", itemLog)
	}
	if strings.Contains(itemLog, "from GitHub") {
		t.Errorf("item log = %q; must not contain \"from GitHub\"", itemLog)
	}
}

// TestInFlightItem_NotDeepFetchedByWorkerLifecycleChanged verifies that after Fix B
// (cycleSetFlags excludes WorkerLifecycleChanged), an in-flight item whose WorkerEntered
// event fires WorkerLifecycleChanged is NOT added to cycleSet and therefore NOT
// deep-fetched in the subsequent poll. The wake channel still fires (wakeChFlags still
// includes WorkerLifecycleChanged), but the cycleSet bypass no longer applies.
func TestInFlightItem_NotDeepFetchedByWorkerLifecycleChanged(t *testing.T) {
	// After Fix B: cycleSetFlags excludes WorkerLifecycleChanged. WorkerEntered fires
	// WorkerLifecycleChanged → wake channel fires (poll wakes up) but the
	// mayNeedWorkObserver does NOT add the item to cycleSet. Without cycleSet membership,
	// an item already in the Store (no "notInStore") with no cooldown and no bypass
	// labels is skipped by the prefilter (line 882 in poll.go). FetchItemDetails is not
	// called and no cooldown is stamped. Re-dispatch is driven by StatusChanged or
	// LabelsChanged events (which ARE in cycleSetFlags), not by WorkerLifecycleChanged.
	deepFetchCalled := false
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{Number: 77, Repo: "owner/repo", Status: "Research", ItemID: "PVTI_77"},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{FieldID: "F1", Options: map[string]string{}}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalled = true
			return nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Register the mayNeedWork observer to demonstrate it does NOT fire for
	// WorkerLifecycleChanged (Fix B: cycleSetFlags excludes WorkerLifecycleChanged).
	mwnObs := newMayNeedWorkObserver(&eng.mayNeedWorkMu, &eng.mayNeedWork)
	eng.store.Subscribe(mwnObs)

	// Simulate a worker still running from a prior poll cycle via the Store.
	// WorkerEntered fires WorkerLifecycleChanged which is in wakeChFlags (wake channel)
	// but NOT in cycleSetFlags (mayNeedWork/cycleSet). The observer does not add #77.
	eng.store.Apply(itemstate.WorkerEntered{
		Repo:      "owner/repo",
		Number:    77,
		StageName: "Research",
		StartedAt: time.Now(),
	})

	// Verify that WorkerLifecycleChanged did NOT populate mayNeedWork (Fix B).
	eng.mayNeedWorkMu.Lock()
	inCycleSet := eng.mayNeedWork["owner/repo#77"]
	eng.mayNeedWorkMu.Unlock()
	if inCycleSet {
		t.Error("Fix B: WorkerLifecycleChanged must not populate cycleSet; item #77 should not be in mayNeedWork after WorkerEntered")
	}

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	// FetchItemDetails must NOT be called: the item is in the Store (not notInStore),
	// has no cooldown, no bypass labels, and is not in cycleSet. The prefilter skips it.
	if deepFetchCalled {
		t.Error("Fix B: FetchItemDetails must not be called for an in-flight item after WorkerEntered fires WorkerLifecycleChanged (cycleSetFlags excludes WorkerLifecycleChanged)")
	}
}

// TestWorkerExitedWakesObserver verifies the end-to-end wakeChFlags wiring for
// WorkerLifecycleChanged. Both WorkerEntered and WorkerExited must fire the wake channel
// because WorkerLifecycleChanged is in wakeChFlags (Fix B for #544).
// WorkerHeartbeat and WorkerPIDSet do NOT fire the wake channel (they emit WorkerChanged
// but not WorkerLifecycleChanged), preventing deep-fetch churn for active workers.
func TestWorkerExitedWakesObserver(t *testing.T) {
	store := itemstate.NewStore(nil)
	wakeCh := make(chan struct{}, 4)

	obs := newWakeChObserver(wakeCh)
	store.Subscribe(obs)

	// Store.Apply + observer notification are synchronous, and the wakeChObserver's
	// channel send is non-blocking. After Apply returns, the wake channel
	// deterministically has a token or doesn't — no need for time.After timeouts.

	// WorkerEntered must wake because WorkerLifecycleChanged is in wakeChFlags.
	store.Apply(itemstate.WorkerEntered{
		Repo:      "owner/repo",
		Number:    99,
		StageName: "Implement",
		StartedAt: time.Now(),
	})
	select {
	case <-wakeCh:
		// expected
	default:
		t.Error("wake channel did not fire after WorkerEntered")
	}

	// WorkerExited must also wake (deterministic re-dispatch after worker finishes).
	store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 99})
	select {
	case <-wakeCh:
		// expected
	default:
		t.Error("wake channel did not fire after WorkerExited")
	}

	// WorkerHeartbeat must NOT wake: it emits WorkerChanged but not WorkerLifecycleChanged.
	// The wake channel must stay empty after a heartbeat to prevent deep-fetch churn.
	store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 99, StageName: "X", StartedAt: time.Now()})
	<-wakeCh // drain the WorkerEntered wake
	store.Apply(itemstate.WorkerHeartbeat{Repo: "owner/repo", Number: 99, At: time.Now()})
	select {
	case <-wakeCh:
		t.Error("wake channel must not fire after WorkerHeartbeat (only WorkerLifecycleChanged is in wakeChFlags)")
	default:
		// expected: heartbeat does not wake
	}
}

// TestDispatchGoroutineWorkerExitedOnEarlyReturn verifies the fix for the
// stuck-Worker bug observed on #559 / #563: the main dispatch goroutine in
// poll.go must defer WorkerExited at the goroutine's top level, so that
// processItem's early-return paths (paused, blocked, awaiting-input,
// locked-by-other, stage-complete, etc.) all release the Worker entry.
//
// Pre-fix, the only WorkerExited defer lived inside processItem itself
// (item.go:533), reached only after ~14 early-return guards. Any of those
// returns would leak a Worker entry, blocking re-dispatch via the
// snap.Worker() != nil guard and also blocking auto-upgrade (which gates
// on snap.Worker() != nil for any item).
//
// This test simulates the goroutine pattern from poll.go: WorkerEntered is
// applied before goroutine launch, the goroutine runs a body that returns
// early (without calling processItem's internal defer), and the
// goroutine-level defer fires WorkerExited. After the goroutine completes,
// snap.Worker() must be nil.
func TestDispatchGoroutineWorkerExitedOnEarlyReturn(t *testing.T) {
	store := itemstate.NewStore(nil)

	// Mirror the dispatch site in poll.go: apply WorkerEntered before goroutine.
	store.Apply(itemstate.WorkerEntered{
		Repo:      "owner/repo",
		Number:    42,
		StageName: "Implement",
		StartedAt: time.Now(),
	})

	snap, err := store.Get("owner/repo", 42)
	if err != nil {
		t.Fatalf("store.Get after WorkerEntered: %v", err)
	}
	if snap.Worker() == nil {
		t.Fatal("Worker should be set after WorkerEntered")
	}

	// Simulate the dispatch goroutine. The defer at the goroutine top must fire
	// even when the inner work (processItem-equivalent) early-returns without
	// running its own defer.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 42})
		// Simulate processItem early-return — no inner WorkerExited fires here.
		return
	}()
	<-done

	snap, err = store.Get("owner/repo", 42)
	if err != nil {
		t.Fatalf("store.Get after goroutine exit: %v", err)
	}
	if snap.Worker() != nil {
		t.Errorf("Worker should be nil after goroutine exit; got %+v", snap.Worker())
	}
}

// TestWorkerExitedIdempotent verifies that WorkerExited applied twice is a
// no-op on the second call (returns 0 changes). This makes the redundant
// defer at item.go:533 harmless — when processItem runs to completion and
// fires its own WorkerExited, the goroutine-level defer in poll.go fires
// WorkerExited again, but the second call should not produce spurious
// observer notifications.
func TestWorkerExitedIdempotent(t *testing.T) {
	store := itemstate.NewStore(nil)
	store.Apply(itemstate.WorkerEntered{
		Repo: "owner/repo", Number: 1,
		StageName: "X", StartedAt: time.Now(),
	})

	wakeCh := make(chan struct{}, 4)
	store.Subscribe(newWakeChObserver(wakeCh))

	// Store.Apply is synchronous and the wakeChObserver's channel send is non-blocking,
	// so after Apply returns the wake channel deterministically either has a token or
	// doesn't. Use non-blocking selects rather than time.After to keep the test
	// deterministic and fast.

	// First WorkerExited: should fire wake (Worker was non-nil → cleared → wake).
	store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 1})
	select {
	case <-wakeCh:
		// expected
	default:
		t.Fatal("first WorkerExited should fire wake")
	}

	// Second WorkerExited: must be a no-op (Worker already nil → no flags → no wake).
	store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 1})
	select {
	case <-wakeCh:
		t.Error("second WorkerExited should be a no-op (Worker already nil); spurious wake fired")
	default:
		// expected: no wake
	}
}

// ---------------------------------------------------------------------------
// runProbeAndDeepFetch integration tests
// ---------------------------------------------------------------------------

// TestRunProbeAndDeepFetch_StaleItem_TriggersDeepFetch verifies that an item
// with no prior deep-fetch (LastSeenSourceUpdatedAt == zero) triggers
// FetchItemDetails when the probe returns a nonzero EffectiveUpdatedAt.
func TestRunProbeAndDeepFetch_StaleItem_TriggersDeepFetch(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	var deepFetchCalls int
	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo", Status: "Research", EffectiveUpdatedAt: now},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	eng.runProbeAndDeepFetch(cache)
	if deepFetchCalls == 0 {
		t.Error("expected FetchItemDetails called for stale item (zero LastSeenSourceUpdatedAt); got 0 calls")
	}
}

// TestRunProbeAndDeepFetch_FreshItem_SkipsDeepFetch verifies that an item
// whose LastSeenSourceUpdatedAt matches the probe's EffectiveUpdatedAt does
// not trigger a FetchItemDetails call.
func TestRunProbeAndDeepFetch_FreshItem_SkipsDeepFetch(t *testing.T) {
	T1 := time.Now().Add(-time.Hour).Truncate(time.Second)
	var deepFetchCalls int
	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo", Status: "Research", EffectiveUpdatedAt: T1},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	// Simulate a prior deep-fetch that set LastSeenSourceUpdatedAt = T1.
	eng.store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 1,
		FreshState: gh.ProjectItem{
			ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", UpdatedAt: T1,
		},
	})
	eng.runProbeAndDeepFetch(cache)
	if deepFetchCalls != 0 {
		t.Errorf("expected 0 FetchItemDetails calls for fresh item; got %d", deepFetchCalls)
	}
}

// TestRunProbeAndDeepFetch_LinkageDrift_InvalidatesAndDeepFetches verifies
// that when the probe detects a linked PR number different from the cached
// value, the cache is invalidated (DeepFetchInvalidated) and FetchItemDetails
// is triggered even though EffectiveUpdatedAt has not advanced.
func TestRunProbeAndDeepFetch_LinkageDrift_InvalidatesAndDeepFetches(t *testing.T) {
	T1 := time.Now().Add(-time.Hour).Truncate(time.Second)
	var deepFetchCalls int
	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				// Same EffectiveUpdatedAt as last deep-fetch (would be fresh) but LinkedPRNumber changed.
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo", Status: "Research", EffectiveUpdatedAt: T1, LinkedPRNumber: 99},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	// Simulate fresh state at T1 with no linked PR (cached LinkedPRNumber = 0).
	eng.store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 1,
		FreshState: gh.ProjectItem{
			ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", UpdatedAt: T1,
		},
	})
	eng.runProbeAndDeepFetch(cache)
	if deepFetchCalls == 0 {
		t.Error("expected FetchItemDetails called after linkage drift (PR# 0 → 99); got 0 calls")
	}
}

// TestRunProbeAndDeepFetch_ItemGone_RemovedFromStore verifies that an item
// present in the store but absent from probe results is removed from the store.
func TestRunProbeAndDeepFetch_ItemGone_RemovedFromStore(t *testing.T) {
	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			// Only item #1; item #2 has left the board.
			return []gh.BoardProbeItem{
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo", Status: "Research"},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { return nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo"},
			{ID: "I_002", ItemID: "PVTI_002", Number: 2, Repo: "owner/repo"},
		},
	})
	eng.readClient = cache

	eng.runProbeAndDeepFetch(cache)

	if _, err := eng.store.Get("owner/repo", 2); err == nil {
		t.Error("item #2 should be removed from store after probe omits it")
	}
	if _, err := eng.store.Get("owner/repo", 1); err != nil {
		t.Errorf("item #1 should still be in store after probe includes it: %v", err)
	}
}

// TestRunStartupTransientLabelScan_RemovesStaleLabelsFromClosedItems verifies
// that runStartupTransientLabelScan triggers label cleanup on closed store
// entries carrying transient lifecycle labels, without touching open items or
// clean closed items.
func TestRunStartupTransientLabelScan_RemovesStaleLabelsFromClosedItems(t *testing.T) {
	var removedLabels []string
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			removedLabels = append(removedLabels, labelName)
			return nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Seed three items into the store:
	//   #1 — closed, carries a transient label → should be cleaned
	//   #2 — open, carries a transient label → must be skipped
	//   #3 — closed, no transient labels → must be skipped
	for _, pi := range []gh.ProjectItem{
		{ID: "I_001", Number: 1, Repo: "owner/repo", IsClosed: true, Labels: []string{"fabrik:awaiting-review", "stage:Review:complete"}},
		{ID: "I_002", Number: 2, Repo: "owner/repo", IsClosed: false, Labels: []string{"fabrik:awaiting-ci"}},
		{ID: "I_003", Number: 3, Repo: "owner/repo", IsClosed: true, Labels: []string{"stage:Validate:complete"}},
	} {
		eng.store.Apply(itemstate.IssueOpened{Item: pi})
		eng.store.Apply(itemstate.ItemDeepFetched{
			Repo:       pi.Repo,
			Number:     pi.Number,
			FreshState: pi,
		})
	}

	eng.runStartupTransientLabelScan()

	// Only the transient label from issue #1 should be removed.
	if len(removedLabels) == 0 {
		t.Fatal("expected RemoveLabelFromIssue called for closed item with stale transient label; got 0 calls")
	}
	found := false
	for _, l := range removedLabels {
		if l == "fabrik:awaiting-review" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'fabrik:awaiting-review' to be removed; removed labels: %v", removedLabels)
	}
}

// TestColdStart_ProbeBootstrap_TerminalItemsSkipDeepFetch verifies the cold-start cost
// reduction: 10 closed Done items are seeded terminal by BootstrapFromProbe and
// are never deep-fetched, while 3 open active items are deep-fetched normally.
// Expected deep-fetch count after the first probe cycle: ≤ 3.
func TestColdStart_ProbeBootstrap_TerminalItemsSkipDeepFetch(t *testing.T) {
	var deepFetchCalls int
	probeTime := time.Now().Add(-time.Minute)

	// Build 10 closed Done items + 3 open Research items for the probe response.
	var probeItems []gh.BoardProbeItem
	for i := 1; i <= 10; i++ {
		probeItems = append(probeItems, gh.BoardProbeItem{
			ContentID: fmt.Sprintf("I_%03d", i), ItemID: fmt.Sprintf("PVTI_%03d", i),
			Number: i, Repo: "owner/repo",
			Status:             "Done",
			IsClosed:           true,
			EffectiveUpdatedAt: probeTime,
		})
	}
	for i := 11; i <= 13; i++ {
		probeItems = append(probeItems, gh.BoardProbeItem{
			ContentID: fmt.Sprintf("I_%03d", i), ItemID: fmt.Sprintf("PVTI_%03d", i),
			Number: i, Repo: "owner/repo",
			Status:             "Research",
			IsClosed:           false,
			EffectiveUpdatedAt: probeTime,
		})
	}

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return probeItems, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", ProjectNum: 1,
			User: "testuser", Token: "token", MaxConcurrent: 5,
			Stages: testStagesWithCleanup(),
		},
		client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	eng.readClient = cache

	// Simulate the virgin-cache branch: probe bootstrap seeds the store.
	items, projectID, err := client.ProbeProjectBoard("owner", "repo", 1, "organization")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	cache.BootstrapFromProbe(items, projectID)
	eng.seedTerminalFromProbeItems(items)

	// Now simulate the next poll cycle — probe-driven deep-fetch pass.
	eng.runProbeAndDeepFetch(cache)

	// The 10 closed Done items are seeded terminal and must NOT be deep-fetched.
	// The 3 open Research items have no prior deep-fetch and MUST be deep-fetched.
	if deepFetchCalls > 3 {
		t.Errorf("cold-start deep-fetch count = %d, want ≤ 3 (only active items)", deepFetchCalls)
	}
	if deepFetchCalls == 0 {
		t.Error("expected ≥ 1 deep-fetch for active items; got 0")
	}

	// Terminal flag must be set on all closed Done items.
	for i := 1; i <= 10; i++ {
		snap, snapErr := eng.store.Get("owner/repo", i)
		if snapErr != nil {
			t.Errorf("item #%d not found in store", i)
			continue
		}
		if !snap.IsTerminal() {
			t.Errorf("item #%d (closed Done): expected IsTerminal()=true", i)
		}
	}

	// Active Research items must NOT be terminal.
	for i := 11; i <= 13; i++ {
		snap, snapErr := eng.store.Get("owner/repo", i)
		if snapErr != nil {
			t.Errorf("item #%d not found in store", i)
			continue
		}
		if snap.IsTerminal() {
			t.Errorf("item #%d (open Research): expected IsTerminal()=false", i)
		}
	}
}

// TestWebhookModeStartup_ClosedDoneItemsNotDeepFetched is the regression test
// for issue #751. It verifies that after the fixed webhook-mode startup path
// (BootstrapFromProbe instead of Bootstrap), the first probe cycle does NOT
// call FetchItemDetails for closed Done items. Only active items are fetched.
func TestWebhookModeStartup_ClosedDoneItemsNotDeepFetched(t *testing.T) {
	var deepFetchCalls int
	probeTime := time.Now().Add(-time.Minute)

	// 3 closed Done items + 1 open Research item returned by the probe.
	allProbeItems := []gh.BoardProbeItem{
		{ContentID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo",
			Status: "Done", IsClosed: true, EffectiveUpdatedAt: probeTime},
		{ContentID: "I_002", ItemID: "PVTI_002", Number: 2, Repo: "owner/repo",
			Status: "Done", IsClosed: true, EffectiveUpdatedAt: probeTime},
		{ContentID: "I_003", ItemID: "PVTI_003", Number: 3, Repo: "owner/repo",
			Status: "Done", IsClosed: true, EffectiveUpdatedAt: probeTime},
		{ContentID: "I_004", ItemID: "PVTI_004", Number: 4, Repo: "owner/repo",
			Status: "Research", IsClosed: false, EffectiveUpdatedAt: probeTime},
	}

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return allProbeItems, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", ProjectNum: 1,
			User: "testuser", Token: "token", MaxConcurrent: 5,
			Stages: testStagesWithCleanup(),
		},
		client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})

	// Simulate the fixed webhook startup path: BootstrapFromProbe seeds Terminal
	// for closed Done items before the first poll cycle runs.
	cache.BootstrapFromProbe(allProbeItems, "PVT_1")
	eng.seedTerminalFromProbeItems(allProbeItems)
	eng.readClient = cache

	// Run one probe cycle — this is what the first poll does after startup.
	eng.runProbeAndDeepFetch(cache)

	// The 3 closed Done items are seeded terminal and must NOT be deep-fetched.
	// Only the 1 open Research item should trigger FetchItemDetails.
	if deepFetchCalls != 1 {
		t.Errorf("webhook startup deep-fetch count = %d, want 1 (only active Research item)", deepFetchCalls)
	}

	// Verify terminal flag is set for closed Done items.
	for i := 1; i <= 3; i++ {
		snap, err := eng.store.Get("owner/repo", i)
		if err != nil {
			t.Errorf("item #%d not in store: %v", i, err)
			continue
		}
		if !snap.IsTerminal() {
			t.Errorf("item #%d (closed Done): expected IsTerminal()=true after webhook startup", i)
		}
	}
}

// TestRunProbeAndDeepFetch_IsClosedPropagates_WithoutDeepFetch verifies that
// IsClosed=true is written to the store via ProbeBoardItemUpdated even when
// the item is cache-fresh (EffectiveUpdatedAt unchanged → no deep-fetch).
func TestRunProbeAndDeepFetch_IsClosedPropagates_WithoutDeepFetch(t *testing.T) {
	T1 := time.Now().Add(-time.Hour).Truncate(time.Second)
	var deepFetchCalls int
	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo", Status: "Research", IsClosed: true, EffectiveUpdatedAt: T1},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	// Fresh at T1 — deep-fetch should not be triggered.
	eng.store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 1,
		FreshState: gh.ProjectItem{
			ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", UpdatedAt: T1,
		},
	})
	eng.runProbeAndDeepFetch(cache)
	if deepFetchCalls != 0 {
		t.Errorf("expected 0 deep-fetch calls for fresh item; got %d", deepFetchCalls)
	}
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get after probe: %v", err)
	}
	if !snap.IsClosed() {
		t.Error("expected IsClosed=true after ProbeBoardItemUpdated; got false")
	}
}

// TestProbeNewItem_ClosedDone_SkipsDeepFetch is a regression test for the
// new-item branch of runProbeAndDeepFetch. It verifies that closed Done items
// discovered by the probe (not yet in store, no prior bootstrap) are seeded as
// terminal and never deep-fetched, while open active items are deep-fetched
// normally. This covers the gap where BootstrapFromProbe cannot help: items
// that appear in the probe for the first time during a mid-run cycle.
func TestProbeNewItem_ClosedDone_SkipsDeepFetch(t *testing.T) {
	const numClosed = 3 // closed Done items
	const numOpen = 2   // open Research items
	var deepFetchCalls int
	probeTime := time.Now().Add(-time.Minute)

	var probeItems []gh.BoardProbeItem
	for i := 1; i <= numClosed; i++ {
		probeItems = append(probeItems, gh.BoardProbeItem{
			ContentID: fmt.Sprintf("I_%03d", i), ItemID: fmt.Sprintf("PVTI_%03d", i),
			Number:             i,
			Repo:               "owner/repo",
			Status:             "Done",
			IsClosed:           true,
			EffectiveUpdatedAt: probeTime,
		})
	}
	for i := numClosed + 1; i <= numClosed+numOpen; i++ {
		probeItems = append(probeItems, gh.BoardProbeItem{
			ContentID: fmt.Sprintf("I_%03d", i), ItemID: fmt.Sprintf("PVTI_%03d", i),
			Number:             i,
			Repo:               "owner/repo",
			Status:             "Research",
			IsClosed:           false,
			EffectiveUpdatedAt: probeTime,
		})
	}

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return probeItems, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", ProjectNum: 1,
			User: "testuser", Token: "token", MaxConcurrent: 5,
			Stages: testStagesWithCleanup(),
		},
		client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	eng.readClient = cache

	// No prior bootstrap — store is empty. All items are new-item discoveries.
	eng.runProbeAndDeepFetch(cache)

	// Only the open Research items should have been deep-fetched.
	if deepFetchCalls != numOpen {
		t.Errorf("deep-fetch count = %d, want %d (open items only)", deepFetchCalls, numOpen)
	}

	// Closed Done items must be terminal in the store.
	for i := 1; i <= numClosed; i++ {
		snap, snapErr := eng.store.Get("owner/repo", i)
		if snapErr != nil {
			t.Errorf("closed Done item #%d not found in store", i)
			continue
		}
		if !snap.IsTerminal() {
			t.Errorf("item #%d (closed Done): expected IsTerminal()=true", i)
		}
	}

	// Open Research items must NOT be terminal.
	for i := numClosed + 1; i <= numClosed+numOpen; i++ {
		snap, snapErr := eng.store.Get("owner/repo", i)
		if snapErr != nil {
			t.Errorf("open Research item #%d not found in store", i)
			continue
		}
		if snap.IsTerminal() {
			t.Errorf("item #%d (open Research): expected IsTerminal()=false", i)
		}
	}
}

// captureStdout redirects os.Stdout to a pipe for the duration of fn,
// then returns everything written to stdout. The original os.Stdout is
// restored before returning.
func captureStdout(fn func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	var buf strings.Builder
	io.Copy(&buf, r)
	return buf.String()
}

func TestCheckAllowAutoMerge_DisabledEmitsWarning(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	client := &mockGitHubClient{
		fetchAllowAutoMergeFn: func(owner, repo string) (bool, error) {
			return false, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	out := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})

	if !strings.Contains(out, "WARNING") {
		t.Errorf("expected WARNING in output; got: %q", out)
	}
	if !strings.Contains(out, "allow_auto_merge") {
		t.Errorf("expected allow_auto_merge mention in output; got: %q", out)
	}
	if !strings.Contains(out, "gh api -X PATCH repos/owner/repo") {
		t.Errorf("expected fix command in output; got: %q", out)
	}
}

func TestCheckAllowAutoMerge_EnabledIsSilent(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	client := &mockGitHubClient{
		fetchAllowAutoMergeFn: func(owner, repo string) (bool, error) {
			return true, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	out := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})

	if out != "" {
		t.Errorf("expected no output for enabled repo; got: %q", out)
	}
}

func TestCheckAllowAutoMerge_APIErrorIsNonFatal(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	client := &mockGitHubClient{
		fetchAllowAutoMergeFn: func(owner, repo string) (bool, error) {
			return false, errors.New("network error")
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Should not panic; engine should log the error at warn level and continue.
	out := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})

	// No WARNING block should be emitted for an API error.
	if strings.Contains(out, "WARNING") {
		t.Errorf("should not print WARNING on API error; got: %q", out)
	}
}

func TestCheckAllowAutoMerge_DedupSuppressesSecondCall(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	var callCount int
	client := &mockGitHubClient{
		fetchAllowAutoMergeFn: func(owner, repo string) (bool, error) {
			callCount++
			return false, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// First call should emit warning.
	out1 := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})
	if !strings.Contains(out1, "WARNING") {
		t.Errorf("first call: expected WARNING; got: %q", out1)
	}

	// Second call for the same repo should be a no-op.
	out2 := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})
	if out2 != "" {
		t.Errorf("second call: expected no output (dedup); got: %q", out2)
	}
	if callCount != 1 {
		t.Errorf("expected API to be called exactly once; got %d", callCount)
	}
}

// TestPoll_InFlightWorker_NotSupplanted is a regression test for the
// dispatch → label → webhook → poll → cancel feedback loop introduced by the
// kill-reason propagation work and observed on 2026-06-15: every stage that
// adds stage:X:in_progress fires a webhook, marks the cache stale, and triggers
// a re-poll while the worker is still running. If the dispatch loop cancels the
// in-flight context on every re-encounter, no stage can complete a turn.
//
// This test asserts: when poll() encounters an item whose Store has a
// registered Worker AND whose issueCtxs entry is live, the entry's context is
// NOT cancelled by the poll cycle.
func TestPoll_InFlightWorker_NotSupplanted(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number:    42,
						ItemID:    "PVTI_42",
						Status:    "Validate",
						Repo:      "owner/repo",
						UpdatedAt: time.Now(),
						Labels:    []string{"stage:Validate:in_progress", "fabrik:yolo"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { return nil },
	}

	eng := NewWithDeps(Config{
		Owner:         "owner",
		Repo:          "repo",
		ProjectNum:    1,
		User:          "testuser",
		Token:         "token",
		MaxConcurrent: 5,
		PollSeconds:   1,
		Stages:        testStagesWithValidate(),
	}, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	// Simulate an in-flight worker: WorkerEntered in the Store + a live
	// issueCtxs entry with a cancellable context. This is the exact state poll()
	// will see during a stage's mid-flight on the next poll cycle.
	eng.store.Apply(itemstate.WorkerEntered{
		Repo:      "owner/repo",
		Number:    42,
		StageName: "Validate",
		StartedAt: time.Now(),
	})
	holder := &killReasonHolder{}
	iCtx, iCancel := context.WithCancel(context.Background())
	defer iCancel()
	eng.issueCtxs.Store("owner/repo#42", issueCtxEntry{cancel: iCancel, holder: holder})

	// Run one poll.
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// The in-flight context must NOT have been cancelled by the dispatch loop.
	select {
	case <-iCtx.Done():
		t.Fatal("poll cancelled the in-flight context — supplant feedback loop regression")
	default:
		// expected: context still live
	}

	// The kill-reason holder must also NOT have been annotated with
	// supplant_by_new_invocation; this prevents an unrelated later cancel
	// (e.g. daemon shutdown) from being mislabeled.
	if v, ok := holder.val.Load().(string); ok && v == "supplant_by_new_invocation" {
		t.Errorf("holder reason annotated to %q by poll — should be untouched while in-flight worker still running", v)
	}
}

// TestSHAInvalidation_ReDispatchesValidateOnForcePush verifies SC-5: a force-push
// simulation (PRHeadSHAUpdated with a new SHA on an item that has
// stage:Validate:complete + a recorded completion SHA) triggers the
// SHA-invalidation scan, clears all FR-3 labels, and leaves the item in a
// state where itemNeedsWork returns true on the next poll.
func TestSHAInvalidation_ReDispatchesValidateOnForcePush(t *testing.T) {
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 3, Prompt: "implement"},
		{Name: "Validate", Order: 4, Prompt: "validate"},
	}
	allLabels := []string{
		"stage:Validate:complete",
		"fabrik:auto-merge-enabled",
		"fabrik:awaiting-ci",
		"fabrik:awaiting-review",
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 99,
						ItemID: "PVTI_99",
						Status: "Validate",
						Repo:   "owner/repo",
						Labels: allLabels,
					},
				},
			}, nil
		},
	}
	eng := testEngineWithStages(t, client, stgs)

	// Simulate Validate completing at "sha-N", then a force-push arriving at "sha-M".
	eng.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        "owner/repo",
		Number:      99,
		LinkedPRNum: 200,
		SHA:         "sha-M",
	})
	eng.store.Apply(itemstate.ValidateCompletedAtSHA{
		Repo:   "owner/repo",
		Number: 99,
		SHA:    "sha-N",
	})

	// Poll: SHA-invalidation scan fires and clears all FR-3 labels.
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// Assert all four FR-3 labels were removed from GitHub.
	client.mu.Lock()
	removed := make(map[string]bool)
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 99 {
			removed[c.labelName] = true
		}
	}
	client.mu.Unlock()
	for _, lbl := range allLabels {
		if !removed[lbl] {
			t.Errorf("force-push: expected label %q to be removed, but it was not", lbl)
		}
	}

	// After label clearance, itemNeedsWork must return true for the cleaned item
	// so the next poll dispatches Validate.
	cleanedItem := gh.ProjectItem{
		Number: 99,
		ItemID: "PVTI_99",
		Status: "Validate",
		Repo:   "owner/repo",
		Labels: []string{}, // all blocking labels cleared
	}
	if !eng.itemNeedsWork(cleanedItem) {
		t.Error("expected itemNeedsWork to return true after SHA-invalidation scan cleared labels")
	}
}

// TestPhase2ValidateCatchup_PRAlreadyMerged_AdvancesToDone verifies that when
// EnablePullRequestAutoMerge returns ErrAutoMergeNotEnabled in the Phase 2
// Validate catch-up, the engine detects the PR is already merged and calls
// advanceToNextStage inline — board advances to Done without waiting for
// checkAutoMergeConvergence (which would never fire, as fabrik:auto-merge-enabled
// is never added in this path).
func TestPhase2ValidateCatchup_PRAlreadyMerged_AdvancesToDone(t *testing.T) {
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
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "sha1", Merged: true}, nil
		},
		enablePullRequestAutoMergeFn: func(owner, repo string, prNumber int, strategy string) error {
			return gh.ErrAutoMergeNotEnabled
		},
	}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	// Seed item into mayNeedWork so the pre-filter admits it.
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#42"] = true
	eng.mayNeedWorkMu.Unlock()

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	advances := len(client.updateStatusCalls)
	client.mu.Unlock()

	if advances != 1 {
		t.Errorf("expected 1 UpdateProjectItemStatus call (advance to Done), got %d", advances)
	}
}

// TestConvergencePausedRecovery_PRMerged_AdvancesToDone verifies that an item
// with fabrik:paused + stage:Validate:complete (no fabrik:awaiting-ci) is
// advanced to Done and unparsed when the linked PR is confirmed merged.
func TestConvergencePausedRecovery_PRMerged_AdvancesToDone(t *testing.T) {
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
						Labels: []string{"stage:Validate:complete", "fabrik:paused"},
					},
				},
			}, nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "sha1", Merged: true}, nil
		},
	}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#55"] = true
	eng.mayNeedWorkMu.Unlock()

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	advances := len(client.updateStatusCalls)
	var removedPaused bool
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 55 && c.labelName == "fabrik:paused" {
			removedPaused = true
		}
	}
	client.mu.Unlock()

	if advances != 1 {
		t.Errorf("expected 1 UpdateProjectItemStatus call (advance to Done), got %d", advances)
	}
	if !removedPaused {
		t.Error("expected fabrik:paused to be removed from issue #55")
	}
}

// TestConvergencePausedRecovery_PRNotMerged_NoAdvance verifies that a
// convergence-paused item is NOT advanced when the linked PR is not yet merged.
func TestConvergencePausedRecovery_PRNotMerged_NoAdvance(t *testing.T) {
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
						Labels: []string{"stage:Validate:complete", "fabrik:paused"},
					},
				},
			}, nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "sha1", Merged: false}, nil
		},
	}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#55"] = true
	eng.mayNeedWorkMu.Unlock()

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	advances := len(client.updateStatusCalls)
	var removedPaused bool
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 55 && c.labelName == "fabrik:paused" {
			removedPaused = true
		}
	}
	client.mu.Unlock()

	if advances != 0 {
		t.Errorf("expected no UpdateProjectItemStatus when PR is not merged, got %d", advances)
	}
	if removedPaused {
		t.Error("fabrik:paused must NOT be removed when PR is not merged")
	}
}

// TestCIAndReviewGate_JointClearingHandoff verifies the two-poll CI→review
// handoff sequence for stages with both wait_for_ci: true and
// wait_for_reviews: true.
//
// Poll 1: item has fabrik:awaiting-ci (no stage:X:complete). fetchLinkedPRFn
// returns nil → R5 (no CI configured) → checkCIGate calls
// addCompleteLabelAndRemoveCI: stage:Validate:complete added,
// fabrik:awaiting-ci removed.
//
// Poll 2: board returns stage:Validate:complete (no fabrik:awaiting-ci).
// fetchItemDetailsFn returns an outstanding reviewer. handleReviewGate guard
// (pctx.hasComplete == true) passes → checkReviewGate adds
// fabrik:awaiting-review.
func TestCIAndReviewGate_JointClearingHandoff(t *testing.T) {
	trueVal := true
	stgs := []*stages.Stage{
		{Name: "Validate", Order: 1, Prompt: "validate", WaitForCI: &trueVal, WaitForReviews: &trueVal},
	}

	var pollCount int32

	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			n := atomic.AddInt32(&pollCount, 1)
			var labels []string
			if n == 1 {
				// Poll 1: CI-await window; stage:X:complete absent.
				labels = []string{"fabrik:awaiting-ci"}
			} else {
				// Poll 2: CI gate already cleared; stage:X:complete present.
				labels = []string{"stage:Validate:complete"}
			}
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 30,
						ItemID: "PVTI_30",
						Status: "Validate",
						Repo:   "owner/repo",
						Labels: labels,
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			// Simulate one outstanding reviewer on every deep-fetch. On Poll 1
			// handleReviewGate is skipped entirely (guard blocks it); on Poll 2
			// handleReviewGate fires and reads this reviewer.
			item.LinkedPRReviewRequests = []gh.ReviewRequest{{Login: "reviewer-bot", IsBot: true}}
			return nil
		},
		// No linked PR → CI gate clears immediately (R5: no CI configured).
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
	}

	eng := testEngineWithStages(t, client, stgs)

	ctx := context.Background()

	// ── Poll 1 ──────────────────────────────────────────────────────────────
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll 1: %v", err)
	}

	client.mu.Lock()
	var poll1AddedComplete, poll1RemovedCI bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			poll1AddedComplete = true
		}
		if c.labelName == "fabrik:awaiting-review" {
			t.Errorf("poll 1: handleReviewGate must not run during CI-await window — spuriously added fabrik:awaiting-review")
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			poll1RemovedCI = true
		}
	}
	// Reset accumulators before poll 2.
	client.addLabelCalls = nil
	client.removeLabelCalls = nil
	client.mu.Unlock()

	if !poll1AddedComplete {
		t.Error("poll 1: expected checkCIGate to add stage:Validate:complete when CI clears (R5)")
	}
	if !poll1RemovedCI {
		t.Error("poll 1: expected checkCIGate to remove fabrik:awaiting-ci when CI clears (R5)")
	}

	// ── Poll 2 ──────────────────────────────────────────────────────────────
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll 2: %v", err)
	}

	client.mu.Lock()
	var poll2AddedReview bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-review" {
			poll2AddedReview = true
		}
	}
	client.mu.Unlock()

	if !poll2AddedReview {
		t.Error("poll 2: expected handleReviewGate to add fabrik:awaiting-review for outstanding reviewer after CI clears")
	}
}

func TestHandleMergeTrainBatch_LogsQueuedItems(t *testing.T) {
	// Use a holding stage named "BatchHold" (not "Queued") to verify that
	// handleMergeTrainBatch is driven by the HoldingStage field, not a hardcoded name.
	client := &mockGitHubClient{}
	stgs := testStagesWithValidateAndHolding()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MergeTrain = "on"

	events := make(chan tui.Event, 10)
	eng.events = events

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 42, Title: "fix the bug",    Status: "BatchHold"},
			{Number: 43, Title: "add the feature", Status: "Implement"},
			{Number: 44, Title: "another ready",   Status: "BatchHold"},
		},
	}

	eng.handleMergeTrainBatch(context.Background(), board)

	var logged []tui.LogEvent
	for len(events) > 0 {
		ev := <-events
		if le, ok := ev.(tui.LogEvent); ok {
			logged = append(logged, le)
		}
	}

	if len(logged) == 0 {
		t.Fatal("expected at least one log event from handleMergeTrainBatch, got none")
	}
	msg := logged[0].Message
	if !strings.Contains(msg, "batch snapshot for owner/repo: 2 item(s)") {
		t.Errorf("expected 'batch snapshot for owner/repo: 2 item(s)' in log message, got: %q", msg)
	}
	if !strings.Contains(msg, "#42") || !strings.Contains(msg, "#44") {
		t.Errorf("expected both holding-stage issue numbers in log message, got: %q", msg)
	}
	if strings.Contains(msg, "#43") {
		t.Errorf("non-holding-stage item #43 must not appear in batch snapshot, got: %q", msg)
	}
	if logged[0].Tag != "merge-train" {
		t.Errorf("expected tag 'merge-train', got %q", logged[0].Tag)
	}
}

func TestHandleMergeTrainBatch_SilentWhenEmpty(t *testing.T) {
	// Use a holding stage so the engine has a configured holding column; none of the
	// board items have that status — the batch snapshot must be silent.
	client := &mockGitHubClient{}
	stgs := testStagesWithValidateAndHolding()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MergeTrain = "on"

	events := make(chan tui.Event, 10)
	eng.events = events

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 10, Title: "in progress", Status: "Implement"},
			{Number: 11, Title: "reviewing",   Status: "Review"},
		},
	}

	eng.handleMergeTrainBatch(context.Background(), board)

	if len(events) != 0 {
		t.Errorf("expected no log events when holding stage column is empty, got %d", len(events))
	}
}

// drainLogMessages collects all LogEvent messages currently buffered on the channel.
func drainLogMessages(events chan tui.Event) []string {
	var msgs []string
	for len(events) > 0 {
		if le, ok := (<-events).(tui.LogEvent); ok {
			msgs = append(msgs, le.Message)
		}
	}
	return msgs
}

func anyContains(msgs []string, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// fillSem fills the engine semaphore to capacity so any merge-train worker goroutine
// launched by dispatchMergeTrainWorker parks at its select (sem full, ctx live) instead
// of running real git work — keeping routing tests hermetic while still proving that
// the LoadOrStore registration (which happens synchronously, before the goroutine) fired.
func fillSem(eng *Engine) {
	for i := 0; i < cap(eng.sem); i++ {
		eng.sem <- struct{}{}
	}
}

// TestHandleMergeTrainBatch_QueueEnabledRepo_Enqueues is the ADR-059 D6 FR-1 routing
// assertion for a queue-enabled repo: Queued items take the ADR-058 enqueue path, NOT the
// internal train. EnqueuePullRequest is called with the poll-native linked-PR SHA,
// fabrik:auto-merge-enabled is applied, and no train worker is registered.
func TestHandleMergeTrainBatch_QueueEnabledRepo_Enqueues(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidateAndHolding())
	eng.cfg.MergeTrain = "on"

	events := make(chan tui.Event, 20)
	eng.events = events

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 42, Title: "a", Status: "BatchHold", Repo: "owner/repo",
				LinkedPRIsMergeQueueEnabled: true, LinkedPRNumber: 142, LinkedPRHeadSHA: "sha42"},
			{Number: 43, Title: "b", Status: "BatchHold", Repo: "owner/repo",
				LinkedPRIsMergeQueueEnabled: true, LinkedPRNumber: 143, LinkedPRHeadSHA: "sha43"},
		},
	}

	eng.handleMergeTrainBatch(context.Background(), board)

	if len(client.enqueuePullRequestCalls) != 2 {
		t.Fatalf("expected 2 EnqueuePullRequest calls (058 path), got %d", len(client.enqueuePullRequestCalls))
	}
	if client.enqueuePullRequestCalls[0].prNumber != 142 || client.enqueuePullRequestCalls[0].expectedHeadOID != "sha42" {
		t.Errorf("enqueue #0 = PR %d @ %q, want 142 @ sha42", client.enqueuePullRequestCalls[0].prNumber, client.enqueuePullRequestCalls[0].expectedHeadOID)
	}
	// fabrik:auto-merge-enabled applied to both items (idempotency + convergence anchor).
	var labelAdds int
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			labelAdds++
		}
	}
	if labelAdds != 2 {
		t.Errorf("expected 2 fabrik:auto-merge-enabled label adds, got %d", labelAdds)
	}
	// No internal train worker for a queue-enabled repo.
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("queue-enabled repo must NOT dispatch an internal train worker")
	}
	if msgs := drainLogMessages(events); anyContains(msgs, "batch snapshot") {
		t.Errorf("queue-enabled repo must not log an internal-train batch snapshot; got %v", msgs)
	}
}

// TestHandleMergeTrainBatch_NonQueueRepo_DispatchesTrain is the FR-1 routing assertion for
// a non-queue repo: Queued items take the internal merge train, NOT the enqueue path. A train
// worker is registered in mergeTrainInFlight and EnqueuePullRequest is never called.
func TestHandleMergeTrainBatch_NonQueueRepo_DispatchesTrain(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidateAndHolding())
	eng.cfg.MergeTrain = "on"
	fillSem(eng) // park the worker goroutine at the semaphore; no real git work.

	events := make(chan tui.Event, 20)
	eng.events = events

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 50, Title: "x", Status: "BatchHold", Repo: "owner/repo", LinkedPRIsMergeQueueEnabled: false},
			{Number: 51, Title: "y", Status: "BatchHold", Repo: "owner/repo", LinkedPRIsMergeQueueEnabled: false},
		},
	}

	eng.handleMergeTrainBatch(context.Background(), board)

	// Train worker registered (LoadOrStore fires synchronously before the goroutine).
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); !ok {
		t.Error("non-queue repo must dispatch an internal train worker (mergeTrainInFlight entry expected)")
	}
	if len(client.enqueuePullRequestCalls) != 0 {
		t.Errorf("non-queue repo must NOT call EnqueuePullRequest, got %d", len(client.enqueuePullRequestCalls))
	}
	if msgs := drainLogMessages(events); !anyContains(msgs, "batch snapshot for owner/repo: 2 item(s)") {
		t.Errorf("expected internal-train batch snapshot for the repo; got %v", msgs)
	}
}

// TestHandleMergeTrainBatch_MixedRepoBatch_RoutesPerRepo is the FR-1 D-3 mixed-repo assertion:
// a Queued column holding items from a queue-enabled repo A and a non-queue repo B routes each
// repo's subset to the correct engine — A enqueues, B trains — and B's items never enter A's batch.
func TestHandleMergeTrainBatch_MixedRepoBatch_RoutesPerRepo(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidateAndHolding())
	eng.cfg.MergeTrain = "on"
	fillSem(eng)

	events := make(chan tui.Event, 30)
	eng.events = events

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			// Repo A: queue-enabled -> enqueue.
			{Number: 60, Title: "a1", Status: "BatchHold", Repo: "owner/repo-a",
				LinkedPRIsMergeQueueEnabled: true, LinkedPRNumber: 160, LinkedPRHeadSHA: "shaA"},
			// Repo B: non-queue -> internal train.
			{Number: 70, Title: "b1", Status: "BatchHold", Repo: "owner/repo-b", LinkedPRIsMergeQueueEnabled: false},
			{Number: 71, Title: "b2", Status: "BatchHold", Repo: "owner/repo-b", LinkedPRIsMergeQueueEnabled: false},
		},
	}

	eng.handleMergeTrainBatch(context.Background(), board)

	// Repo A: exactly one enqueue (its single item), no train worker.
	if len(client.enqueuePullRequestCalls) != 1 || client.enqueuePullRequestCalls[0].prNumber != 160 {
		t.Fatalf("expected exactly one enqueue for repo A PR #160, got %+v", client.enqueuePullRequestCalls)
	}
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo-a"); ok {
		t.Error("queue-enabled repo A must NOT dispatch an internal train worker")
	}
	// Repo B: train worker registered, and its snapshot mentions only B's items (#70,#71) — never A's #60.
	if _, ok := eng.mergeTrainInFlight.Load("owner/repo-b"); !ok {
		t.Error("non-queue repo B must dispatch an internal train worker")
	}
	msgs := drainLogMessages(events)
	if !anyContains(msgs, "batch snapshot for owner/repo-b: 2 item(s)") {
		t.Errorf("expected repo B train snapshot with 2 items; got %v", msgs)
	}
	for _, m := range msgs {
		if strings.Contains(m, "batch snapshot") && strings.Contains(m, "#60") {
			t.Errorf("repo A's item #60 must never appear in an internal-train batch snapshot; got %q", m)
		}
	}
}

// TestHandleMergeTrainBatch_Idempotency_SkipsAlreadyEnqueued verifies the FR-1 idempotency
// guard: a queue-enabled Queued item already carrying fabrik:auto-merge-enabled is mid-
// convergence and must not be re-enqueued.
func TestHandleMergeTrainBatch_Idempotency_SkipsAlreadyEnqueued(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidateAndHolding())
	eng.cfg.MergeTrain = "on"

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 80, Title: "already", Status: "BatchHold", Repo: "owner/repo",
				LinkedPRIsMergeQueueEnabled: true, LinkedPRNumber: 180, LinkedPRHeadSHA: "sha80",
				Labels: []string{"fabrik:auto-merge-enabled"}},
		},
	}

	eng.handleMergeTrainBatch(context.Background(), board)

	if len(client.enqueuePullRequestCalls) != 0 {
		t.Errorf("item already carrying fabrik:auto-merge-enabled must not be re-enqueued, got %d enqueue call(s)", len(client.enqueuePullRequestCalls))
	}
}

// TestHandleMergeTrainBatch_QueueEnabled_CacheMissSkips verifies the FR-1 poll-native
// cache-miss guard: a queue-enabled item missing its linked-PR number/head SHA is skipped
// this poll (no enqueue, no REST fetch) and retried next.
func TestHandleMergeTrainBatch_QueueEnabled_CacheMissSkips(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidateAndHolding())
	eng.cfg.MergeTrain = "on"

	events := make(chan tui.Event, 10)
	eng.events = events

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 90, Title: "no-sha", Status: "BatchHold", Repo: "owner/repo",
				LinkedPRIsMergeQueueEnabled: true, LinkedPRNumber: 190, LinkedPRHeadSHA: ""},
		},
	}

	eng.handleMergeTrainBatch(context.Background(), board)

	if len(client.enqueuePullRequestCalls) != 0 {
		t.Errorf("cache-miss item (empty head SHA) must not be enqueued, got %d", len(client.enqueuePullRequestCalls))
	}
	if msgs := drainLogMessages(events); !anyContains(msgs, "missing poll-native linked-PR state") {
		t.Errorf("expected cache-miss skip log; got %v", msgs)
	}
}

// TestHandleMergeTrainBatch_PerGroupMaxBatchSize verifies FR-4 as realized in D6: max_batch_size
// caps each repo's internal-train subset INDEPENDENTLY, not the flat cross-repo batch.
func TestHandleMergeTrainBatch_PerGroupMaxBatchSize(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidateAndHolding())
	eng.cfg.MergeTrain = "on"
	eng.cfg.MaxBatchSize = 2
	fillSem(eng)

	events := make(chan tui.Event, 40)
	eng.events = events

	// Repo A: 3 non-queue items (exceeds cap 2). Repo B: 1 non-queue item (under cap).
	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 1, Title: "a1", Status: "BatchHold", Repo: "owner/repo-a"},
			{Number: 2, Title: "a2", Status: "BatchHold", Repo: "owner/repo-a"},
			{Number: 3, Title: "a3", Status: "BatchHold", Repo: "owner/repo-a"},
			{Number: 4, Title: "b1", Status: "BatchHold", Repo: "owner/repo-b"},
		},
	}

	eng.handleMergeTrainBatch(context.Background(), board)

	msgs := drainLogMessages(events)
	// Repo A capped to 2 (its own group), logged explicitly (never silent).
	if !anyContains(msgs, "batch capped for owner/repo-a") {
		t.Errorf("expected per-group cap log for repo A; got %v", msgs)
	}
	if !anyContains(msgs, "batch snapshot for owner/repo-a: 2 item(s)") {
		t.Errorf("expected repo A snapshot of 2 items after cap; got %v", msgs)
	}
	// Repo B (1 item) is under the cap — no cap log, snapshot of 1.
	if anyContains(msgs, "batch capped for owner/repo-b") {
		t.Errorf("repo B (1 item) must not be capped; got %v", msgs)
	}
	if !anyContains(msgs, "batch snapshot for owner/repo-b: 1 item(s)") {
		t.Errorf("expected repo B snapshot of 1 item; got %v", msgs)
	}
}

// TestGroupQueuedByRepo verifies the D-3 grouping helper: only holding-stage items are
// collected, grouped by owner/repo, preserving first-seen repo order and per-repo entry order.
func TestGroupQueuedByRepo(t *testing.T) {
	items := []gh.ProjectItem{
		{Number: 1, Status: "BatchHold", Repo: "owner/repo-b"},
		{Number: 2, Status: "Implement", Repo: "owner/repo-a"}, // not holding — excluded
		{Number: 3, Status: "BatchHold", Repo: "owner/repo-a"},
		{Number: 4, Status: "BatchHold", Repo: "owner/repo-b"},
		{Number: 5, Status: "BatchHold", Repo: ""}, // defaults to owner/repo
	}
	groups := groupQueuedByRepo(items, "BatchHold", "owner/repo")

	if len(groups) != 3 {
		t.Fatalf("expected 3 repo groups, got %d: %+v", len(groups), groups)
	}
	// First-seen order: repo-b, repo-a, then the default repo.
	if groups[0].repoKey != "owner/repo-b" || groups[1].repoKey != "owner/repo-a" || groups[2].repoKey != "owner/repo" {
		t.Errorf("unexpected group order: %q, %q, %q", groups[0].repoKey, groups[1].repoKey, groups[2].repoKey)
	}
	// repo-b keeps entry order #1 then #4; the non-holding #2 is excluded.
	if len(groups[0].items) != 2 || groups[0].items[0].Number != 1 || groups[0].items[1].Number != 4 {
		t.Errorf("repo-b group wrong: %+v", groups[0].items)
	}
	if len(groups[1].items) != 1 || groups[1].items[0].Number != 3 {
		t.Errorf("repo-a group wrong: %+v", groups[1].items)
	}
}

// TestReconcileLoop_RunsWithoutWebhookManager is the #955 regression for the
// architectural fix: the reconcile ticker must run — and repair label drift — even
// when the webhook manager is nil (webhooks disabled or wm.Start failed).
// Previously the ticker was nested inside the webhook-start block, so a webhook-less
// deployment never reconciled and could not self-heal a drifted fabrik label set,
// stranding items at gates (e.g. fabrik:awaiting-ci missing from the store forever).
func TestReconcileLoop_RunsWithoutWebhookManager(t *testing.T) {
	t1 := time.Now().Truncate(time.Second)
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.ReconcileInterval = 10 * time.Millisecond

	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	// Seed #1 at Validate WITHOUT fabrik:awaiting-ci — the drifted store state.
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Validate", Labels: []string{"fabrik:cruise"}, UpdatedAt: t1},
		},
	})
	eng.readClient = cache

	// GitHub's fresh board: same status + updatedAt, but WITH fabrik:awaiting-ci.
	// Only the fabrik-managed label differs — exactly the #1479 stranding condition.
	client.fetchProjectBoardFn = func(_, _ string, _ int, _ string) (*gh.ProjectBoard, error) {
		return &gh.ProjectBoard{
			ProjectID: "PVT_1",
			Items: []gh.ProjectItem{
				{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Validate", Labels: []string{"fabrik:cruise", "fabrik:awaiting-ci"}, UpdatedAt: t1},
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// nil webhook manager — the whole point: reconcile must run anyway.
	go eng.reconcileLoop(ctx, cache, nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		snap, err := eng.store.Get("owner/repo", 1)
		if err == nil {
			for _, l := range snap.Labels() {
				if l == "fabrik:awaiting-ci" {
					return // success: reconcile ran with nil wm and synced the gate label
				}
			}
		}
		if time.Now().After(deadline) {
			var labels []string
			if err == nil {
				labels = snap.Labels()
			}
			t.Fatalf("reconcileLoop did not sync fabrik:awaiting-ci within 2s (nil wm); labels = %v", labels)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
