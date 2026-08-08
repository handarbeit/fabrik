package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// TestTurnCapPreemption_IncrementsSliceRetriesOnly verifies the core #1199 fix:
// a turn-cap exit (CLI subtype error_max_turns, surfaced as *claudeTurnLimitError)
// increments the new SliceRetries counter, not Attempts, and must not apply
// stage:<name>:failed or fabrik:paused — it is a resumable preemption, not a
// failure.
func TestTurnCapPreemption_IncrementsSliceRetriesOnly(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "partial output", false, TokenUsage{TurnsUsed: 50, MaxTurns: 50},
				&claudeTurnLimitError{TerminalReason: "max_turns", NumTurns: 50}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, MaxSliceRetries: 5, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 200, Title: "Turn-cap test", Status: "Research", ItemID: "PVTI_200"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	snap, err := eng.store.Get("owner/repo", 200)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.SliceRetries("Research"); got != 1 {
		t.Errorf("SliceRetries(\"Research\") = %d, want 1 (turn-cap exit must count as a slice)", got)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts(\"Research\") = %d, want 0 (turn-cap exit must NOT count against max_retries)", got)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Research:failed" {
			t.Error("stage:Research:failed must NOT be applied for a turn-cap preemption — it has not failed")
		}
		if c.labelName == "fabrik:paused" {
			t.Error("should not pause on a single slice, well below MaxSliceRetries")
		}
	}
}

// TestGenuineError_IncrementsFailureCounterOnly_NotSlice verifies that a
// non-turn-limited error keeps counting against Attempts/MaxRetries exactly as
// before, and never touches SliceRetries — the two counters are structurally
// distinguished by turnLimited, not by inference.
func TestGenuineError_IncrementsFailureCounterOnly_NotSlice(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "partial output", false, TokenUsage{}, errors.New("some genuine transient failure")
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, MaxSliceRetries: 5, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 201, Title: "Genuine error test", Status: "Research", ItemID: "PVTI_201"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	snap, err := eng.store.Get("owner/repo", 201)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 1 {
		t.Errorf("Attempts(\"Research\") = %d, want 1 (genuine failure must count against max_retries)", got)
	}
	if got := snap.SliceRetries("Research"); got != 0 {
		t.Errorf("SliceRetries(\"Research\") = %d, want 0 (a genuine error is not a slice)", got)
	}
}

// TestSliceLimit_Escalates_WithCorrectMessage_NoFailedLabel verifies that
// exceeding MaxSliceRetries pauses the issue (fabrik:paused + fabrik:awaiting-input,
// mirroring pauseForRebaseCycleLimit) without ever applying stage:<name>:failed,
// and that the posted comment describes a slice-budget condition, not a failure.
func TestSliceLimit_Escalates_WithCorrectMessage_NoFailedLabel(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "partial output", false, TokenUsage{TurnsUsed: 50, MaxTurns: 50},
				&claudeTurnLimitError{TerminalReason: "max_turns", NumTurns: 50}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, MaxSliceRetries: 3, Stages: testStages()},
		client, claude, wm,
	)

	// Pre-fill SliceRetries to one below the limit so this invocation is the one
	// that crosses it.
	for i := 0; i < eng.cfg.MaxSliceRetries-1; i++ {
		eng.store.Apply(itemstate.SliceRetryIncremented{Repo: "owner/repo", Number: 202, StageName: "Research"})
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 202, Title: "Slice limit test", Status: "Research", ItemID: "PVTI_202"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	snap, err := eng.store.Get("owner/repo", 202)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.SliceRetries("Research"); got != eng.cfg.MaxSliceRetries {
		t.Errorf("SliceRetries(\"Research\") = %d, want %d (at the limit)", got, eng.cfg.MaxSliceRetries)
	}

	wantAdded := map[string]bool{"fabrik:paused": false, "fabrik:awaiting-input": false}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Research:failed" {
			t.Error("stage:Research:failed must NOT be applied when the slice budget is exceeded — it has not failed")
		}
		if _, ok := wantAdded[c.labelName]; ok {
			wantAdded[c.labelName] = true
		}
	}
	for label, found := range wantAdded {
		if !found {
			t.Errorf("expected label %q to be added on slice-limit escalation", label)
		}
	}

	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected a slice-limit comment to be posted")
	}
	body := client.addCommentCalls[len(client.addCommentCalls)-1].body
	if strings.Contains(body, "failed to complete after") {
		t.Errorf("comment must not describe this as a failure, got: %q", body)
	}
	if !strings.Contains(body, "slice budget") {
		t.Errorf("comment should mention 'slice budget', got: %q", body)
	}
	if !strings.Contains(body, "--max-slice-retries") {
		t.Errorf("comment should name the override flag --max-slice-retries, got: %q", body)
	}
	if !strings.Contains(body, "fabrik:extend-turns") {
		t.Errorf("comment should suggest fabrik:extend-turns, got: %q", body)
	}
}

// TestJobExceedingMaxRetriesSlices_DoesNotPause verifies the issue's headline
// Definition-of-Done claim: a job that has taken more turn-cap slices than
// MaxRetries (the old, overloaded ceiling) is not paused, as long as it is still
// within MaxSliceRetries — the two counters are now independent.
func TestJobExceedingMaxRetriesSlices_DoesNotPause(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "partial output", false, TokenUsage{TurnsUsed: 50, MaxTurns: 50},
				&claudeTurnLimitError{TerminalReason: "max_turns", NumTurns: 50}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, MaxSliceRetries: 10, Stages: testStages()},
		client, claude, wm,
	)

	// Simulate a job already 5 slices in — more than MaxRetries (2) but well
	// under MaxSliceRetries (10).
	for i := 0; i < 5; i++ {
		eng.store.Apply(itemstate.SliceRetryIncremented{Repo: "owner/repo", Number: 203, StageName: "Research"})
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 203, Title: "Large job test", Status: "Research", ItemID: "PVTI_203"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	snap, err := eng.store.Get("owner/repo", 203)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.SliceRetries("Research"); got != 6 {
		t.Errorf("SliceRetries(\"Research\") = %d, want 6", got)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts(\"Research\") = %d, want 0 — slices never touch the failure counter", got)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" || c.labelName == "stage:Research:failed" {
			t.Errorf("must not pause/fail a job that has exceeded MaxRetries in slices alone (label %q added)", c.labelName)
		}
	}
}
