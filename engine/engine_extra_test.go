package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/tui"
)

func TestSetEvents_ConfiguresChannel(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	ch := make(chan tui.Event, 4)
	eng.SetEvents(ch)

	if eng.events != ch {
		t.Error("SetEvents should set the events channel")
	}
}

func TestEmit_NilChannel_NoOp(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	// No channel set — emit should not panic
	eng.emit(tui.LogEvent{IssueNumber: 1, Tag: "test", Message: "hello"})
}

func TestEmit_WithChannel_SendsEvent(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
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
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Seed locks in the store via LocalLockAcquired mutations.
	now := time.Now()
	eng.store.Apply(itemstate.LocalLockAcquired{Repo: "owner/repo", Number: 10, User: "testuser", AcquiredAt: now})
	eng.store.Apply(itemstate.LocalLockAcquired{Repo: "owner/repo", Number: 11, User: "testuser", AcquiredAt: now})

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

	// Store should have no held locks after cleanup.
	for _, snap := range eng.store.All() {
		if lock := snap.Lock(); lock != nil && lock.HeldByThis {
			t.Errorf("store still has held lock for %s#%d after cleanup", snap.Repo(), snap.Number())
		}
	}
}

func TestCleanupLockedIssues_Empty_NoOp(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
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
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	// Use a new repo key (not "owner/repo" which testEngine already registers).
	wm1 := eng.registerWorktrees("other/repo", t.TempDir(), t.TempDir())
	if wm1 == nil {
		t.Fatal("registerWorktrees returned nil on first registration")
	}
	// Second call with same key should return the existing WM.
	wm2 := eng.registerWorktrees("other/repo", t.TempDir(), t.TempDir())
	if wm1 != wm2 {
		t.Error("registerWorktrees should return the same WM on repeated calls")
	}
}

func TestEnsureRepoReady_AlreadyRegistered_ReturnsNil(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	// Pre-register a new repo so ensureRepoReady returns immediately for it.
	eng.registerWorktrees("newowner/newrepo", t.TempDir(), t.TempDir())
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
		client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)
	item := gh.ProjectItem{Number: 1}
	if err := eng.ensureRepoReady(context.Background(), item); err != nil {
		t.Errorf("expected nil for empty owner/repo, got %v", err)
	}
}

