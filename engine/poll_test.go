package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func TestPoll_FetchesBoardAndProcessesItems(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
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
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
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
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
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
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
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
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
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
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
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
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
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
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
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

// TestProcessedSetConcurrency verifies that concurrent access to processedSet
// via the mutex-protected methods does not cause data races.
