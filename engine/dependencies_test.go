package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/internal/itemstate"
	"github.com/verveguy/fabrik/stages"
)

// depTestStages returns a two-stage pipeline for dependency gate tests.
func depTestStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1, Prompt: "specify"},
		{Name: "Research", Order: 2, Prompt: "research"},
		{Name: "Implement", Order: 3, Prompt: "implement"},
	}
}

func depTestEngine(client *mockGitHubClient) *Engine {
	return testEngineWithStages(client, depTestStages())
}

func TestCheckDependencies_NoDeps_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if blocked {
		t.Error("expected not blocked when no deps")
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label adds, got %d", len(client.addLabelCalls))
	}
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no label removes, got %d", len(client.removeLabelCalls))
	}
}

func TestCheckDependencies_AllDepsClosed_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		BlockedBy: []gh.Dependency{
			{Number: 9, State: "CLOSED", Repo: "owner/repo"},
		},
	}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if blocked {
		t.Error("expected not blocked when all deps closed")
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label adds, got %d", len(client.addLabelCalls))
	}
}

func TestCheckDependencies_AllDepsClosed_RemovesBlockedLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:blocked"},
		BlockedBy: []gh.Dependency{
			{Number: 9, State: "CLOSED", Repo: "owner/repo"},
		},
	}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if blocked {
		t.Error("expected not blocked when all deps closed")
	}
	if len(client.removeLabelCalls) != 1 {
		t.Fatalf("expected 1 remove label call, got %d", len(client.removeLabelCalls))
	}
	if client.removeLabelCalls[0].labelName != "fabrik:blocked" {
		t.Errorf("expected removal of fabrik:blocked, got %q", client.removeLabelCalls[0].labelName)
	}
}

func TestCheckDependencies_OpenDeps_ReturnsTrue_FirstTime(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		// No fabrik:blocked label — first time blocking
		BlockedBy: []gh.Dependency{
			{Number: 8, State: "OPEN", Repo: "owner/repo"},
		},
	}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if !blocked {
		t.Error("expected blocked with open deps")
	}
	// Should have added fabrik:blocked label
	if len(client.addLabelCalls) != 1 {
		t.Fatalf("expected 1 add label call, got %d", len(client.addLabelCalls))
	}
	if client.addLabelCalls[0].labelName != "fabrik:blocked" {
		t.Errorf("expected fabrik:blocked, got %q", client.addLabelCalls[0].labelName)
	}
	// Should have posted comment
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	if !strings.Contains(client.addCommentCalls[0].body, "#8") {
		t.Errorf("comment should mention #8, got: %q", client.addCommentCalls[0].body)
	}
}

func TestCheckDependencies_OpenDeps_AlreadyBlocked_NoComment(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:blocked"}, // already blocked
		BlockedBy: []gh.Dependency{
			{Number: 8, State: "OPEN", Repo: "owner/repo"},
		},
	}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if !blocked {
		t.Error("expected blocked with open deps")
	}
	// No comment or label add because already blocked
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no comment when already blocked, got %d", len(client.addCommentCalls))
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label add when already blocked, got %d", len(client.addLabelCalls))
	}
}

func TestCheckDependencies_FirstStage_BlockedWithOpenDeps(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		// Has open blocker — first stage is no longer exempt (#473)
		BlockedBy: []gh.Dependency{
			{Number: 8, State: "OPEN", Repo: "owner/repo"},
		},
	}
	// "Specify" is the first stage in depTestStages()
	stage := &stages.Stage{Name: "Specify"}

	blocked := eng.checkDependencies(board, item, stage)

	if !blocked {
		t.Error("expected blocked for first stage when open deps exist")
	}
	if len(client.addLabelCalls) != 1 {
		t.Fatalf("expected 1 add label call, got %d", len(client.addLabelCalls))
	}
	if client.addLabelCalls[0].labelName != "fabrik:blocked" {
		t.Errorf("expected fabrik:blocked, got %q", client.addLabelCalls[0].labelName)
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
}