func TestEnsureRepoReady_CloneFailure_PostsCommentAndPauses(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	// ENAMETOOLONG: fails os.MkdirAll before any network call — deterministic, no git subprocess.
	eng.fabrikDir = strings.Repeat("a", 10000)

	// Item with a new (unregistered) repo — ensureBareClone will fail at os.MkdirAll.
	item := gh.ProjectItem{Number: 77, Repo: "fail-test/unregistered"}
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
//
// Each goroutine uses a distinct item Number (same repo) — matching what actually
// happens in production bursts (different board items racing on a never-before-cloned
// repo), and required by the identity-gated retry boundary (ADR-1543, #1543): with a
// single shared item and empty Labels, every goroutine would trivially match the
// owner's identity as unpaused and treat itself as retry-eligible, which is the
// opposite of what this test is meant to prove. See
// TestEnsureRepoReady_DifferentItemAfterFailure_NoSecondCloneOrComment and
// TestEnsureRepoReady_SameItemStillPaused_NoRetry /
// TestEnsureRepoReady_SameItemUnpausedAfterFix_Retries below for the deterministic,
// non-racing regression coverage of the actual defect (#1543 R5).
func TestEnsureRepoReady_ConcurrentSameRepo_OnlyOneCloneAttempt(t *testing.T) {
	skipIfNoGit(t)
	// Maximize concurrency so goroutines interleave.
	runtime.GOMAXPROCS(runtime.NumCPU())

	const numWorkers = 8
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	// ENAMETOOLONG: fails os.MkdirAll before any network call — deterministic, no git subprocess.
	eng.fabrikDir = strings.Repeat("a", 10000)

	// Use a barrier so all goroutines start at the same moment.
	var barrier sync.WaitGroup
	barrier.Add(numWorkers)

	errs := make([]error, numWorkers)
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		i := i
		item := gh.ProjectItem{Number: 100 + i, Repo: "fail-test/concurrent"}
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

// TestEnsureRepoReady_DifferentItemAfterFailure_NoSecondCloneOrComment is the
// deterministic (sequential, non-racing) regression test for #1543: item A fails
// to clone; item B (same repo, no scheduling luck involved) must silently skip
// without a second clone attempt or a second comment. This directly proves the
// identity-gated retry boundary rather than relying on goroutine scheduling.
func TestEnsureRepoReady_DifferentItemAfterFailure_NoSecondCloneOrComment(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	// ENAMETOOLONG: fails os.MkdirAll before any network call — deterministic, no git subprocess.
	eng.fabrikDir = strings.Repeat("a", 10000)

	var cloneAttempts int
	eng.cloneAttemptHook = func(string) { cloneAttempts++ }

	itemA := gh.ProjectItem{Number: 1, Repo: "fail-test/different-item"}
	if err := eng.ensureRepoReady(context.Background(), itemA); err != ErrSkipItem {
		t.Fatalf("item A: expected ErrSkipItem, got %v", err)
	}
	if cloneAttempts != 1 {
		t.Fatalf("item A: expected 1 clone attempt, got %d", cloneAttempts)
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("item A: expected 1 AddComment call, got %d", len(client.addCommentCalls))
	}

	itemB := gh.ProjectItem{Number: 2, Repo: "fail-test/different-item"}
	if err := eng.ensureRepoReady(context.Background(), itemB); err != ErrSkipItem {
		t.Fatalf("item B: expected ErrSkipItem, got %v", err)
	}
	if cloneAttempts != 1 {
		t.Errorf("item B: expected no additional clone attempt, got %d total", cloneAttempts)
	}
	if len(client.addCommentCalls) != 1 {
		t.Errorf("item B: expected no additional AddComment call, got %d total", len(client.addCommentCalls))
	}
}

// TestEnsureRepoReady_SameItemStillPaused_NoRetry proves the other edge of the
// identity gate: even the item that owned the failed attempt must not retry while
// its own (already-fetched) Labels still carry fabrik:paused — otherwise the fix
// would trade an intermittent duplicate for retrying too eagerly.
func TestEnsureRepoReady_SameItemStillPaused_NoRetry(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.fabrikDir = strings.Repeat("a", 10000)

	var cloneAttempts int
	eng.cloneAttemptHook = func(string) { cloneAttempts++ }

	item := gh.ProjectItem{Number: 1, Repo: "fail-test/still-paused"}
	if err := eng.ensureRepoReady(context.Background(), item); err != ErrSkipItem {
		t.Fatalf("expected ErrSkipItem on first attempt, got %v", err)
	}
	if cloneAttempts != 1 {
		t.Fatalf("expected 1 clone attempt after first failure, got %d", cloneAttempts)
	}

	// Same item, but now carrying fabrik:paused — as pauseIssue would have just
	// applied. Must not be treated as retry-eligible.
	pausedItem := item
	pausedItem.Labels = []string{"fabrik:paused", "fabrik:awaiting-input"}
	if err := eng.ensureRepoReady(context.Background(), pausedItem); err != ErrSkipItem {
		t.Fatalf("expected ErrSkipItem while still paused, got %v", err)
	}
	if cloneAttempts != 1 {
		t.Errorf("expected no additional clone attempt while paused, got %d total", cloneAttempts)
	}
	if len(client.addCommentCalls) != 1 {
		t.Errorf("expected no additional comment while paused, got %d total", len(client.addCommentCalls))
	}
}

// TestEnsureRepoReady_SameItemUnpausedAfterFix_Retries demonstrates AC3: after a
// clone failure, a subsequent call for the *same* item — once its Labels no longer
// carry fabrik:paused, modeling an operator having fixed the cause and cleared the
// label — retries and succeeds. The bare-clone directory is pre-created as a real
// (empty) bare repo so ensureBareClone's "already exists" repair path returns
// success deterministically, without a network dependency (see
// .claude/rules/golang.md).
func TestEnsureRepoReady_SameItemUnpausedAfterFix_Retries(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.fabrikDir = strings.Repeat("a", 10000)

	var cloneAttempts int
	eng.cloneAttemptHook = func(string) { cloneAttempts++ }

	item := gh.ProjectItem{Number: 1, Repo: "retry-test/repo"}
	if err := eng.ensureRepoReady(context.Background(), item); err != ErrSkipItem {
		t.Fatalf("expected ErrSkipItem on first attempt, got %v", err)
	}
	if cloneAttempts != 1 {
		t.Fatalf("expected 1 clone attempt after first failure, got %d", cloneAttempts)
	}

	// Operator fixes the underlying cause (a valid fabrikDir) and pre-creates the
	// bare-clone destination as a real bare repo, then removes fabrik:paused.
	validDir := t.TempDir()
	eng.fabrikDir = validDir
	bareDir := filepath.Join(validDir, ".fabrik", "repos", "retry-test-repo.git")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatalf("creating bare dir: %v", err)
	}
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}

	unpausedItem := item // fresh copy, no fabrik:paused label
	if err := eng.ensureRepoReady(context.Background(), unpausedItem); err != nil {
		t.Fatalf("expected retry to succeed after fix, got %v", err)
	}
	if cloneAttempts != 2 {
		t.Errorf("expected exactly 2 clone attempts total (initial failure + retry), got %d", cloneAttempts)
	}
	eng.mu.Lock()
	_, registered := eng.worktreeManagers["retry-test/repo"]
	eng.mu.Unlock()
	if !registered {
		t.Error("expected WorktreeManager registered after successful retry")
	}
}

