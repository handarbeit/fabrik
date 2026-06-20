package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/tui"
)

// TestHandleStopRequest_CancelsCorrectIssue verifies that stop cancels the
// targeted issue's context while leaving another in-flight issue unaffected.
func TestHandleStopRequest_CancelsCorrectIssue(t *testing.T) {
	client := &mockGitHubClient{}
	client.addLabelToIssueFn = func(owner, repo string, issueNumber int, label string) error { return nil }
	client.addCommentFn = func(owner, repo string, issueNumber int, body string) (int, error) { return 1, nil }

	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Register two in-flight per-issue contexts.
	holder42 := &killReasonHolder{}
	ctx42, cancel42 := context.WithCancel(context.Background())
	defer cancel42()
	eng.issueCtxs.Store("owner/repo#42", issueCtxEntry{cancel: cancel42, holder: holder42})

	holder99 := &killReasonHolder{}
	ctx99, cancel99 := context.WithCancel(context.Background())
	defer cancel99()
	eng.issueCtxs.Store("owner/repo#99", issueCtxEntry{cancel: cancel99, holder: holder99})

	// Stop issue 42.
	eng.handleStopRequest(context.Background(), tui.StopRequest{
		IssueNumber: 42,
		Repo:        "owner/repo",
		StageName:   "Implement",
	})

	// Issue 42's context must be cancelled.
	select {
	case <-ctx42.Done():
		// ok
	default:
		t.Error("expected ctx42 to be cancelled after stop")
	}
	reason, _ := holder42.val.Load().(string)
	if reason != "user_stop" {
		t.Errorf("holder42 reason = %q, want %q", reason, "user_stop")
	}

	// Issue 99's context must remain alive.
	select {
	case <-ctx99.Done():
		t.Error("expected ctx99 to still be alive — stop should not affect other workers")
	default:
		// ok
	}

	// Labels and comment must have been posted for issue 42.
	client.mu.Lock()
	defer client.mu.Unlock()

	labels := make([]string, 0, len(client.addLabelCalls))
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 42 {
			labels = append(labels, c.labelName)
		}
	}
	if !containsString(labels, "fabrik:paused") {
		t.Errorf("expected fabrik:paused label for issue 42; got %v", labels)
	}
	if !containsString(labels, "fabrik:awaiting-input") {
		t.Errorf("expected fabrik:awaiting-input label for issue 42; got %v", labels)
	}

	commentFound := false
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 42 && strings.Contains(c.body, "stopped from TUI") {
			commentFound = true
			if !strings.Contains(c.body, "Implement") {
				t.Errorf("comment should mention stage name 'Implement'; got: %q", c.body)
			}
		}
	}
	if !commentFound {
		t.Error("expected stop comment to be posted for issue 42")
	}
}

// TestHandleStopRequest_NoWorkerEntry verifies that when the worker has already
// exited (no issueCtxs entry), labels and comment are still applied.
func TestHandleStopRequest_NoWorkerEntry(t *testing.T) {
	client := &mockGitHubClient{}
	client.addLabelToIssueFn = func(owner, repo string, issueNumber int, label string) error { return nil }
	client.addCommentFn = func(owner, repo string, issueNumber int, body string) (int, error) { return 1, nil }

	eng := testEngine(t, client, &mockClaudeInvoker{})

	// No entry in issueCtxs — worker already exited.
	eng.handleStopRequest(context.Background(), tui.StopRequest{
		IssueNumber: 7,
		Repo:        "owner/repo",
		StageName:   "Plan",
	})

	client.mu.Lock()
	defer client.mu.Unlock()

	labels := make([]string, 0)
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 7 {
			labels = append(labels, c.labelName)
		}
	}
	if !containsString(labels, "fabrik:paused") {
		t.Errorf("expected fabrik:paused applied even without worker entry; got %v", labels)
	}
	if !containsString(labels, "fabrik:awaiting-input") {
		t.Errorf("expected fabrik:awaiting-input applied even without worker entry; got %v", labels)
	}

	commentFound := false
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 7 {
			commentFound = true
		}
	}
	if !commentFound {
		t.Error("expected stop comment even without worker entry")
	}
}

// TestHandleStopRequest_NilWebhookMgr verifies that stop works without panicking
// when webhookMgr is nil (non-webhook mode or test environments).
func TestHandleStopRequest_NilWebhookMgr(t *testing.T) {
	client := &mockGitHubClient{}
	client.addLabelToIssueFn = func(owner, repo string, issueNumber int, label string) error { return nil }
	client.addCommentFn = func(owner, repo string, issueNumber int, body string) (int, error) { return 1, nil }

	eng := testEngine(t, client, &mockClaudeInvoker{})
	// webhookMgr is nil by default in testEngine.
	if eng.webhookMgr != nil {
		t.Fatal("precondition: testEngine must not have webhookMgr set")
	}

	// Must not panic.
	eng.handleStopRequest(context.Background(), tui.StopRequest{
		IssueNumber: 5,
		Repo:        "owner/repo",
		StageName:   "Research",
	})
}

// TestHandleStopRequest_SingleRepoFallback verifies that when Repo is empty
// (single-repo mode), the engine's defaultRepo is used to derive the issue key.
func TestHandleStopRequest_SingleRepoFallback(t *testing.T) {
	client := &mockGitHubClient{}
	client.addLabelToIssueFn = func(owner, repo string, issueNumber int, label string) error { return nil }
	client.addCommentFn = func(owner, repo string, issueNumber int, body string) (int, error) { return 1, nil }

	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Register entry under default repo key (what poll.go would store in single-repo mode).
	holder := &killReasonHolder{}
	ctx55, cancel55 := context.WithCancel(context.Background())
	defer cancel55()
	eng.issueCtxs.Store("owner/repo#55", issueCtxEntry{cancel: cancel55, holder: holder})

	// Send stop request with empty Repo (single-repo mode).
	eng.handleStopRequest(context.Background(), tui.StopRequest{
		IssueNumber: 55,
		Repo:        "", // empty → falls back to defaultRepo()
		StageName:   "Validate",
	})

	// Context should be cancelled via the defaultRepo fallback.
	select {
	case <-ctx55.Done():
		// ok
	default:
		t.Error("expected ctx55 to be cancelled via defaultRepo fallback")
	}
}

// containsString is a small helper since we don't use testify.
func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
