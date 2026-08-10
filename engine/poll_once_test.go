package engine

import (
	"context"
	"fmt"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestPollOnce_DelegatesToPoll confirms PollOnce actually drives a real poll
// cycle (board fetch, status field population) rather than being a no-op —
// the failure mode a stub delegation would silently pass.
func TestPollOnce_DelegatesToPoll(t *testing.T) {
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

	if eng.statusField != nil {
		t.Fatal("statusField should be unset before the first poll")
	}
	if err := eng.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if eng.statusField == nil {
		t.Error("PollOnce should have driven a real poll cycle (statusField unset — looks like a no-op stub)")
	}
}

// TestPollOnce_PropagatesError confirms PollOnce surfaces poll()'s error
// rather than swallowing it.
func TestPollOnce_PropagatesError(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return nil, fmt.Errorf("network error")
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	if err := eng.PollOnce(context.Background()); err == nil {
		t.Error("PollOnce should propagate poll()'s error")
	}
}

// TestPollOnce_DoesNotTouchRunPreamble confirms PollOnce leaves Run()'s
// process-global setup (the persistent log file handle and the package-level
// pollLogFile) untouched — the deliberate omission its doc comment
// describes. A regression that "restored" that preamble inside PollOnce would
// make pollLogFile non-nil after this call.
func TestPollOnce_DoesNotTouchRunPreamble(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	if eng.logFile != nil {
		t.Fatal("logFile should be unset for a NewWithDeps-constructed engine")
	}
	_ = eng.PollOnce(context.Background())
	if eng.logFile != nil {
		t.Error("PollOnce must not open/assign e.logFile — that is Run()'s preamble, deliberately not replicated")
	}
	if pollLogFile != nil {
		t.Error("PollOnce must not assign the package-level pollLogFile global — that is Run()'s preamble, deliberately not replicated")
	}
}