// TestEnsureSpawnTargetReady_CloneFailure_PostsCommentAndPausesParent is the
// single-caller parity test for ensureSpawnTargetReady, mirroring
// TestEnsureRepoReady_CloneFailure_PostsCommentAndPauses.
func TestEnsureSpawnTargetReady_CloneFailure_PostsCommentAndPausesParent(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	// ENAMETOOLONG: fails os.MkdirAll before any network call — deterministic, no git subprocess.
	eng.fabrikDir = strings.Repeat("a", 10000)

	parentItem := gh.ProjectItem{Number: 5, Repo: "owner/repo"}
	err := eng.ensureSpawnTargetReady(context.Background(), "spawn-target-owner", "spawn-target-repo", parentItem)
	if err == nil {
		t.Fatal("expected error on clone failure")
	}
	if len(client.addCommentCalls) != 1 {
		t.Errorf("expected 1 AddComment call, got %d", len(client.addCommentCalls))
	}
	var pausedAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused label added on parent on clone failure")
	}
}

// TestEnsureSpawnTargetReady_DifferentParentAfterFailure_NoSecondCloneAttempt is the
// deterministic (sequential, non-racing) regression test for #1543 R4:
// ensureSpawnTargetReady shares cloneInFlight's identity-gated retry boundary with
// ensureRepoReady. Two different parents racing on the same new spawn-target repo
// must produce exactly one clone attempt — each parent still legitimately gets its
// own error comment by design (unlike ensureRepoReady's silent waiters), so the
// non-duplication signal here is the clone-attempt count, not the comment count.
func TestEnsureSpawnTargetReady_DifferentParentAfterFailure_NoSecondCloneAttempt(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.fabrikDir = strings.Repeat("a", 10000)

	var cloneAttempts int
	eng.cloneAttemptHook = func(string) { cloneAttempts++ }

	parentA := gh.ProjectItem{Number: 10, Repo: "owner/repo"}
	if err := eng.ensureSpawnTargetReady(context.Background(), "spawn-target-owner", "spawn-target-repo2", parentA); err == nil {
		t.Fatal("parent A: expected error on clone failure")
	}
	if cloneAttempts != 1 {
		t.Fatalf("parent A: expected 1 clone attempt, got %d", cloneAttempts)
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("parent A: expected 1 AddComment call, got %d", len(client.addCommentCalls))
	}

	parentB := gh.ProjectItem{Number: 11, Repo: "owner/repo"}
	if err := eng.ensureSpawnTargetReady(context.Background(), "spawn-target-owner", "spawn-target-repo2", parentB); err == nil {
		t.Fatal("parent B: expected error (own comment) on clone failure")
	}
	if cloneAttempts != 1 {
		t.Errorf("parent B: expected no additional clone attempt, got %d total", cloneAttempts)
	}
	// Each distinct parent legitimately posts its own comment — this is by design,
	// not the bug (see ensureSpawnTargetReady's doc comment).
	if len(client.addCommentCalls) != 2 {
		t.Errorf("parent B: expected its own additional comment, got %d total", len(client.addCommentCalls))
	}
}