func TestCheckDependencies_CrossRepoDep_FormattedCorrectly(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		BlockedBy: []gh.Dependency{
			{Number: 99, State: "OPEN", Repo: "other/repo"}, // cross-repo
		},
	}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if !blocked {
		t.Error("expected blocked")
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	// Cross-repo dep should be formatted as "other/repo#99"
	if !strings.Contains(client.addCommentCalls[0].body, "other/repo#99") {
		t.Errorf("expected cross-repo format in comment, got: %q", client.addCommentCalls[0].body)
	}
}

// TestProcessItem_SkipsBlockedNonFirstStage verifies that processItem returns nil
// without invoking Claude when an item in a non-first stage has an open BlockedBy
// dependency. This exercises the checkDependencies call added before stage work
// begins (Bug 1 fix).
func TestProcessItem_SkipsBlockedNonFirstStage(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	// depTestStages: Specify (order 1), Research (order 2), Implement (order 3)
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        depTestStages(),
		},
		client,
		claude,
		NewWorktreeManager("/tmp/test-repo"),
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 42,
		Title:  "Blocked item",
		Status: "Research", // non-first stage
		Repo:   "owner/repo",
		BlockedBy: []gh.Dependency{
			{Number: 41, State: "OPEN", Repo: "owner/repo"},
		},
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}
	// Claude must not be invoked — the dependency gate should return nil before
	// any worktree setup or stage invocation.
	if len(claude.calls) != 0 {
		t.Errorf("expected no Claude invocations for blocked non-first stage item, got %d", len(claude.calls))
	}
}

// TestProcessItem_SkipsBlockedFirstStage verifies the regression path from #473:
// processItem must skip Claude invocation for the first stage (Specify) when the
// item has an open BlockedBy dependency. Previously, checkDependencies exempted
// the first stage and Specify ran against pre-merge code despite open blockers.
func TestProcessItem_SkipsBlockedFirstStage(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	// depTestStages: Specify (order 1), Research (order 2), Implement (order 3)
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        depTestStages(),
		},
		client,
		claude,
		NewWorktreeManager("/tmp/test-repo"),
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 43,
		Title:  "Blocked first-stage item",
		Status: "Specify", // first stage
		Repo:   "owner/repo",
		BlockedBy: []gh.Dependency{
			{Number: 41, State: "OPEN", Repo: "owner/repo"},
		},
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}
	// Claude must not be invoked — the dependency gate now applies to the first stage.
	if len(claude.calls) != 0 {
		t.Errorf("expected no Claude invocations for blocked first-stage item, got %d", len(claude.calls))
	}
	// fabrik:blocked label must be added.
	if len(client.addLabelCalls) != 1 || client.addLabelCalls[0].labelName != "fabrik:blocked" {
		t.Errorf("expected fabrik:blocked to be added, got addLabelCalls=%v", client.addLabelCalls)
	}
}

// ---- removeBlockedIfResolved unit tests (Task 4) ----

// TestRemoveBlockedIfResolved_Success verifies the happy path: label removed and
// cache written on the first attempt.
func TestRemoveBlockedIfResolved_Success(t *testing.T) {
	orig := blockedLabelRetryDelay
	blockedLabelRetryDelay = 0
	t.Cleanup(func() { blockedLabelRetryDelay = orig })

	var calls int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:blocked" {
				calls++
			}
			return nil
		},
	}
	eng := depTestEngine(client)
	eng.removeBlockedIfResolved("owner", "repo", 10)
	if calls != 1 {
		t.Errorf("expected 1 RemoveLabelFromIssue call, got %d", calls)
	}
}

// TestRemoveBlockedIfResolved_ErrNotFound verifies that ErrNotFound is treated as
// success (label already absent) — exactly one call, no further retries.
func TestRemoveBlockedIfResolved_ErrNotFound(t *testing.T) {
	orig := blockedLabelRetryDelay
	blockedLabelRetryDelay = 0
	t.Cleanup(func() { blockedLabelRetryDelay = orig })

	var calls int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:blocked" {
				calls++
				return gh.ErrNotFound
			}
			return nil
		},
	}
	eng := depTestEngine(client)
	eng.removeBlockedIfResolved("owner", "repo", 10)
	if calls != 1 {
		t.Errorf("expected exactly 1 call for ErrNotFound, got %d", calls)
	}
}

