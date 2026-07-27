package engine

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestUsageLimitExit_ExemptFromRetryAndLabeledDistinctly is the core
// acceptance-criteria test: a usage-limit exit must not increment the retry
// count, must not lead to stage:<name>:failed or fabrik:paused, must apply
// fabrik:claude-limit, and must post an explanatory comment naming the
// condition and (when parseable) the reset time.
func TestUsageLimitExit_ExemptFromRetryAndLabeledDistinctly(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{TurnsUsed: 1, CostUSD: 0},
				&claudeUsageLimitError{Message: "hit your session limit", ResetTime: "10:20pm (America/Edmonton)"}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 100, Title: "Limit test", Status: "Research", ItemID: "PVTI_100"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Retry count must be 0 — the stage never ran.
	snap, err := eng.store.Get("owner/repo", 100)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts(\"Research\") = %d, want 0 (usage-limit must not count against max_retries)", got)
	}
	// LastAttemptAt must be set — the dispatch cooldown must apply so the
	// item doesn't retry on the very next poll and hammer the limit.
	if snap.LastAttemptAt("Research").IsZero() {
		t.Error("LastAttemptAt(\"Research\") is zero, want set (cooldown must apply)")
	}

	var sawLimitLabel, sawFailedLabel, sawPausedLabel bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:claude-limit":
			sawLimitLabel = true
		case "stage:Research:failed":
			sawFailedLabel = true
		case "fabrik:paused":
			sawPausedLabel = true
		}
	}
	if !sawLimitLabel {
		t.Error("expected fabrik:claude-limit label to be added")
	}
	if sawFailedLabel {
		t.Error("stage:Research:failed must NOT be applied for a usage-limit exit")
	}
	if sawPausedLabel {
		t.Error("fabrik:paused must NOT be applied for a usage-limit exit")
	}

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly 1 comment posted, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "usage limit") {
		t.Errorf("comment does not name the condition: %s", body)
	}
	if !strings.Contains(body, "10:20pm (America/Edmonton)") {
		t.Errorf("comment does not include the parsed reset time: %s", body)
	}
	if !strings.Contains(body, "max_retries") {
		t.Errorf("comment does not clarify retry-budget exemption: %s", body)
	}
}

// TestGenuineStageFailure_StillCountsAgainstRetries is the control case:
// a genuine non-zero exit (not a usage-limit exit) must be entirely
// unaffected by this feature — it still counts against max_retries and
// carries no fabrik:claude-limit label.
func TestGenuineStageFailure_StillCountsAgainstRetries(t *testing.T) {
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
			MaxRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 101, Title: "Genuine failure", Status: "Research", ItemID: "PVTI_101"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	snap, err := eng.store.Get("owner/repo", 101)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 1 {
		t.Errorf("Attempts(\"Research\") = %d, want 1 (genuine failure must count)", got)
	}

	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:claude-limit" {
			t.Error("fabrik:claude-limit must NOT be applied for a genuine stage failure")
		}
		if c.labelName == "fabrik:paused" {
			t.Error("should not escalate after a single failure below max_retries")
		}
	}
}

// TestStartFailure_ExcludedFromClaudeLimitHandling verifies that a process
// start failure (binary not found) — a pre-existing classification distinct
// from both "ran and failed" and "usage limit hit" — is unaffected: no
// retry-count increment (claudeRan=false skips it already), and no
// fabrik:claude-limit label.
func TestStartFailure_ExcludedFromClaudeLimitHandling(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			cmd := exec.Command("this-binary-does-not-exist-fabrik-test")
			_, startErr := cmd.Output()
			return "partial output", false, TokenUsage{}, startErr
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 102, Title: "Start failure", Status: "Research", ItemID: "PVTI_102"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	snap, err := eng.store.Get("owner/repo", 102)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts(\"Research\") = %d, want 0 (start failure must not count)", got)
	}
	if !snap.LastAttemptAt("Research").IsZero() {
		t.Error("LastAttemptAt should NOT be set on a start-failure error")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:claude-limit" {
			t.Error("fabrik:claude-limit must NOT be applied for a start failure")
		}
	}
}

