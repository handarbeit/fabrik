package engine

import (
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// TestSettleClaudeLimitClearRequests_TriggersClear verifies that an operator
// applying fabrik:clear-claude-limit to any open item clears an active
// account-wide suspension and removes the trigger label — the restart-free
// clear mechanism required by #1183.
func TestSettleClaudeLimitClearRequests_TriggersClear(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)
	eng.activateClaudeSuspension(1, "", now)
	if _, ok := eng.claudeSuspendedUntilTime(now); !ok {
		t.Fatal("expected suspension to be active before the clear request")
	}

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 42, Repo: "owner/repo", Labels: []string{claudeClearLimitLabel}},
		},
	}

	eng.settleClaudeLimitClearRequests(board)

	if _, ok := eng.claudeSuspendedUntilTime(now); ok {
		t.Error("expected suspension to be cleared")
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	found := false
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 42 && c.labelName == claudeClearLimitLabel {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %s removed from issue #42, got %v", claudeClearLimitLabel, client.removeLabelCalls)
	}
}

// TestSettleClaudeLimitClearRequests_NoOpWithoutLabel verifies the scan does
// nothing when no item carries the trigger label — an active suspension must
// survive untouched.
func TestSettleClaudeLimitClearRequests_NoOpWithoutLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)
	eng.activateClaudeSuspension(1, "", now)

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 43, Repo: "owner/repo", Labels: []string{"stage:Validate:complete"}},
		},
	}

	eng.settleClaudeLimitClearRequests(board)

	if _, ok := eng.claudeSuspendedUntilTime(now); !ok {
		t.Error("expected suspension to remain active when no item carries the trigger label")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no label removals, got %v", client.removeLabelCalls)
	}
}

// TestSettleClaudeLimitClearRequests_SkipsClosedItems verifies a closed item
// carrying the trigger label is not a valid trigger.
func TestSettleClaudeLimitClearRequests_SkipsClosedItems(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)
	eng.activateClaudeSuspension(1, "", now)

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 44, Repo: "owner/repo", IsClosed: true, Labels: []string{claudeClearLimitLabel}},
		},
	}

	eng.settleClaudeLimitClearRequests(board)

	if _, ok := eng.claudeSuspendedUntilTime(now); !ok {
		t.Error("expected suspension to remain active — a closed item must not trigger the clear")
	}
}

// TestSettleClaudeLimitLabelSweep_RemovesWhenNotSuspended verifies the
// account-wide sweep clears the informational fabrik:claude-limit label from
// every open item once the underlying suspension has lifted — the
// requirement that the label clear account-wide, not only on a labelled
// issue's next dispatched invocation.
func TestSettleClaudeLimitLabelSweep_RemovesWhenNotSuspended(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 50, Repo: "owner/repo", Labels: []string{"fabrik:claude-limit"}},
			{Number: 51, Repo: "owner/repo", Labels: []string{"fabrik:claude-limit", "fabrik:paused"}},
		},
	}

	eng.settleClaudeLimitLabelSweep(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	removed := map[int]bool{}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:claude-limit" {
			removed[c.issueNumber] = true
		}
	}
	if !removed[50] || !removed[51] {
		t.Errorf("expected fabrik:claude-limit removed from both open items, got %v", client.removeLabelCalls)
	}
}

// TestSettleClaudeLimitLabelSweep_LeavesLabelWhileSuspended verifies the
// sweep is a no-op while the account-wide suspension is still active — the
// label is accurate in that state and must not be removed prematurely.
func TestSettleClaudeLimitLabelSweep_LeavesLabelWhileSuspended(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.activateClaudeSuspension(1, "", time.Now())

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 52, Repo: "owner/repo", Labels: []string{"fabrik:claude-limit"}},
		},
	}

	eng.settleClaudeLimitLabelSweep(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no label removals while suspended, got %v", client.removeLabelCalls)
	}
}

// TestSettleClaudeLimitLabelSweep_SkipsClosedItems verifies closed items are
// left to cleanupClosedIssueTransientLabels rather than double-handled here.
func TestSettleClaudeLimitLabelSweep_SkipsClosedItems(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 53, Repo: "owner/repo", IsClosed: true, Labels: []string{"fabrik:claude-limit"}},
		},
	}

	eng.settleClaudeLimitLabelSweep(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no label removals for a closed item, got %v", client.removeLabelCalls)
	}
}

// TestSettleClaudeLimitLabelSweep_SkipsItemsWithoutLabel verifies the sweep
// only acts on items actually carrying fabrik:claude-limit.
func TestSettleClaudeLimitLabelSweep_SkipsItemsWithoutLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 54, Repo: "owner/repo", Labels: []string{"stage:Validate:complete"}},
		},
	}

	eng.settleClaudeLimitLabelSweep(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no label removals for an item without the label, got %v", client.removeLabelCalls)
	}
}

// TestSettleClaudeLimitLabelSweep_ConcurrentWithActivate drives the sweep
// concurrently with a fresh activation racing on the same
// claudeSuspendedUntil field — go test -race must be clean, validating
// claudeSuspendMu actually guards the shared state the sweep and a real hit
// both read/act on (the sweep/activate race flagged during Plan).
func TestSettleClaudeLimitLabelSweep_ConcurrentWithActivate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 55, Repo: "owner/repo", Labels: []string{"fabrik:claude-limit"}},
		},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		eng.settleClaudeLimitLabelSweep(board)
	}()
	go func() {
		defer wg.Done()
		eng.activateClaudeSuspension(55, "", time.Now())
	}()
	wg.Wait()
}