// TestRemoveBlockedIfResolved_TransientRetrySucceeds verifies that a transient
// error is retried and the call succeeds on the third attempt (3 total calls).
func TestRemoveBlockedIfResolved_TransientRetrySucceeds(t *testing.T) {
	orig := blockedLabelRetryDelay
	blockedLabelRetryDelay = 0
	t.Cleanup(func() { blockedLabelRetryDelay = orig })

	var calls int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:blocked" {
				calls++
				if calls < 3 {
					return fmt.Errorf("executing request: %w", &net.OpError{Op: "read", Net: "tcp"})
				}
			}
			return nil
		},
	}
	eng := depTestEngine(client)
	eng.removeBlockedIfResolved("owner", "repo", 10)
	if calls != 3 {
		t.Errorf("expected 3 calls (2 transient then success), got %d", calls)
	}
}

// TestRemoveBlockedIfResolved_NonTransientNoRetry verifies that a non-transient
// error produces exactly one attempt with no retries.
func TestRemoveBlockedIfResolved_NonTransientNoRetry(t *testing.T) {
	orig := blockedLabelRetryDelay
	blockedLabelRetryDelay = 0
	t.Cleanup(func() { blockedLabelRetryDelay = orig })

	var calls int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:blocked" {
				calls++
				return errors.New("GitHub API returned 422: validation failed")
			}
			return nil
		},
	}
	eng := depTestEngine(client)
	eng.removeBlockedIfResolved("owner", "repo", 10)
	if calls != 1 {
		t.Errorf("expected exactly 1 call for non-transient error, got %d", calls)
	}
}

// ---- PushUnblockObserver tests (Task 5) ----

// openItemWithBlockedLabel creates a gh.ProjectItem in the given column with
// fabrik:blocked and the given blockers.
func openItemWithBlockedLabel(number int, repo, column string, blockers []gh.Dependency) gh.ProjectItem {
	return gh.ProjectItem{
		Number:    number,
		Repo:      repo,
		Status:    column,
		Labels:    []string{"fabrik:blocked"},
		BlockedBy: blockers,
	}
}

// waitForRemove waits up to 1 second for a value on ch; returns true if received.
func waitForRemove(t *testing.T, ch <-chan int) (int, bool) {
	t.Helper()
	select {
	case n := <-ch:
		return n, true
	case <-time.After(time.Second):
		return 0, false
	}
}

// TestPushUnblockObserver_SingleBlocker_NonStageColumn verifies that an item in a
// non-stage column (Backlog) is unblocked when its single blocker closes.
func TestPushUnblockObserver_SingleBlocker_NonStageColumn(t *testing.T) {
	store := itemstate.NewStore(nil)

	// Seed blocker Y (open) and dependent X (in Backlog, blocked).
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 9, Repo: "owner/repo"}})
	store.Apply(itemstate.IssueOpened{Item: openItemWithBlockedLabel(10, "owner/repo", "Backlog", []gh.Dependency{
		{Number: 9, Repo: "owner/repo", State: "OPEN"},
	})})

	removeCh := make(chan int, 1)
	obs := &PushUnblockObserver{
		Store:  store,
		Remove: func(owner, repo string, n int) { removeCh <- n },
	}
	store.Subscribe(obs)

	// Close Y.
	store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 9})

	n, ok := waitForRemove(t, removeCh)
	if !ok {
		t.Fatal("timeout: Remove was not called after blocker closed")
	}
	if n != 10 {
		t.Errorf("expected Remove called for issue 10, got %d", n)
	}
}