// TestEnsureSpawnTargetReady_SameParentUnpausedAfterFix_Retries mirrors
// TestEnsureRepoReady_SameItemUnpausedAfterFix_Retries, keyed on the owning parent
// item: after a clone failure, a later call from the *same* parent — once its
// Labels no longer carry fabrik:paused — retries and succeeds.
func TestEnsureSpawnTargetReady_SameParentUnpausedAfterFix_Retries(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.fabrikDir = strings.Repeat("a", 10000)

	var cloneAttempts int
	eng.cloneAttemptHook = func(string) { cloneAttempts++ }

	parentItem := gh.ProjectItem{Number: 20, Repo: "owner/repo"}
	if err := eng.ensureSpawnTargetReady(context.Background(), "spawn-target-owner", "spawn-target-repo3", parentItem); err == nil {
		t.Fatal("expected error on first attempt")
	}
	if cloneAttempts != 1 {
		t.Fatalf("expected 1 clone attempt after first failure, got %d", cloneAttempts)
	}

	// Operator fixes the underlying cause and pre-creates the bare-clone
	// destination as a real bare repo, then removes fabrik:paused from the parent.
	validDir := t.TempDir()
	eng.fabrikDir = validDir
	bareDir := filepath.Join(validDir, ".fabrik", "repos", "spawn-target-owner-spawn-target-repo3.git")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatalf("creating bare dir: %v", err)
	}
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}

	unpausedParent := parentItem // fresh copy, no fabrik:paused label
	if err := eng.ensureSpawnTargetReady(context.Background(), "spawn-target-owner", "spawn-target-repo3", unpausedParent); err != nil {
		t.Fatalf("expected retry to succeed after fix, got %v", err)
	}
	if cloneAttempts != 2 {
		t.Errorf("expected exactly 2 clone attempts total (initial failure + retry), got %d", cloneAttempts)
	}
	eng.mu.Lock()
	_, registered := eng.worktreeManagers["spawn-target-owner/spawn-target-repo3"]
	eng.mu.Unlock()
	if !registered {
		t.Error("expected WorktreeManager registered after successful retry")
	}
}

func TestEnsureDraftPR_NoPR_FailedPush_ReturnsError(t *testing.T) {
	// No existing PR; push will fail because base dir is not a real repo.
	// fetchLinkedPRFn not set → mock returns nil, nil (no PR found).
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	// WorktreeManager points to /tmp/test-repo which doesn't exist
	item := gh.ProjectItem{Number: 99, Title: "Test"}
	prNum, err := eng.ensureDraftPR(item, "main")

	// Push fails → returns (0, err)
	if prNum != 0 {
		t.Errorf("expected 0 when push fails, got %d", prNum)
	}
	if err == nil {
		t.Error("expected error when push fails, got nil")
	}
}
