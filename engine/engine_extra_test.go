package engine

import (
	"context"
	"runtime"
	"sync"
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
	// Set fabrikDir to a tempdir so ensureBareClone tries (and fails) to clone.
	eng.fabrikDir = t.TempDir()

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

// TestEnsureRepoReady_ConcurrentSameRepo_OnlyOneCloneAttempt verifies that when
// multiple goroutines call ensureRepoReady for the same (new) repo simultaneously,
// exactly one AddComment call is made (the clone-failure comment) and all callers
// return ErrSkipItem. This exercises the singleflight coordination in cloneInFlight.
func TestEnsureRepoReady_ConcurrentSameRepo_OnlyOneCloneAttempt(t *testing.T) {
	skipIfNoGit(t)
	// Maximize concurrency so goroutines interleave.
	runtime.GOMAXPROCS(runtime.NumCPU())

	const numWorkers = 8
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.fabrikDir = t.TempDir() // causes ensureBareClone to fail (no network)

	item := gh.ProjectItem{Number: 42, Repo: "nonexistent-xyz/nonexistent-repo-concurrent-test"}

	// Use a barrier so all goroutines start at the same moment.
	var barrier sync.WaitGroup
	barrier.Add(numWorkers)

	errs := make([]error, numWorkers)
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		i := i
		go func() {
			defer wg.Done()
			barrier.Done()
			barrier.Wait() // all goroutines reach here before any proceeds
			errs[i] = eng.ensureRepoReady(context.Background(), item)
		}()
	}
	wg.Wait()

	// All callers must return ErrSkipItem.
	for i, err := range errs {
		if err != ErrSkipItem {
			t.Errorf("goroutine %d: expected ErrSkipItem, got %v", i, err)
		}
	}

	// Exactly one AddComment call must have been made (the clone-failure comment).
	// Waiters must not post duplicate comments.
	client.mu.Lock()
	numComments := len(client.addCommentCalls)
	client.mu.Unlock()
	if numComments != 1 {
		t.Errorf("expected exactly 1 AddComment call, got %d", numComments)
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