// TestNormalCompletion_ClearsPriorClaudeLimitLabel verifies the auto-clear
// requirement: an issue carrying fabrik:claude-limit from a prior episode has
// the label removed on its next (non-limit) invocation, here a normal
// successful completion.
func TestNormalCompletion_ClearsPriorClaudeLimitLabel(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "done\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 103, Title: "Clears label", Status: "Research", ItemID: "PVTI_103",
		Labels: []string{"fabrik:claude-limit"},
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	var sawComplete, sawRemoval bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Research:complete" {
			sawComplete = true
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:claude-limit" {
			sawRemoval = true
		}
	}
	if !sawComplete {
		t.Error("expected stage:Research:complete to be added on successful completion")
	}
	if !sawRemoval {
		t.Error("expected fabrik:claude-limit to be removed on a non-limit invocation")
	}
}

// TestUsageLimitThenGenuineFailure_LeavesFullRetryBudget is the spec's
// explicit regression guard: a usage-limit exit followed by a real failure
// must leave the full retry budget available for the real failure — the
// limit hit must not have silently consumed one of the MaxRetries attempts.
func TestUsageLimitThenGenuineFailure_LeavesFullRetryBudget(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	callCount := 0
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			if callCount == 1 {
				return "", false, TokenUsage{}, &claudeUsageLimitError{Message: "hit your session limit"}
			}
			return "partial output", false, TokenUsage{}, errors.New("some genuine transient failure")
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 104, Title: "Limit then failure", Status: "Research", ItemID: "PVTI_104"}

	// First poll: usage-limit exit.
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (first call): %v", err)
	}
	// The first hit activated the account-wide Claude suspension (ADR-1120,
	// exercised separately by TestUsageLimitSuspension_ConcurrentDispatchShortCircuitsAfterDetection).
	// This test is about per-issue retry-budget semantics, not the suspension
	// window, so clear it here to simulate the second poll happening after the
	// suspension has lifted — otherwise the second call would be gated before
	// ever reaching Claude and callCount would never advance to the genuine
	// failure branch.
	eng.clearClaudeSuspension("test: simulate suspension window elapsed before second poll")
	// Second poll: item now carries fabrik:claude-limit, as it would after a
	// real board re-fetch following the first call's label add.
	item.Labels = []string{"fabrik:claude-limit"}
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (second call): %v", err)
	}

	snap, err := eng.store.Get("owner/repo", 104)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 1 {
		t.Errorf("Attempts(\"Research\") = %d, want 1 (the usage-limit hit must not have consumed budget)", got)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("must not escalate — only 1 of 2 max_retries consumed")
		}
	}
}

// TestUsageLimitExit_SecondConsecutiveHit_DoesNotRepostComment verifies the
// once-per-episode comment cadence: while fabrik:claude-limit remains applied
// across repeated poll cycles, the explanatory comment must be posted only
// once, not on every retry.
func TestUsageLimitExit_SecondConsecutiveHit_DoesNotRepostComment(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, &claudeUsageLimitError{Message: "hit your session limit"}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 105, Title: "Repeated limit hits", Status: "Research", ItemID: "PVTI_105"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (first call): %v", err)
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("after first hit: expected 1 comment, got %d", len(client.addCommentCalls))
	}

	// Simulate the next poll cycle observing the label the first call applied.
	item.Labels = []string{"fabrik:claude-limit"}
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (second call): %v", err)
	}
	if len(client.addCommentCalls) != 1 {
		t.Errorf("after second consecutive hit: expected comment count to stay at 1, got %d", len(client.addCommentCalls))
	}

	// Retry count must still be 0 after both calls.
	snap, err := eng.store.Get("owner/repo", 105)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts(\"Research\") = %d, want 0 after two usage-limit hits", got)
	}
}

