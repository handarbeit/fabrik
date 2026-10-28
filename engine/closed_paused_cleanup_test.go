package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// cpRemovedLabels returns the labels the mock was asked to remove from issue n.
func cpRemovedLabels(c *mockGitHubClient, n int) map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for _, call := range c.removeLabelCalls {
		if call.issueNumber == n {
			out[call.labelName] = true
		}
	}
	return out
}

func cpAddedLabel(c *mockGitHubClient, n int, label string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, call := range c.addLabelCalls {
		if call.issueNumber == n && call.labelName == label {
			return true
		}
	}
	return false
}

// TestClosedPausedCleanup_Admission covers FR-001, FR-003 and FR-005: a closed
// non-PR item at a cleanup stage is admitted despite the pause labels by both
// admission functions; an open paused item and a closed PR are not (#2459).
func TestClosedPausedCleanup_Admission(t *testing.T) {
	cases := []struct {
		name       string
		item       gh.ProjectItem
		wantNeeds  bool
		wantMayNow bool // itemMayNeedWork (no worktree on disk in these cases)
	}{
		{"closed paused+awaiting", gh.ProjectItem{Number: 1, Status: "Done", IsClosed: true, Labels: []string{"fabrik:paused", "fabrik:awaiting-input"}}, true, true},
		{"closed paused only", gh.ProjectItem{Number: 2, Status: "Done", IsClosed: true, Labels: []string{"fabrik:paused"}}, true, true},
		{"closed awaiting-input only", gh.ProjectItem{Number: 3, Status: "Done", IsClosed: true, Labels: []string{"fabrik:awaiting-input"}}, true, true},
		{"open paused", gh.ProjectItem{Number: 4, Status: "Done", Labels: []string{"fabrik:paused", "fabrik:awaiting-input"}}, false, false},
		{"closed PR paused", gh.ProjectItem{Number: 5, Status: "Done", IsClosed: true, IsPR: true, Labels: []string{"fabrik:paused"}}, false, false},
		{"closed already complete", gh.ProjectItem{Number: 6, Status: "Done", IsClosed: true, Labels: []string{"fabrik:paused", "stage:Done:complete"}}, false, false},
	}
	for _, tc := range cases {
		for _, disabled := range []bool{false, true} {
			name := tc.name
			if disabled {
				name += "/seam-on"
			}
			t.Run(name, func(t *testing.T) {
				eng := testEngineWithCleanup(t, &mockGitHubClient{}, &mockClaudeInvoker{})
				eng.SetClosedPausedCleanupDisabledForTest(disabled)
				want := tc.wantNeeds
				if disabled && tc.wantNeeds && hasLabel(tc.item.Labels, "fabrik:paused") {
					want = false // today's behaviour: fabrik:paused vetoes
				}
				if got := eng.itemNeedsWork(tc.item); got != want {
					t.Errorf("itemNeedsWork = %v, want %v", got, want)
				}
				// itemMayNeedWork never had a pause check, so it is unaffected by the seam.
				if got := eng.itemMayNeedWork(tc.item); got != tc.wantMayNow {
					t.Errorf("itemMayNeedWork = %v, want %v", got, tc.wantMayNow)
				}
			})
		}
	}
}

// TestClosedPausedCleanup_ProcessItem_CleansWithoutClaude drives processItem for
// a closed paused item that also has a human comment after the pause: it must be
// cleaned up and never reach processComments (ADR 1387).
func TestClosedPausedCleanup_ProcessItem_CleansWithoutClaude(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngineWithCleanup(t, client, claude)

	item := gh.ProjectItem{
		Repo: "owner/repo", Number: 7, Status: "Done", IsClosed: true,
		Labels: []string{"fabrik:paused", "fabrik:awaiting-input"},
		Comments: []gh.Comment{{
			DatabaseID: 99, Author: "someone", Body: "hello?",
			CreatedAt: time.Now().Add(time.Hour),
		}},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	claude.mu.Lock()
	nInvoke, nComments := len(claude.calls), len(claude.forCommentsCalls)
	claude.mu.Unlock()
	if nInvoke != 0 || nComments != 0 {
		t.Errorf("Claude invoked (%d stage, %d comment) for a closed cleanup item", nInvoke, nComments)
	}
	rm := cpRemovedLabels(client, 7)
	if !rm["fabrik:paused"] || !rm["fabrik:awaiting-input"] {
		t.Errorf("pause pair not cleared, removed=%v", rm)
	}
	if !cpAddedLabel(client, 7, "stage:Done:complete") {
		t.Error("stage:Done:complete not applied")
	}
	client.mu.Lock()
	nComment := len(client.addCommentCalls)
	client.mu.Unlock()
	if nComment != 0 {
		t.Errorf("posted %d comment(s), want none (FR-004)", nComment)
	}
}

// TestClosedPausedCleanup_ProcessItem_OpenPausedUntouched covers FR-003 at the
// dispatch point: an open paused item is skipped and nothing is written.
func TestClosedPausedCleanup_ProcessItem_OpenPausedUntouched(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithCleanup(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Repo: "owner/repo", Number: 8, Status: "Done",
		Labels: []string{"fabrik:paused", "fabrik:awaiting-input"},
	}
	if err := eng.processItem(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if rm := cpRemovedLabels(client, 8); len(rm) != 0 {
		t.Errorf("removed labels from open paused item: %v", rm)
	}
	if cpAddedLabel(client, 8, "stage:Done:complete") {
		t.Error("completion label applied to open paused item")
	}
}

// TestClosedPausedCleanup_SeamRestoresOldBehaviour: with the seam on, a closed
// paused item is skipped by processItem as before the fix.
func TestClosedPausedCleanup_SeamRestoresOldBehaviour(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithCleanup(t, client, &mockClaudeInvoker{})
	eng.SetClosedPausedCleanupDisabledForTest(true)
	item := gh.ProjectItem{
		Repo: "owner/repo", Number: 9, Status: "Done", IsClosed: true,
		Labels: []string{"fabrik:paused", "fabrik:awaiting-input"},
	}
	if err := eng.processItem(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if cpAddedLabel(client, 9, "stage:Done:complete") {
		t.Error("seam on: completion label applied")
	}
}

// TestClosedPausedCleanup_RemovalFailureDefersCompletion: a real removal error
// must leave the completion label unapplied so the whole cleanup is retried.
func TestClosedPausedCleanup_RemovalFailureDefersCompletion(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, n int, label string) error {
			if label == "fabrik:paused" {
				return errors.New("boom")
			}
			return nil
		},
	}
	eng := testEngineWithCleanup(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Repo: "owner/repo", Number: 10, Status: "Done", IsClosed: true,
		Labels: []string{"fabrik:paused", "fabrik:awaiting-input"},
	}
	eng.handleCleanupStage(item, eng.cfg.Stages[len(eng.cfg.Stages)-1], "owner/repo")
	if cpAddedLabel(client, 10, "stage:Done:complete") {
		t.Error("completion label applied despite failed pause removal")
	}
}

// TestClosedPausedCleanup_ErrNotFoundIsSuccess: a pause label that is already
// gone (404) counts as cleared.
func TestClosedPausedCleanup_ErrNotFoundIsSuccess(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, n int, label string) error {
			return gh.ErrNotFound
		},
	}
	eng := testEngineWithCleanup(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Repo: "owner/repo", Number: 11, Status: "Done", IsClosed: true}
	if !eng.clearPauseOnClosedCleanup(item) {
		t.Error("ErrNotFound should count as cleared")
	}
}
