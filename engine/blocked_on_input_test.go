package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

// --- (a) CheckBlockedOnInput ---

func TestCheckBlockedOnInput_DetectsMarker(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"exact marker", "FABRIK_BLOCKED_ON_INPUT\n", true},
		{"marker with CRLF", "FABRIK_BLOCKED_ON_INPUT\r\n", true},
		{"marker in middle", "some text\nFABRIK_BLOCKED_ON_INPUT\nmore text", true},
		{"stage complete — not blocked", "FABRIK_STAGE_COMPLETE\n", false},
		{"empty output", "", false},
		{"partial match", "FABRIK_BLOCKED_ON_INPUT_EXTRA\n", false},
		{"not on its own line", "prefix FABRIK_BLOCKED_ON_INPUT suffix\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckBlockedOnInput(tc.output)
			if got != tc.want {
				t.Errorf("CheckBlockedOnInput(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// --- (b) isAwaitingInput ---

func TestIsAwaitingInput(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"both labels", []string{"fabrik:paused", "fabrik:awaiting-input"}, true},
		{"only paused", []string{"fabrik:paused"}, false},
		{"only awaiting-input", []string{"fabrik:awaiting-input"}, false},
		{"neither", []string{}, false},
		{"other labels too", []string{"fabrik:paused", "fabrik:awaiting-input", "stage:Research:complete"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := gh.ProjectItem{Labels: tc.labels}
			got := isAwaitingInput(item)
			if got != tc.want {
				t.Errorf("isAwaitingInput(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

// --- (c) itemMayNeedWork with awaiting-input ---

func TestItemMayNeedWork_AwaitingInput_PassesThrough(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number:    1,
		Status:    "Research",
		Labels:    []string{"fabrik:paused", "fabrik:awaiting-input"},
		UpdatedAt: time.Now(),
	}

	if !eng.itemMayNeedWork(item) {
		t.Error("itemMayNeedWork should return true for awaiting-input item")
	}
}

func TestItemMayNeedWork_AwaitingInput_LockedByOther(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:paused", "fabrik:awaiting-input", "fabrik:locked:otheruser"},
	}

	if eng.itemMayNeedWork(item) {
		t.Error("itemMayNeedWork should return false when locked by another user")
	}
}

func TestItemMayNeedWork_PausedOnly_Blocked(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number:    1,
		Status:    "Research",
		Labels:    []string{"fabrik:paused"},
		UpdatedAt: time.Now(),
	}

	if eng.itemMayNeedWork(item) {
		t.Error("itemMayNeedWork should return false for plain paused item")
	}
}

// --- (d) itemNeedsWork with awaiting-input ---

func TestItemNeedsWork_AwaitingInput_WithNewComments(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:paused", "fabrik:awaiting-input"},
		Comments: []gh.Comment{
			{ID: "C1", Author: "testuser", Body: "Here is my answer"},
		},
	}

	if !eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return true for awaiting-input item with new comments")
	}
}

func TestItemNeedsWork_AwaitingInput_NoComments(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number:   1,
		Status:   "Research",
		Labels:   []string{"fabrik:paused", "fabrik:awaiting-input"},
		Comments: nil,
	}

	if eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return false for awaiting-input item with no new comments")
	}
}

// --- (e) processItem unblocks on awaiting-input + new comment ---

func TestProcessItem_AwaitingInput_UnblocksOnComment(t *testing.T) {
	// This test verifies that when an awaiting-input issue receives a new comment:
	// 1. Both fabrik:paused and fabrik:awaiting-input labels are removed
	// 2. processComments is entered (indicated by fabrik:editing label being added)
	//
	// Note: processComments uses InvokeClaudeForComments directly (not via the
	// ClaudeInvoker interface), so we can't mock the actual Claude invocation here.
	// We verify routing by checking the fabrik:editing label that processComments adds.
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			Stages:     testStages(),
		},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1,
		Title:  "Test",
		Status: "Research",
		Labels: []string{"fabrik:paused", "fabrik:awaiting-input"},
		Comments: []gh.Comment{
			{ID: "C1", Author: "testuser", Body: "Here is my answer"},
		},
	}

	// processItem may error during InvokeClaudeForComments (no real Claude binary
	// in test env) — we only care that unblocking happened before that point.
	_ = eng.processItem(context.Background(), board, item)

	// Both labels should be removed by unblockAwaitingInput
	var removedPaused, removedAwaiting bool
	for _, call := range client.removeLabelCalls {
		if call.labelName == "fabrik:paused" {
			removedPaused = true
		}
		if call.labelName == "fabrik:awaiting-input" {
			removedAwaiting = true
		}
	}
	if !removedPaused {
		t.Error("expected fabrik:paused to be removed on unblock")
	}
	if !removedAwaiting {
		t.Error("expected fabrik:awaiting-input to be removed on unblock")
	}

	// fabrik:editing should be added — confirms processComments was entered
	var editingAdded bool
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:editing" {
			editingAdded = true
		}
	}
	if !editingAdded {
		t.Error("expected fabrik:editing to be added, confirming processComments was entered")
	}
}