// TestUsageLimitSuspension_ConcurrentDispatchShortCircuitsAfterDetection is the
// account-wide suspension acceptance-criteria test (ADR-1120): once one worker
// has detected the usage limit and activated the suspension, every other
// concurrently-dispatched item must short-circuit via the gate in
// runInvocationWithExtension without reaching e.claude.Invoke — "a single
// detection suspends all workers; subsequent items do not each invoke and
// fail."
func TestUsageLimitSuspension_ConcurrentDispatchShortCircuitsAfterDetection(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, &claudeUsageLimitError{Message: "hit your session limit", ResetTime: "10:20pm (America/Edmonton)"}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, MaxConcurrent: 5, Stages: testStages()},
		client, claude, wm,
	)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	// First dispatch: no suspension active yet, so this one actually reaches
	// Claude, detects the limit, and activates the account-wide suspension.
	detector := gh.ProjectItem{Number: 200, Title: "Detector", Status: "Research", ItemID: "PVTI_200"}
	if err := eng.processItem(context.Background(), board, detector); err != nil {
		t.Fatalf("processItem (detector): %v", err)
	}
	claude.mu.Lock()
	callsAfterDetection := len(claude.calls)
	claude.mu.Unlock()
	if callsAfterDetection != 1 {
		t.Fatalf("expected exactly 1 Claude invocation from the detecting call, got %d", callsAfterDetection)
	}
	if _, suspended := eng.claudeSuspendedUntilTime(time.Now()); !suspended {
		t.Fatal("expected account-wide Claude suspension to be active after detection")
	}

	// N concurrent dispatches for other items — the suspension is already
	// active, so every one of them must be gated in runInvocationWithExtension
	// before e.claude.Invoke is ever called.
	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			num := 201 + i
			item := gh.ProjectItem{Number: num, Title: fmt.Sprintf("Concurrent %d", num), Status: "Research", ItemID: fmt.Sprintf("PVTI_%d", num)}
			if err := eng.processItem(context.Background(), board, item); err != nil {
				t.Errorf("processItem (concurrent #%d): %v", num, err)
			}
		}(i)
	}
	wg.Wait()

	claude.mu.Lock()
	finalCalls := len(claude.calls)
	claude.mu.Unlock()
	if finalCalls != callsAfterDetection {
		t.Errorf("expected Claude invocation count to stay at %d while suspended, got %d — concurrent dispatch bypassed the gate", callsAfterDetection, finalCalls)
	}
}

// TestUsageLimitSuspension_UnrelatedErrorDoesNotClearConcurrentlyActivatedSuspension
// is a regression guard for a race in the "clear on non-limit error" path: an
// invocation that started before any suspension was active can still be
// in-flight when another worker detects the limit and activates the
// suspension. If the in-flight invocation then returns a generic, unrelated
// error (not a claudeUsageLimitError, and not a success), that must NOT clear
// the suspension the other worker just activated — a generic error is not
// evidence the account-wide limit has cleared. Only a successful invocation
// (err == nil) may clear it (ADR-1120's "early clear on success").
func TestUsageLimitSuspension_UnrelatedErrorDoesNotClearConcurrentlyActivatedSuspension(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	var eng *Engine
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Simulate a concurrent worker detecting the limit and activating the
			// account-wide suspension while THIS invocation is still in flight.
			eng.activateClaudeSuspension(999, "10:20pm (America/Edmonton)", time.Now())
			// This invocation's own outcome is unrelated to the usage limit.
			return "partial output", false, TokenUsage{}, errors.New("some genuine transient failure")
		},
	}

	eng = NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 106, Title: "Race regression", Status: "Research", ItemID: "PVTI_106"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if _, suspended := eng.claudeSuspendedUntilTime(time.Now()); !suspended {
		t.Error("expected the concurrently-activated suspension to survive this invocation's unrelated error, but it was cleared")
	}
}

// TestProcessComments_SuspendedGate_IsFullNoOp verifies the ADR-1120 claim in
// processComments' own gate comment: while the account-wide Claude suspension
// is active, the function must return immediately with zero side effects — no
// 👀 reaction, no fabrik:editing label, no Claude invocation — so the comment
// remains "new" and is retried on a later poll once dispatch resumes, rather
// than being partially consumed by a doomed cycle.
func TestProcessComments_SuspendedGate_IsFullNoOp(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngineWithRepo(t, client, claude)
	eng.activateClaudeSuspension(0, "10:20pm (America/Edmonton)", time.Now())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 11, Body: "spec"}
	userComments := []gh.Comment{
		{ID: "C_1", DatabaseID: 111, Author: "testuser", Body: "please continue"},
	}

	if err := eng.processComments(context.Background(), board, item, stage, userComments); err != nil {
		t.Fatalf("processComments returned error while suspended: %v", err)
	}

	if len(claude.forCommentsCalls) != 0 {
		t.Errorf("expected 0 Claude invocations while suspended, got %d", len(claude.forCommentsCalls))
	}
	if len(client.addCommentReactionCalls) != 0 {
		t.Errorf("expected 0 👀 reactions while suspended, got %d: %v", len(client.addCommentReactionCalls), client.addCommentReactionCalls)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:editing" {
			t.Error("expected fabrik:editing to NOT be added while suspended (gate must fire before any side effect)")
		}
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected 0 label mutations while suspended, got %d: %v", len(client.addLabelCalls), client.addLabelCalls)
	}
}
