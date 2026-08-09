package engine

import (
	"context"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestAPIErrorExit_ExemptFromRetryNoLabelNoComment is the core
// acceptance-criteria test (#1458 Acceptance 1): an api_error exit must not
// increment the retry count, and — unlike claudeUsageLimitError/
// apiKeyHelperDetectedError — must apply no label at all and post no
// comment (R4's "fifth, more minimal shape"). Assert on the counter itself,
// not just on the absence of a pause, per the issue's explicit instruction
// that "not paused" alone is vacuous.
func TestAPIErrorExit_ExemptFromRetryNoLabelNoComment(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{TurnsUsed: 1, CostUSD: 0},
				&claudeAPIErrorExit{TerminalReason: "api_error", NumTurns: 1, CostUSD: 0}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 300, Title: "api_error test", Status: "Research", ItemID: "PVTI_300"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Retry count must be 0 — the stage never ran.
	snap, err := eng.store.Get("owner/repo", 300)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts(\"Research\") = %d, want 0 (api_error must not count against max_retries)", got)
	}
	// LastAttemptAt must be set — the dispatch cooldown must apply so the
	// item doesn't retry on the very next poll.
	if snap.LastAttemptAt("Research").IsZero() {
		t.Error("LastAttemptAt(\"Research\") is zero, want set (cooldown must apply)")
	}

	// fabrik:locked:<user> and stage:<name>:in_progress are pre-invocation
	// dispatch bookkeeping applied before Claude ever runs — unrelated to
	// R4's "no durable condition-specific label" claim. What R4 forbids is
	// any label describing the api_error condition itself (the
	// fabrik:claude-limit/fabrik:api-key-helper-detected shape).
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:locked:testuser", "stage:Research:in_progress":
			continue
		default:
			t.Errorf("expected no condition-specific label for an api_error exit (R4), got %q", c.labelName)
		}
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected zero comments posted for an api_error exit (R4), got %d", len(client.addCommentCalls))
	}
}

// TestAPIErrorExit_ThreeConsecutiveThenSuccess_CompletesNormally is the
// end-to-end #1408 scenario (#1458 Acceptance 2): three consecutive
// api_error exits must not apply stage:<name>:failed or fabrik:paused, and a
// subsequent successful invocation must complete the stage normally.
func TestAPIErrorExit_ThreeConsecutiveThenSuccess_CompletesNormally(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	callCount := 0
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			if callCount <= 3 {
				return "", false, TokenUsage{TurnsUsed: 1, CostUSD: 0},
					&claudeAPIErrorExit{TerminalReason: "api_error", NumTurns: 1, CostUSD: 0}
			}
			return "done\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 301, Title: "Three api_error then success", Status: "Research", ItemID: "PVTI_301"}

	for i := 0; i < 3; i++ {
		if err := eng.processItem(context.Background(), board, item); err != nil {
			t.Fatalf("processItem (api_error call %d): %v", i+1, err)
		}
	}

	snap, err := eng.store.Get("owner/repo", 301)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts(\"Research\") = %d, want 0 after 3 api_error exits (max_retries=2 would otherwise have paused the issue)", got)
	}

	// Fourth call: a genuine successful invocation.
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (success call): %v", err)
	}

	var sawFailedLabel, sawPausedLabel, sawCompleteLabel bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "stage:Research:failed":
			sawFailedLabel = true
		case "fabrik:paused":
			sawPausedLabel = true
		case "stage:Research:complete":
			sawCompleteLabel = true
		}
	}
	if sawFailedLabel {
		t.Error("stage:Research:failed must NOT be applied — three api_error exits never charged against max_retries")
	}
	if sawPausedLabel {
		t.Error("fabrik:paused must NOT be applied — three api_error exits never charged against max_retries")
	}
	if !sawCompleteLabel {
		t.Error("expected stage:Research:complete to be added once the successful invocation lands")
	}
	if callCount != 4 {
		t.Errorf("expected exactly 4 Claude invocations, got %d", callCount)
	}
}

// TestAPIErrorExit_DoesNotActivateAccountWideSuspension verifies #1458
// Acceptance 4: unlike claudeUsageLimitError, an api_error exit is
// per-invocation/transient and must never touch claudeSuspendedUntilTime or
// any account-wide suspension state.
func TestAPIErrorExit_DoesNotActivateAccountWideSuspension(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{TurnsUsed: 1, CostUSD: 0},
				&claudeAPIErrorExit{TerminalReason: "api_error", NumTurns: 1, CostUSD: 0}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 302, Title: "No account-wide suspension", Status: "Research", ItemID: "PVTI_302"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if _, suspended := eng.claudeSuspendedUntilTime(time.Now()); suspended {
		t.Error("api_error exit must NOT activate the account-wide Claude suspension")
	}
}
