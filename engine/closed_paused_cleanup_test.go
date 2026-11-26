package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
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

// --- #2628: landing-label fill in the Done cleanup of a closed item -----------

func cpMergedPRClient() *mockGitHubClient {
	return &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 8, Merged: true, State: "closed"}, nil
		},
	}
}

func cpAddedOrder(c *mockGitHubClient, n int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, call := range c.addLabelCalls {
		if call.issueNumber == n {
			out = append(out, call.labelName)
		}
	}
	return out
}

func cpHasCooldown(eng *Engine, n int) bool {
	st, ok := eng.store.Peek("owner/repo", n)
	if !ok {
		return false
	}
	return !st.CooldownAt(reasonPeriodicReEval).IsZero()
}

func cpDoneStage(eng *Engine) *stages.Stage {
	return eng.cfg.Stages[len(eng.cfg.Stages)-1]
}

func TestClosedCleanupFill_MergedPRFillsBeforeDoneLabel(t *testing.T) {
	for _, paused := range []bool{false, true} {
		name := "not paused"
		labels := []string{"stage:Implement:complete"}
		if paused {
			name = "paused"
			labels = append(labels, "fabrik:paused", "fabrik:awaiting-input")
		}
		t.Run(name, func(t *testing.T) {
			client := cpMergedPRClient()
			eng := testEngineWithStages(t, client, terminalAdvanceStages())
			item := gh.ProjectItem{Repo: "owner/repo", Number: 20, Status: "Done", IsClosed: true, Labels: labels}
			eng.handleCleanupStage(item, cpDoneStage(eng), "owner/repo")

			got := cpAddedOrder(client, 20)
			want := []string{"stage:Review:complete", "stage:Validate:complete", "stage:Done:complete"}
			if len(got) != len(want) {
				t.Fatalf("added labels = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("added labels = %v, want %v", got, want)
				}
			}
			if paused {
				if rm := cpRemovedLabels(client, 20); !rm["fabrik:paused"] || !rm["fabrik:awaiting-input"] {
					t.Errorf("pause pair not cleared: %v", rm)
				}
			}
			client.mu.Lock()
			nComments := len(client.addCommentCalls)
			client.mu.Unlock()
			if nComments != 0 {
				t.Errorf("posted %d comment(s), want none", nComments)
			}
			if cpAddedLabel(client, 20, "fabrik:awaiting-landing-verification") {
				t.Error("landing-verification marker added")
			}
		})
	}
}

func TestClosedCleanupFill_OnlyMissingLabelsFilled(t *testing.T) {
	client := cpMergedPRClient()
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	item := gh.ProjectItem{Repo: "owner/repo", Number: 21, Status: "Done", IsClosed: true,
		Labels: []string{"stage:Implement:complete", "stage:Review:complete"}}
	eng.handleCleanupStage(item, cpDoneStage(eng), "owner/repo")
	got := cpAddedOrder(client, 21)
	if len(got) != 2 || got[0] != "stage:Validate:complete" || got[1] != "stage:Done:complete" {
		t.Errorf("added labels = %v", got)
	}
}

func TestClosedCleanupFill_ReadFailureDefers(t *testing.T) {
	cases := map[string]*mockGitHubClient{
		"FetchLinkedPR error": {
			fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) { return nil, errors.New("boom") },
		},
		"FetchPRMerged error": {
			fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 8, State: "closed"}, nil
			},
			fetchPRMergedFn: func(owner, repo string, n int) (bool, error) { return false, errors.New("boom") },
		},
	}
	for name, client := range cases {
		t.Run(name, func(t *testing.T) {
			eng := testEngineWithStages(t, client, terminalAdvanceStages())
			item := gh.ProjectItem{Repo: "owner/repo", Number: 22, Status: "Done", IsClosed: true,
				Labels: []string{"stage:Implement:complete"}}
			eng.handleCleanupStage(item, cpDoneStage(eng), "owner/repo")
			if got := cpAddedOrder(client, 22); len(got) != 0 {
				t.Errorf("labels added despite failed read: %v", got)
			}
			if !cpHasCooldown(eng, 22) {
				t.Error("no periodic-re-eval cooldown recorded")
			}
		})
	}
}