func TestProcessItem_AwaitingInput_NoComment_Skips(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:   1,
		Title:    "Test",
		Status:   "Research",
		Labels:   []string{"fabrik:paused", "fabrik:awaiting-input"},
		Comments: nil,
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Should NOT remove labels or invoke Claude
	if len(client.removeLabelCalls) != 0 {
		t.Error("should not remove labels when no new comment on awaiting-input item")
	}
	if len(claude.calls) != 0 {
		t.Error("should not invoke claude when awaiting-input with no new comments")
	}
}

// --- (f) processItem calls blockOnInput, does not increment retryCount ---

func TestProcessItem_BlockedOnInput_AddsLabels(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, TokenUsage, error) {
			return "I need more info.\nFABRIK_BLOCKED_ON_INPUT\n", false, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			MaxRetries: 3,
			Stages:     testStages(),
		},
		client,
		claude,
		wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Title:  "Blocked Issue",
		Status: "Research",
		ItemID: "PVTI_10",
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// fabrik:paused and fabrik:awaiting-input should be added
	var addedPaused, addedAwaiting bool
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:paused" {
			addedPaused = true
		}
		if call.labelName == "fabrik:awaiting-input" {
			addedAwaiting = true
		}
	}
	if !addedPaused {
		t.Error("expected fabrik:paused to be added on blocked-on-input")
	}
	if !addedAwaiting {
		t.Error("expected fabrik:awaiting-input to be added on blocked-on-input")
	}

	// stage:<name>:failed should NOT be added
	for _, call := range client.addLabelCalls {
		if strings.Contains(call.labelName, ":failed") {
			t.Errorf("should not add failed label on blocked-on-input, got %q", call.labelName)
		}
	}

	// retryCount should NOT be incremented
	itemKey := "10-Research"
	eng.mu.Lock()
	count := eng.retryCount[itemKey]
	eng.mu.Unlock()
	if count != 0 {
		t.Errorf("retryCount should be 0 after blocked-on-input, got %d", count)
	}

	// Lock should be released (since blockOnInput calls releaseLock)
	var lockRemoved bool
	for _, call := range client.removeLabelCalls {
		if call.labelName == "fabrik:locked:testuser" {
			lockRemoved = true
		}
	}
	if !lockRemoved {
		t.Error("expected lock to be released on blocked-on-input")
	}
}

func TestProcessItem_BlockedOnInput_ProcessedSetEntry(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, TokenUsage, error) {
			return "FABRIK_BLOCKED_ON_INPUT\n", false, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", User: "testuser", Token: "token", Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 11, Title: "Blocked", Status: "Research", ItemID: "PVTI_11"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// processedSet should have an entry (Claude ran)
	itemKey := "11-Research"
	eng.mu.Lock()
	_, hasEntry := eng.processedSet[itemKey]
	eng.mu.Unlock()
	if !hasEntry {
		t.Error("processedSet should have entry after blocked-on-input (Claude ran)")
	}

	// Now simulate user comment arriving — unblockAwaitingInput should clear it
	stage := testStages()[0] // Research
	eng.unblockAwaitingInput(item, stage, itemKey)

	eng.mu.Lock()
	_, stillHasEntry := eng.processedSet[itemKey]
	eng.mu.Unlock()
	if stillHasEntry {
		t.Error("processedSet entry should be cleared after unblockAwaitingInput")
	}
}
