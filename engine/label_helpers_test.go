package engine

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// --- removeEditingLabel tests ---

// TestRemoveEditingLabel_NonTransientNoRetry verifies that a non-transient error
// logs a warning immediately (no retry) and returns after exactly one attempt.
func TestRemoveEditingLabel_NonTransientNoRetry(t *testing.T) {
	orig := editingLabelRetryDelay
	editingLabelRetryDelay = 0
	t.Cleanup(func() { editingLabelRetryDelay = orig })
	var calls int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:editing" {
				calls++
				return errors.New("GitHub API returned 422: validation failed")
			}
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.removeEditingLabel("owner", "repo", 5)
	if calls != 1 {
		t.Errorf("expected exactly 1 call for non-transient error, got %d", calls)
	}
}

// TestRemoveEditingLabel_TransientRetrySucceeds verifies that a transient error
// retried twice followed by success results in exactly 3 calls and no panic.
func TestRemoveEditingLabel_TransientRetrySucceeds(t *testing.T) {
	orig := editingLabelRetryDelay
	editingLabelRetryDelay = 0
	t.Cleanup(func() { editingLabelRetryDelay = orig })
	var calls int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:editing" {
				calls++
				if calls < 3 {
					return fmt.Errorf("executing request: %w", &net.OpError{Op: "read", Net: "tcp"})
				}
				return nil
			}
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.removeEditingLabel("owner", "repo", 5)
	if calls != 3 {
		t.Errorf("expected 3 calls (2 transient then success), got %d", calls)
	}
}

// TestRemoveEditingLabel_TransientExhausted verifies that 3 consecutive transient
// errors exhaust the retry budget: exactly 3 calls are made and no panic occurs.
func TestRemoveEditingLabel_TransientExhausted(t *testing.T) {
	orig := editingLabelRetryDelay
	editingLabelRetryDelay = 0
	t.Cleanup(func() { editingLabelRetryDelay = orig })
	var calls int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:editing" {
				calls++
				return fmt.Errorf("executing request: %w", &net.OpError{Op: "read", Net: "tcp"})
			}
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.removeEditingLabel("owner", "repo", 5)
	if calls != 3 {
		t.Errorf("expected 3 calls on transient exhaustion, got %d", calls)
	}
}

// TestRemoveEditingLabel_ErrNotFoundSilent verifies that ErrNotFound is silently
// ignored (label already absent) — exactly one call, no panic.
func TestRemoveEditingLabel_ErrNotFoundSilent(t *testing.T) {
	orig := editingLabelRetryDelay
	editingLabelRetryDelay = 0
	t.Cleanup(func() { editingLabelRetryDelay = orig })
	var calls int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:editing" {
				calls++
				return gh.ErrNotFound
			}
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.removeEditingLabel("owner", "repo", 5)
	if calls != 1 {
		t.Errorf("expected exactly 1 call for ErrNotFound, got %d", calls)
	}
}

// --- isTransientError unit tests ---

func TestIsTransientError_NetOpError(t *testing.T) {
	err := fmt.Errorf("executing request: %w", &net.OpError{Op: "read", Net: "tcp"})
	if !isTransientError(err) {
		t.Error("expected net.OpError wrapped error to be transient")
	}
}

func TestIsTransientError_DirectNetOpError(t *testing.T) {
	err := &net.OpError{Op: "dial", Net: "tcp"}
	if !isTransientError(err) {
		t.Error("expected direct net.OpError to be transient")
	}
}

func TestIsTransientError_UnexpectedEOF(t *testing.T) {
	err := fmt.Errorf("reading body: %w", io.ErrUnexpectedEOF)
	if !isTransientError(err) {
		t.Error("expected io.ErrUnexpectedEOF wrapped error to be transient")
	}
}

func TestIsTransientError_GitHub5xx(t *testing.T) {
	err := fmt.Errorf("GitHub API returned 503: service unavailable")
	if !isTransientError(err) {
		t.Error("expected GitHub 5xx error to be transient")
	}
}

func TestIsTransientError_ConnectionReset(t *testing.T) {
	err := errors.New("read: connection reset by peer")
	if !isTransientError(err) {
		t.Error("expected connection reset error to be transient")
	}
}

func TestIsTransientError_IOTimeout(t *testing.T) {
	err := errors.New("i/o timeout")
	if !isTransientError(err) {
		t.Error("expected i/o timeout error to be transient")
	}
}

func TestIsTransientError_ErrNotFound(t *testing.T) {
	if isTransientError(gh.ErrNotFound) {
		t.Error("expected ErrNotFound to be non-transient")
	}
}

func TestIsTransientError_GitHub4xx(t *testing.T) {
	err := fmt.Errorf("GitHub API returned 422: validation failed")
	if isTransientError(err) {
		t.Error("expected GitHub 4xx error to be non-transient")
	}
}

func TestIsTransientError_PlainError(t *testing.T) {
	if isTransientError(errors.New("some random error")) {
		t.Error("expected plain error to be non-transient")
	}
}

func TestIsTransientError_Nil(t *testing.T) {
	if isTransientError(nil) {
		t.Error("expected nil to be non-transient")
	}
}

// --- other label helper tests (unchanged) ---

func TestRemoveLockLabel_NonNotFoundError_LogsWarning(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("network error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// non-ErrNotFound error should log a warning without panicking
	eng.removeLockLabel("owner", "repo", 5, "fabrik:locked:testuser")
}

func TestRemoveLockLabel_ErrNotFound_Ignored(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return gh.ErrNotFound
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// ErrNotFound should be silently ignored
	eng.removeLockLabel("owner", "repo", 5, "fabrik:locked:testuser")
}

func TestEnsureDraftPR_FindPRError_ReturnsZero(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 0, errors.New("api error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 1, Title: "Test"}
	prNum := eng.ensureDraftPR(item, "main")
	if prNum != 0 {
		t.Errorf("expected 0 on FindPRForIssue error, got %d", prNum)
	}
}

func TestEnsurePRLinksIssue_GetIssueBodyError_Logs(t *testing.T) {
	client := &mockGitHubClient{
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "", errors.New("not found")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// Should not panic when GetIssueBody fails
	eng.ensurePRLinksIssue(gh.ProjectItem{Number: 5}, 10)
}

func TestMarkPRReady_MarkReadyError_LogsWarning(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManagerWithRoot(repoDir, repoDir+"/.fabrik/worktrees")

	client := &mockGitHubClient{
		markPRReadyFn: func(owner, repo string, prNumber int) error {
			return errors.New("api error")
		},
	}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", User: "u", Token: "t", Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	item := gh.ProjectItem{Number: 7, Title: "test"}
	// Should not panic on MarkPRReady error
	eng.markPRReady(item, 55)
}

func TestAddFailedLabel_Success(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.addFailedLabel("owner", "repo", 9, "Research")

	if len(client.addLabelCalls) != 1 {
		t.Fatalf("expected 1 AddLabel call, got %d", len(client.addLabelCalls))
	}
	if client.addLabelCalls[0].labelName != fmt.Sprintf("stage:Research:failed") {
		t.Errorf("label = %q", client.addLabelCalls[0].labelName)
	}
}

func TestAddFailedLabel_ErrorLogsWarning(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("api error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// Should not panic
	eng.addFailedLabel("owner", "repo", 10, "Plan")
}

func TestRemoveFailedLabel_ErrorLogsWarning(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("api error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// Should not panic
	eng.removeFailedLabel("owner", "repo", 10, "Plan")
}

func TestRemoveInProgressLabel_ErrorLogsWarning(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("api error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// Should not panic
	eng.removeInProgressLabel("owner", "repo", 10, "Plan")
}
