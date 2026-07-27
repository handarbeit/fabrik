package engine

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// ---- effectiveCommentBreakerWindow defaults (#1089) ----

func TestEffectiveCommentBreakerWindow_Defaults(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	n, window := eng.effectiveCommentBreakerWindow()
	if n != 10 {
		t.Errorf("default MaxCommentCyclesPerWindow = %d, want 10", n)
	}
	if window != 30*time.Minute {
		t.Errorf("default CommentCycleWindow = %v, want 30m", window)
	}
}

func TestEffectiveCommentBreakerWindow_ConfigOverride(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxCommentCyclesPerWindow = 3
	eng.cfg.CommentCycleWindow = 5 * time.Minute

	n, window := eng.effectiveCommentBreakerWindow()
	if n != 3 {
		t.Errorf("MaxCommentCyclesPerWindow = %d, want 3", n)
	}
	if window != 5*time.Minute {
		t.Errorf("CommentCycleWindow = %v, want 5m", window)
	}
}

// ---- commentBreakerCount prune-on-read window math (#1089) ----

func TestCommentBreakerCount_PrunesOutsideWindow(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.CommentCycleWindow = 30 * time.Minute

	item := gh.ProjectItem{Number: 5, Repo: "owner/repo"}
	repoStr := "owner/repo"

	// Two invocations outside the window, three inside.
	eng.store.Apply(itemstate.CommentBreakerInvocationRecorded{Repo: repoStr, Number: 5, At: time.Now().Add(-time.Hour), Author: "a"})
	eng.store.Apply(itemstate.CommentBreakerInvocationRecorded{Repo: repoStr, Number: 5, At: time.Now().Add(-45 * time.Minute), Author: "b"})
	eng.store.Apply(itemstate.CommentBreakerInvocationRecorded{Repo: repoStr, Number: 5, At: time.Now().Add(-10 * time.Minute), Author: "c"})
	eng.store.Apply(itemstate.CommentBreakerInvocationRecorded{Repo: repoStr, Number: 5, At: time.Now().Add(-5 * time.Minute), Author: "d"})
	eng.store.Apply(itemstate.CommentBreakerInvocationRecorded{Repo: repoStr, Number: 5, At: time.Now(), Author: "e"})

	if got := eng.commentBreakerCount(item); got != 3 {
		t.Errorf("commentBreakerCount = %d, want 3 (outside-window invocations pruned)", got)
	}
}

// TestRecordCommentBreakerInvocation_PrunesStoredSliceAtWriteTime is a
// regression guard: recordCommentBreakerInvocation must prune stale entries
// out of the Store's InvocationsAt slice at write time (not just filter them
// out at read time in commentBreakerCount), or an issue that receives
// invocations sparser than the window — never tripping, never hitting a reset
// trigger — grows InvocationsAt without bound over the issue's lifetime.
func TestRecordCommentBreakerInvocation_PrunesStoredSliceAtWriteTime(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.CommentCycleWindow = 30 * time.Minute
	item := gh.ProjectItem{Number: 6, Repo: "owner/repo"}
	repoStr := "owner/repo"

	// Two stale invocations, well outside the 30m window.
	eng.store.Apply(itemstate.CommentBreakerInvocationRecorded{Repo: repoStr, Number: 6, At: time.Now().Add(-2 * time.Hour), Author: "a"})
	eng.store.Apply(itemstate.CommentBreakerInvocationRecorded{Repo: repoStr, Number: 6, At: time.Now().Add(-time.Hour), Author: "b"})

	// A fresh invocation through the real recording path should prune the two
	// stale entries above, not just the new one appended.
	eng.recordCommentBreakerInvocation(item, "c")

	snap, err := eng.store.Get(repoStr, 6)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	invocations := snap.CommentBreakerInvocationsAt()
	if len(invocations) != 1 {
		t.Errorf("InvocationsAt len = %d, want 1 (stale entries pruned at write time, not left to accumulate)", len(invocations))
	}
}

// ---- Trip after N non-advancing comment-processing cycles (#1089) ----

// nonAdvancingClaude returns a mockClaudeInvoker whose InvokeForComments never
// signals completion, never touches the issue body, and never commits — i.e.
// a comment-processing cycle that makes no forward progress whatsoever.
func nonAdvancingClaude() *mockClaudeInvoker {
	return &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "still looking into it, no change yet.\n", false, TokenUsage{}, nil
		},
	}
}

