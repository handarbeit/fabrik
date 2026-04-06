package engine

import (
	"context"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tui"
)

func TestSetEvents_ConfiguresChannel(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	ch := make(chan tui.Event, 4)
	eng.SetEvents(ch)

	if eng.events != ch {
		t.Error("SetEvents should set the events channel")
	}
}

func TestEmit_NilChannel_NoOp(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	// No channel set — emit should not panic
	eng.emit(tui.LogEvent{IssueNumber: 1, Tag: "test", Message: "hello"})
}

func TestEmit_WithChannel_SendsEvent(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	ch := make(chan tui.Event, 4)
	eng.events = ch

	eng.emit(tui.LogEvent{IssueNumber: 1, Tag: "test", Message: "hello"})

	select {
	case ev := <-ch:
		le, ok := ev.(tui.LogEvent)
		if !ok {
			t.Fatalf("expected LogEvent, got %T", ev)
		}
		if le.Message != "hello" {
			t.Errorf("message = %q, want %q", le.Message, "hello")
		}
	default:
		t.Error("expected event on channel")
	}
}

func TestCleanupLockedIssues_RemovesLabels(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})

	// Seed the lockedIssues map (keys are "owner/repo#N" format)
	eng.mu.Lock()
	eng.lockedIssues["owner/repo#10"] = true
	eng.lockedIssues["owner/repo#11"] = true
	eng.mu.Unlock()

	eng.cleanupLockedIssues()

	// Should have removed lock labels for both issues
	if len(client.removeLabelCalls) != 2 {
		t.Errorf("expected 2 RemoveLabelFromIssue calls, got %d", len(client.removeLabelCalls))
	}
	for _, call := range client.removeLabelCalls {
		expectedLabel := "fabrik:locked:testuser"
		if call.labelName != expectedLabel {
			t.Errorf("label = %q, want %q", call.labelName, expectedLabel)
		}
	}

	// lockedIssues should be empty after cleanup
	eng.mu.Lock()
	remaining := len(eng.lockedIssues)
	eng.mu.Unlock()
	if remaining != 0 {
		t.Errorf("lockedIssues should be empty after cleanup, got %d", remaining)
	}
}

func TestCleanupLockedIssues_Empty_NoOp(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	// No locked issues
	eng.cleanupLockedIssues()
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no label removal calls for empty lockedIssues, got %d", len(client.removeLabelCalls))
	}
}

func TestParseIssueKey_Malformed(t *testing.T) {
	cases := []struct {
		key        string
		wantOwner  string
		wantRepo   string
		wantIssueN int
	}{
		{"nohash", "defOwner", "defRepo", 0},                // no # → fallback
		{"owner/repo#notanumber", "defOwner", "defRepo", 0}, // bad number → fallback
		{"#5", "defOwner", "defRepo", 5},                    // empty owner/repo → partial fallback
	}
	for _, tc := range cases {
		o, r, n := parseIssueKey(tc.key, "defOwner", "defRepo")
		if o != tc.wantOwner || r != tc.wantRepo || n != tc.wantIssueN {
			t.Errorf("parseIssueKey(%q) = (%q, %q, %d), want (%q, %q, %d)",
				tc.key, o, r, n, tc.wantOwner, tc.wantRepo, tc.wantIssueN)
		}
	}
}

func TestChangeType(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{"M", "Modified"},
		{"A", "New"},
		{"D", "Deleted"},
		{"R100", "Renamed"},
		{"C50", "Copied"},
		{"U", "U"}, // unknown passthrough
	}
	for _, tc := range cases {
		got := changeType(tc.code)
		if got != tc.want {
			t.Errorf("changeType(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestDevCheckout_NilWhenNoDefaultRepo(t *testing.T) {
	// Engine with no owner/repo configured → defaultRepo() is "" → devCheckout returns nil.
	eng := NewWithDeps(
		Config{Owner: "", Repo: "", User: "u", Token: "t", Stages: testStages()},
		&mockGitHubClient{}, &mockClaudeInvoker{}, nil,
	)
	if wm := eng.devCheckout(); wm != nil {
		t.Errorf("expected nil devCheckout when no default repo, got %v", wm)
	}
}

func TestDevCheckout_ReturnsRegistered(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	// testEngine already registers "owner/repo" via NewWithDeps — devCheckout should return it.
	if got := eng.devCheckout(); got == nil {
		t.Errorf("devCheckout should return the WM registered for owner/repo")
	}
}

func TestRegisterWorktrees_Idempotent(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	// Use a new repo key (not "owner/repo" which testEngine already registers).
	wm1 := eng.registerWorktrees("other/repo", "/tmp/base", "/tmp/worktrees")
	if wm1 == nil {
		t.Fatal("registerWorktrees returned nil on first registration")
	}
	// Second call with same key should return the existing WM.
	wm2 := eng.registerWorktrees("other/repo", "/tmp/base", "/tmp/worktrees")
	if wm1 != wm2 {
		t.Error("registerWorktrees should return the same WM on repeated calls")
	}
}

func TestEnsureRepoReady_AlreadyRegistered_ReturnsNil(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	// Pre-register a new repo so ensureRepoReady returns immediately for it.
	eng.registerWorktrees("newowner/newrepo", "/tmp/base", "/tmp/worktrees")
	item := gh.ProjectItem{Number: 1, Repo: "newowner/newrepo"}
	if err := eng.ensureRepoReady(context.Background(), item); err != nil {
		t.Errorf("expected nil when repo already registered, got %v", err)
	}
}

func TestEnsureRepoReady_EmptyOwner_ReturnsNil(t *testing.T) {
	// Item with no Repo and no default owner/repo → cannot determine repo → returns nil.
	client := &mockGitHubClient{}
	eng := NewWithDeps(
		Config{Owner: "", Repo: "", User: "u", Token: "t", Stages: testStages()},
		client, &mockClaudeInvoker{}, NewWorktreeManager("/tmp"),
	)
	item := gh.ProjectItem{Number: 1}
	if err := eng.ensureRepoReady(context.Background(), item); err != nil {
		t.Errorf("expected nil for empty owner/repo, got %v", err)
	}
}

func TestEnsureRepoReady_CloneFailure_PostsCommentAndPauses(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	// Set jobControlDir to a tempdir so ensureBareClone tries (and fails) to clone.
	eng.jobControlDir = t.TempDir()

	// Item with a new (unregistered) repo — ensureBareClone will fail (no network to nonexistent).
	item := gh.ProjectItem{Number: 77, Repo: "nonexistent-xyz/nonexistent-repo-abc123"}
	err := eng.ensureRepoReady(context.Background(), item)
	if err != ErrSkipItem {
		t.Errorf("expected ErrSkipItem on clone failure, got %v", err)
	}
	// Should have posted a comment about the failure
	if len(client.addCommentCalls) == 0 {
		t.Error("expected AddComment call for clone failure")
	}
	// Should have added fabrik:paused
	var pausedAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused label to be added on clone failure")
	}
}

func TestEnsureDraftPR_NoPR_FailedPush_ReturnsZero(t *testing.T) {
	// No existing PR; push will fail because base dir is not a real repo.
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 0, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// WorktreeManager points to /tmp/test-repo which doesn't exist
	item := gh.ProjectItem{Number: 99, Title: "Test"}
	prNum := eng.ensureDraftPR(item, "main")

	// Push fails → returns 0
	if prNum != 0 {
		t.Errorf("expected 0 when push fails, got %d", prNum)
	}
}
