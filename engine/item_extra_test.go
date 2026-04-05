package engine

import (
	"errors"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
)

// TestEmitStructural_WithChannel sends a structural event and verifies it's received.
func TestEmitStructural_WithChannel(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	ch := make(chan tui.Event, 4)
	eng.events = ch

	eng.emitStructural(tui.PollStartedEvent{Owner: "owner", Repo: "repo", Project: 1})

	select {
	case ev := <-ch:
		if _, ok := ev.(tui.PollStartedEvent); !ok {
			t.Errorf("expected PollStartedEvent, got %T", ev)
		}
	default:
		t.Error("expected event in channel")
	}
}

// TestItemMayNeedWork_StaleButCooldownExpired verifies that a stale item is
// retried after the cooldown period expires.
func TestItemMayNeedWork_StaleButCooldownExpired(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1 // short cooldown

	ts := time.Now().Add(-time.Minute)
	item := gh.ProjectItem{
		Number:    42,
		Status:    "Research",
		ItemID:    "PVTI_42",
		UpdatedAt: ts,
	}

	// Record the last-seen timestamp so the "unchanged" path triggers
	eng.mu.Lock()
	eng.lastUpdatedAt["owner/repo#42"] = ts
	// Record a processedSet entry from >cooldown ago
	eng.processedSet["owner/repo#42-Research"] = time.Now().Add(-2 * time.Minute)
	eng.mu.Unlock()

	if !eng.itemMayNeedWork(item) {
		t.Error("stale item with expired cooldown should need work")
	}
}

// TestItemMayNeedWork_StaleWithinCooldown verifies that a stale item within
// cooldown is skipped.
func TestItemMayNeedWork_StaleWithinCooldown(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 60 // long cooldown

	ts := time.Now().Add(-time.Minute)
	item := gh.ProjectItem{
		Number:    43,
		Status:    "Research",
		ItemID:    "PVTI_43",
		UpdatedAt: ts,
	}

	eng.mu.Lock()
	eng.lastUpdatedAt["owner/repo#43"] = ts
	eng.processedSet["owner/repo#43-Research"] = time.Now() // just processed
	eng.mu.Unlock()

	if eng.itemMayNeedWork(item) {
		t.Error("stale item within cooldown should not need work")
	}
}

// TestItemMayNeedWork_LockedByOtherUser verifies locked items are skipped.
func TestItemMayNeedWork_LockedByOtherUser(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 44,
		Status: "Research",
		Labels: []string{"fabrik:locked:otheruser"},
	}
	if eng.itemMayNeedWork(item) {
		t.Error("item locked by other user should not need work")
	}
}

// TestBlockOnInput_Success covers both AddLabel calls.
func TestBlockOnInput_Success(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{Number: 5}
	eng.blockOnInput(item, stage)

	// Both fabrik:paused and fabrik:awaiting-input should have been added
	if len(client.addLabelCalls) < 2 {
		t.Errorf("expected 2 AddLabel calls, got %d", len(client.addLabelCalls))
	}
}

// TestBlockOnInput_LabelErrors_LogsWarning covers the warning log branches.
func TestBlockOnInput_LabelErrors_LogsWarning(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("label error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{Number: 6}
	// Should not panic when labels fail
	eng.blockOnInput(item, stage)
}