func TestCommentBreaker_TripsAfterThreshold(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := nonAdvancingClaude()
	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.MaxCommentCyclesPerWindow = 3
	eng.cfg.CommentCycleWindow = time.Hour

	ch := make(chan tui.Event, 64)
	eng.events = ch

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Review", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 30, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-bot", Body: "still stuck?"}}

	for i := 0; i < 3; i++ {
		if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
			t.Fatalf("processComments call %d: %v", i, err)
		}
	}

	var paused, awaitingInput bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			paused = true
		}
		if c.labelName == "fabrik:awaiting-input" {
			awaitingInput = true
		}
	}
	if !paused || !awaitingInput {
		t.Fatalf("expected fabrik:paused + fabrik:awaiting-input after trip; addLabelCalls=%v", client.addLabelCalls)
	}

	var tripComments int
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "circuit breaker") {
			tripComments++
			if !strings.Contains(c.body, "3 comment-processing invocations") {
				t.Errorf("trip comment missing invocation count: %q", c.body)
			}
		}
	}
	if tripComments != 1 {
		t.Fatalf("expected exactly 1 trip comment, got %d", tripComments)
	}

	events := collectEvents(ch, 20*time.Millisecond)
	var sawTrip bool
	for _, ev := range events {
		if te, ok := ev.(tui.CommentBreakerTrippedEvent); ok {
			sawTrip = true
			if te.InvocationCount != 3 {
				t.Errorf("CommentBreakerTrippedEvent.InvocationCount = %d, want 3", te.InvocationCount)
			}
			if te.IssueNumber != 30 {
				t.Errorf("CommentBreakerTrippedEvent.IssueNumber = %d, want 30", te.IssueNumber)
			}
		}
	}
	if !sawTrip {
		t.Error("expected a CommentBreakerTrippedEvent to be emitted")
	}
}

func TestCommentBreaker_DoesNotTripBelowThreshold(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := nonAdvancingClaude()
	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.MaxCommentCyclesPerWindow = 3
	eng.cfg.CommentCycleWindow = time.Hour

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Review", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 31, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-bot", Body: "still stuck?"}}

	for i := 0; i < 2; i++ {
		if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
			t.Fatalf("processComments call %d: %v", i, err)
		}
	}

	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused should not be added before the threshold is reached")
		}
	}
}

// ---- Reset triggers (#1089) ----

func TestCommentBreaker_ResetOnStageComplete(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	item := gh.ProjectItem{Number: 40, ItemID: "PVTI_40", Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Research"}

	for i := 0; i < 5; i++ {
		eng.recordCommentBreakerInvocation(item, "some-bot")
	}
	if got := eng.commentBreakerCount(item); got != 5 {
		t.Fatalf("commentBreakerCount before completion = %d, want 5", got)
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	eng.handleStageComplete(context.Background(), board, item, stage)

	if got := eng.commentBreakerCount(item); got != 0 {
		t.Errorf("commentBreakerCount after stage:*:complete = %d, want 0 (reset)", got)
	}
}

func TestCommentBreaker_ResetOnNewCommit(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			cmd := exec.Command("git", "commit", "--allow-empty", "-m", "progress commit")
			cmd.Dir = workDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git commit: %s: %v", out, err)
			}
			return "pushed a fix.\n", false, TokenUsage{}, nil
		},
	}
	eng := testEngineWithRepo(t, client, claude)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Implement", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 41, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "testuser", Body: "please fix"}}

	if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// The invocation was recorded, but the mid-cycle commit must have reset it.
	if got := eng.commentBreakerCount(item); got != 0 {
		t.Errorf("commentBreakerCount after new commit = %d, want 0 (reset)", got)
	}
}

func TestCommentBreaker_ResetOnIssueBodyUpdate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Specify"}

	for i := 0; i < 4; i++ {
		eng.recordCommentBreakerInvocation(item, "testuser")
	}
	if got := eng.commentBreakerCount(item); got != 4 {
		t.Fatalf("commentBreakerCount before body update = %d, want 4", got)
	}

	output := "FABRIK_ISSUE_UPDATE_BEGIN\nupdated spec body\nFABRIK_ISSUE_UPDATE_END\nresolved the open question.\n"
	eng.publishCommentOutput("owner", "repo", item, stage, nil, output, "", "main")

	if got := eng.commentBreakerCount(item); got != 0 {
		t.Errorf("commentBreakerCount after FABRIK_ISSUE_UPDATE = %d, want 0 (reset)", got)
	}
}

func TestCommentBreakerObserver_ResetsOnPRStateChanged(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 43, Repo: "owner/repo"}

	for i := 0; i < 6; i++ {
		eng.recordCommentBreakerInvocation(item, "testuser")
	}
	if got := eng.commentBreakerCount(item); got != 6 {
		t.Fatalf("commentBreakerCount before PR state change = %d, want 6", got)
	}

	obs := &CommentBreakerObserver{Store: eng.store}
	obs.OnChange(itemstate.Change{Repo: "owner/repo", Number: 43, Fields: itemstate.LinkedPRChanged | itemstate.PRStateChanged}, itemstate.Snapshot{})

	if got := eng.commentBreakerCount(item); got != 0 {
		t.Errorf("commentBreakerCount after PRStateChanged = %d, want 0 (reset)", got)
	}
}

func TestCommentBreakerObserver_IgnoresUnrelatedChange(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 44, Repo: "owner/repo"}

	eng.recordCommentBreakerInvocation(item, "testuser")

	obs := &CommentBreakerObserver{Store: eng.store}
	obs.OnChange(itemstate.Change{Repo: "owner/repo", Number: 44, Fields: itemstate.CommentsChanged}, itemstate.Snapshot{})

	if got := eng.commentBreakerCount(item); got != 1 {
		t.Errorf("commentBreakerCount after unrelated change = %d, want 1 (unchanged)", got)
	}
}