func TestClosedCleanupFill_FillFailureDefers(t *testing.T) {
	client := cpMergedPRClient()
	client.addLabelToIssueFn = func(owner, repo string, n int, label string) error {
		if label == "stage:Validate:complete" {
			return errors.New("boom")
		}
		return nil
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	item := gh.ProjectItem{Repo: "owner/repo", Number: 23, Status: "Done", IsClosed: true,
		Labels: []string{"stage:Implement:complete"}}
	eng.handleCleanupStage(item, cpDoneStage(eng), "owner/repo")
	if cpAddedLabel(client, 23, "stage:Done:complete") {
		t.Error("completion label applied despite failed fill")
	}
	if !cpHasCooldown(eng, 23) {
		t.Error("no periodic-re-eval cooldown recorded")
	}
}

func TestClosedCleanupFill_NoFillShapes(t *testing.T) {
	cases := map[string]struct {
		client *mockGitHubClient
		item   gh.ProjectItem
	}{
		"no linked PR": {&mockGitHubClient{}, gh.ProjectItem{IsClosed: true}},
		"open PR": {&mockGitHubClient{fetchLinkedPRFn: func(o, r string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 8, State: "open"}, nil
		}}, gh.ProjectItem{IsClosed: true}},
		"closed unmerged PR": {&mockGitHubClient{fetchLinkedPRFn: func(o, r string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 8, State: "closed"}, nil
		}}, gh.ProjectItem{IsClosed: true}},
		"open item": {cpMergedPRClient(), gh.ProjectItem{}},
		"PR card":   {cpMergedPRClient(), gh.ProjectItem{IsClosed: true, IsPR: true}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			eng := testEngineWithStages(t, tc.client, terminalAdvanceStages())
			item := tc.item
			item.Repo, item.Number, item.Status = "owner/repo", 24, "Done"
			item.Labels = []string{"stage:Implement:complete"}
			eng.handleCleanupStage(item, cpDoneStage(eng), "owner/repo")
			got := cpAddedOrder(tc.client, 24)
			if len(got) != 1 || got[0] != "stage:Done:complete" {
				t.Errorf("added labels = %v, want only stage:Done:complete", got)
			}
		})
	}
}

func TestClosedCleanupFill_AlreadyCompleteUntouched(t *testing.T) {
	client := cpMergedPRClient()
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	item := gh.ProjectItem{Repo: "owner/repo", Number: 25, Status: "Done", IsClosed: true,
		Labels: []string{"stage:Implement:complete", "stage:Done:complete"}}
	eng.handleCleanupStage(item, cpDoneStage(eng), "owner/repo")
	if got := cpAddedOrder(client, 25); len(got) != 0 {
		t.Errorf("added labels = %v, want none", got)
	}
}

func TestClosedCleanupFill_SeamDisablesFillOnly(t *testing.T) {
	client := cpMergedPRClient()
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	eng.SetClosedCleanupFillDisabledForTest(true)
	item := gh.ProjectItem{Repo: "owner/repo", Number: 26, Status: "Done", IsClosed: true,
		Labels: []string{"stage:Implement:complete", "fabrik:paused", "fabrik:awaiting-input"}}
	eng.handleCleanupStage(item, cpDoneStage(eng), "owner/repo")
	got := cpAddedOrder(client, 26)
	if len(got) != 1 || got[0] != "stage:Done:complete" {
		t.Errorf("added labels = %v, want only stage:Done:complete", got)
	}
	if rm := cpRemovedLabels(client, 26); !rm["fabrik:paused"] {
		t.Error("seam must not disable the pause clearing (#2459)")
	}
}