// TestPushUnblockObserver_TwoBlockers_OneCloses_NotRemoved verifies that closing
// only one of two blockers does NOT trigger removal.
func TestPushUnblockObserver_TwoBlockers_OneCloses_NotRemoved(t *testing.T) {
	store := itemstate.NewStore(nil)

	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 9, Repo: "owner/repo"}})
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 11, Repo: "owner/repo"}})
	store.Apply(itemstate.IssueOpened{Item: openItemWithBlockedLabel(10, "owner/repo", "Backlog", []gh.Dependency{
		{Number: 9, Repo: "owner/repo", State: "OPEN"},
		{Number: 11, Repo: "owner/repo", State: "OPEN"},
	})})

	removeCh := make(chan int, 1)
	obs := &PushUnblockObserver{
		Store:  store,
		Remove: func(owner, repo string, n int) { removeCh <- n },
	}
	store.Subscribe(obs)

	// Close only Y (9); Z (11) is still open.
	store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 9})

	// Should NOT remove — Z is still open.
	select {
	case n := <-removeCh:
		t.Errorf("Remove unexpectedly called for issue %d when one blocker still open", n)
	case <-time.After(100 * time.Millisecond):
		// expected: no removal
	}
}

// TestPushUnblockObserver_TwoBlockers_BothClose_Removed verifies that closing
// both blockers eventually triggers removal.
func TestPushUnblockObserver_TwoBlockers_BothClose_Removed(t *testing.T) {
	store := itemstate.NewStore(nil)

	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 9, Repo: "owner/repo"}})
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 11, Repo: "owner/repo"}})
	store.Apply(itemstate.IssueOpened{Item: openItemWithBlockedLabel(10, "owner/repo", "Backlog", []gh.Dependency{
		{Number: 9, Repo: "owner/repo", State: "OPEN"},
		{Number: 11, Repo: "owner/repo", State: "OPEN"},
	})})

	removeCh := make(chan int, 2)
	obs := &PushUnblockObserver{
		Store:  store,
		Remove: func(owner, repo string, n int) { removeCh <- n },
	}
	store.Subscribe(obs)

	// Close Y (9) — X remains blocked (Z still open).
	store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 9})
	select {
	case n := <-removeCh:
		t.Errorf("Remove unexpectedly called for issue %d after only first blocker closed", n)
	case <-time.After(100 * time.Millisecond):
		// expected: no removal yet
	}

	// Close Z (11) — now all blockers closed.
	store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 11})

	n, ok := waitForRemove(t, removeCh)
	if !ok {
		t.Fatal("timeout: Remove was not called after both blockers closed")
	}
	if n != 10 {
		t.Errorf("expected Remove called for issue 10, got %d", n)
	}
}

// TestPushUnblockObserver_NoBlockedLabel_NotRemoved verifies that items without
// fabrik:blocked are ignored even if they have a BlockedBy entry.
func TestPushUnblockObserver_NoBlockedLabel_NotRemoved(t *testing.T) {
	store := itemstate.NewStore(nil)

	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 9, Repo: "owner/repo"}})
	// Item 10 has BlockedBy but NOT the fabrik:blocked label.
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Number:    10,
		Repo:      "owner/repo",
		Status:    "Backlog",
		BlockedBy: []gh.Dependency{{Number: 9, Repo: "owner/repo", State: "OPEN"}},
	}})

	removeCh := make(chan int, 1)
	obs := &PushUnblockObserver{
		Store:  store,
		Remove: func(owner, repo string, n int) { removeCh <- n },
	}
	store.Subscribe(obs)

	store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 9})

	select {
	case n := <-removeCh:
		t.Errorf("Remove unexpectedly called for issue %d (no fabrik:blocked label)", n)
	case <-time.After(100 * time.Millisecond):
		// expected: no removal
	}
}

// TestCheckDependencies_PullPath_Regression verifies that the existing
// pull-based checkDependencies path (Task 5d) still removes fabrik:blocked
// when invoked for items in stage columns.
func TestCheckDependencies_PullPath_Regression(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Status: "Research", // stage column
		Labels: []string{"fabrik:blocked"},
		BlockedBy: []gh.Dependency{
			{Number: 9, State: "CLOSED", Repo: "owner/repo"},
		},
	}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if blocked {
		t.Error("expected not blocked when all deps CLOSED")
	}
	if len(client.removeLabelCalls) != 1 || client.removeLabelCalls[0].labelName != "fabrik:blocked" {
		t.Errorf("expected pull-path to remove fabrik:blocked, got removeLabelCalls=%v", client.removeLabelCalls)
	}
}