// TestCommentBreakerObserver_IgnoresLinkedPRChangedWithoutPRStateChanged is a
// regression guard: LinkedPRChanged alone (without PRStateChanged) must NOT
// reset the breaker. LinkedPRChanged also fires on a new PR review or inline
// review comment (PRReviewSubmitted / PRReviewCommentCreated /
// ReviewThreadCommentAdded) — the same event that supplies the comment
// processComments is about to process for a review-reinvoke. Resetting on
// that broader flag would zero the counter on every review-comment cycle,
// capping the observed count at 1 forever and defeating the breaker for its
// primary PR-review-comment-loop use case (caught during PR review of #1089).
func TestCommentBreakerObserver_IgnoresLinkedPRChangedWithoutPRStateChanged(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 47, Repo: "owner/repo"}

	for i := 0; i < 4; i++ {
		eng.recordCommentBreakerInvocation(item, "review-bot")
	}
	if got := eng.commentBreakerCount(item); got != 4 {
		t.Fatalf("commentBreakerCount before review comment = %d, want 4", got)
	}

	// Simulate a new PR review comment arriving: LinkedPRChanged fires (as
	// PRReviewCommentCreated returns), but PRStateChanged does not.
	obs := &CommentBreakerObserver{Store: eng.store}
	obs.OnChange(itemstate.Change{Repo: "owner/repo", Number: 47, Fields: itemstate.LinkedPRChanged | itemstate.CommentsChanged}, itemstate.Snapshot{})

	if got := eng.commentBreakerCount(item); got != 4 {
		t.Errorf("commentBreakerCount after LinkedPRChanged-only (review comment) = %d, want 4 (unchanged — must not reset)", got)
	}
}

func TestCommentBreaker_ResetOnManualUnpause(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	item := gh.ProjectItem{
		Number: 45,
		Repo:   "owner/repo",
		Labels: []string{"stage:Review:failed", "fabrik:paused"},
	}

	for i := 0; i < 7; i++ {
		eng.recordCommentBreakerInvocation(item, "some-bot")
	}
	if got := eng.commentBreakerCount(item); got != 7 {
		t.Fatalf("commentBreakerCount before manual unpause = %d, want 7", got)
	}

	reviewStage := &stages.Stage{Name: "Review", Order: 1}
	eng.clearFailedStage(item, reviewStage)

	if got := eng.commentBreakerCount(item); got != 0 {
		t.Errorf("commentBreakerCount after clearFailedStage = %d, want 0 (reset)", got)
	}
}

// ---- Breaker's pause must be honorable (ADR-069 regression guard, #1089) ----

// TestCommentBreaker_PauseNotStrippedByBotComment mirrors
// TestProcessItem_Paused_BotComment_Retained: once the circuit breaker has
// tripped and applied fabrik:paused + fabrik:awaiting-input, a subsequent
// bot-authored comment must not silently resume the issue — the exact
// failure mode ADR-069 (fix 1/4) closed for operator-applied pauses. The
// breaker's pause reuses the same labels through the same pauseIssue
// machinery, so this is a regression guard that ADR-069 stays honored here too.
func TestCommentBreaker_PauseNotStrippedByBotComment(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(t, client, claude)
	eng.cfg.MaxCommentCyclesPerWindow = 2
	eng.cfg.CommentCycleWindow = time.Hour

	item := gh.ProjectItem{Number: 46, Repo: "owner/repo"}

	// Trip the breaker directly (equivalent to two non-advancing cycles).
	eng.recordCommentBreakerInvocation(item, "some-bot")
	eng.recordCommentBreakerInvocation(item, "some-bot")
	eng.checkCommentBreaker(item)

	var pausedApplied, awaitingInputApplied bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedApplied = true
		}
		if c.labelName == "fabrik:awaiting-input" {
			awaitingInputApplied = true
		}
	}
	if !pausedApplied || !awaitingInputApplied {
		t.Fatalf("expected the breaker to apply fabrik:paused + fabrik:awaiting-input; addLabelCalls=%v", client.addLabelCalls)
	}

	// Now simulate the tripped issue as seen on the next poll: both labels
	// present, and only a bot comment arrived since.
	client.removeLabelCalls = nil
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	pausedItem := gh.ProjectItem{
		Number: 46,
		Title:  "runaway loop repro",
		Status: "Review",
		Labels: []string{"fabrik:paused", "fabrik:awaiting-input"},
		Comments: []gh.Comment{
			{ID: "C1", Author: "gemini-code-assist", Body: "quota notice"},
		},
	}

	if err := eng.processItem(context.Background(), board, pausedItem); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused should not be removed when only a bot comment is present after a breaker trip")
		}
		if c.labelName == "fabrik:awaiting-input" {
			t.Error("fabrik:awaiting-input should not be removed when only a bot comment is present after a breaker trip")
		}
	}
	if len(claude.calls) != 0 || len(claude.forCommentsCalls) != 0 {
		t.Error("should not invoke claude while the breaker's pause is honored against a bot-only comment")
	}
}
