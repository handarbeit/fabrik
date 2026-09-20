package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Tests for #1812: a reinvoke whose Claude invocation provably never ran
// (usage limit, api_error, apiKeyHelper, account suspension) must not be
// charged a review/rebase/CI-fix cycle; every other error keeps today's
// charge (R3/R4).

func TestClassifyDidNotRun(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want didNotRunKind
	}{
		{"nil", nil, ""},
		{"usage limit", &claudeUsageLimitError{}, didNotRunUsageLimit},
		{"api error", &claudeAPIErrorExit{TerminalReason: "api_error", NumTurns: 1}, didNotRunAPIError},
		{"wrapped api error", fmt.Errorf("wrap: %w", &claudeAPIErrorExit{TerminalReason: "api_error"}), didNotRunAPIError},
		{"api key helper", &apiKeyHelperDetectedError{Layer: "worktree", Path: "x"}, didNotRunAPIKeyHelper},
		{"turn limit", &claudeTurnLimitError{TerminalReason: "max_turns", NumTurns: 50}, ""},
		{"resume failure", &claudeResumeFailureError{Cause: errors.New("boom")}, ""},
		{"tools denied", &claudeToolsDeniedError{ToolNames: []string{"Bash"}}, ""},
		{"generic", errors.New("network failure publishing output"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyDidNotRun(c.err); got != c.want {
				t.Errorf("classifyDidNotRun(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// reviewReinvokeRound dispatches one review reinvoke (blocked=false) for a
// fresh COMMENTED review and waits for the worker to finish. The mock Claude
// returns claudeErr (nil = clean no-op run).
func newDidNotRunReviewEngine(t *testing.T, claudeErr error, calls *int) (*Engine, *mockGitHubClient, []*stages.Stage) {
	t.Helper()
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			*calls++
			return "", false, TokenUsage{}, claudeErr
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true)},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, stgs)
	eng.cfg.MaxReviewCycles = 3
	if _, err := eng.worktreesFor("owner/repo").EnsureWorktree(31, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	return eng, client, stgs
}

func dispatchReviewRound(t *testing.T, eng *Engine, stg *stages.Stage, id int) {
	t.Helper()
	item := gh.ProjectItem{
		Number:         31,
		Repo:           "owner/repo",
		Labels:         []string{"stage:Implement:complete"},
		LinkedPRNumber: 100,
		LinkedPRReviews: []gh.PRReview{{
			Author:     "handarbeit-pruefer",
			State:      "COMMENTED",
			Body:       "**File:** engine/foo.go\nmissing nil check.",
			DatabaseID: id,
		}},
	}
	pctx := &phase1Ctx{
		ctx:           context.Background(),
		board:         &gh.ProjectBoard{ProjectID: "PVT_1"},
		item:          item,
		stage:         stg,
		hasComplete:   true,
		advancedItems: map[string]bool{},
	}
	if !eng.handleReviewGate(pctx) {
		t.Fatalf("round %d: handleReviewGate did not claim the item", id)
	}
	eng.wg.Wait()
}

func pausedByLabel(client *mockGitHubClient) bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			return true
		}
	}
	return false
}

// The #1777 shape: five api_error reinvokes in a row must leave ReviewCycles at
// zero and never pause; the never-refunded tally records all five.
func TestReviewReinvoke_APIErrorStorm_NeverChargedNeverPaused(t *testing.T) {
	var calls int
	eng, client, stgs := newDidNotRunReviewEngine(t, &claudeAPIErrorExit{TerminalReason: "api_error", NumTurns: 1}, &calls)
	// Keep the sibling no-op breaker (#1555) out of this test's way.
	eng.cfg.MaxNoOpCommentCycles = 100
	eng.cfg.MaxCommentCyclesPerWindow = 100

	for i := 0; i < 5; i++ {
		dispatchReviewRound(t, eng, stgs[0], 3000+i)
		snap, _ := eng.store.Get("owner/repo", 31)
		if got := snap.ReviewCycles("Implement"); got != 0 {
			t.Fatalf("round %d: ReviewCycles = %d, want 0 (did-not-run must be refunded)", i+1, got)
		}
		if got := snap.ReviewBlockedCycles("Implement"); got != 0 {
			t.Fatalf("round %d: ReviewBlockedCycles = %d, want 0", i+1, got)
		}
		if got := snap.DidNotRunReinvokes("Implement"); got != i+1 {
			t.Errorf("round %d: DidNotRunReinvokes = %d, want %d", i+1, got, i+1)
		}
		if pausedByLabel(client) {
			t.Fatalf("round %d: issue paused at the review cycle limit despite no invocation ever running", i+1)
		}
	}
	if calls != 5 {
		t.Errorf("Claude invoked %d times, want 5", calls)
	}
}

func TestReviewReinvoke_UsageLimitNilReturn_Refunded(t *testing.T) {
	var calls int
	eng, _, stgs := newDidNotRunReviewEngine(t, &claudeUsageLimitError{}, &calls)
	dispatchReviewRound(t, eng, stgs[0], 3100)
	snap, _ := eng.store.Get("owner/repo", 31)
	if got := snap.ReviewCycles("Implement"); got != 0 {
		t.Errorf("ReviewCycles = %d, want 0", got)
	}
	if got := snap.DidNotRunReinvokes("Implement"); got != 1 {
		t.Errorf("DidNotRunReinvokes = %d, want 1", got)
	}
}

// Account-wide suspension gate (ADR-1120): processComments returns before any
// side effect, so the skipped dispatch also never ran and is refunded.
func TestReviewReinvoke_SuspensionGate_Refunded(t *testing.T) {
	var calls int
	eng, _, stgs := newDidNotRunReviewEngine(t, nil, &calls)
	eng.activateClaudeSuspension(1, "", time.Now())
	dispatchReviewRound(t, eng, stgs[0], 3150)
	if calls != 0 {
		t.Fatalf("Claude invoked %d times under suspension, want 0", calls)
	}
	snap, _ := eng.store.Get("owner/repo", 31)
	if got := snap.ReviewCycles("Implement"); got != 0 {
		t.Errorf("ReviewCycles = %d, want 0", got)
	}
	if got := snap.DidNotRunReinvokes("Implement"); got != 1 {
		t.Errorf("DidNotRunReinvokes = %d, want 1", got)
	}
}

// R3: errors that may have done real work keep today's charge.
func TestReviewReinvoke_OtherErrors_StayCharged(t *testing.T) {
	errs := map[string]error{
		"turn limit":     &claudeTurnLimitError{TerminalReason: "max_turns", NumTurns: 50},
		"resume failure": &claudeResumeFailureError{Cause: errors.New("boom")},
		"tools denied":   &claudeToolsDeniedError{ToolNames: []string{"Bash"}},
		"generic":        errors.New("crashed mid-run"),
	}
	for name, cerr := range errs {
		t.Run(name, func(t *testing.T) {
			var calls int
			eng, _, stgs := newDidNotRunReviewEngine(t, cerr, &calls)
			dispatchReviewRound(t, eng, stgs[0], 3200)
			snap, _ := eng.store.Get("owner/repo", 31)
			if got := snap.ReviewCycles("Implement"); got != 1 {
				t.Errorf("ReviewCycles = %d, want 1 (a non-did-not-run error must stay charged)", got)
			}
			if got := snap.DidNotRunReinvokes("Implement"); got != 0 {
				t.Errorf("DidNotRunReinvokes = %d, want 0", got)
			}
		})
	}
}

// A did-not-run refund and the #1045 HEAD-unchanged refund are disjoint: a clean
// no-op run nets to zero exactly once, and records no did-not-run tally.
func TestReviewReinvoke_CleanNoOp_NoDidNotRunTally(t *testing.T) {
	var calls int
	eng, _, stgs := newDidNotRunReviewEngine(t, nil, &calls)
	dispatchReviewRound(t, eng, stgs[0], 3300)
	snap, _ := eng.store.Get("owner/repo", 31)
	if got := snap.ReviewCycles("Implement"); got != 0 {
		t.Errorf("ReviewCycles = %d, want 0 (#1045 no-op refund)", got)
	}
	if got := snap.DidNotRunReinvokes("Implement"); got != 0 {
		t.Errorf("DidNotRunReinvokes = %d, want 0", got)
	}
}

// The blocked-gate variant: ReviewBlockedCycles is refunded too, but only for a
// did-not-run exit.
func TestReviewReinvoke_BlockedCharged_RefundsBlockedCounterOnlyWhenDidNotRun(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		wantBlocked int
		wantTally   int
	}{
		{"did not run", &claudeAPIErrorExit{TerminalReason: "api_error", NumTurns: 1}, 0, 1},
		{"ran and no-op'd", nil, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			eng, _, stgs := newDidNotRunReviewEngine(t, tc.err, &calls)
			item := gh.ProjectItem{Number: 31, Repo: "owner/repo", LinkedPRNumber: 100,
				LinkedPRReviews: []gh.PRReview{{Author: "handarbeit-pruefer", State: "COMMENTED", Body: "**File:** engine/foo.go\nmissing nil check.", DatabaseID: 3400}}}
			// Mirror handleReviewGate's blocked pre-dispatch charge.
			eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 31, StageName: "Implement"})
			eng.store.Apply(itemstate.ReviewBlockedCycleIncremented{Repo: "owner/repo", Number: 31, StageName: "Implement"})
			eng.dispatchReviewReinvoke(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stgs[0],
				[]gh.Comment{{ID: "x", Body: "finding", Author: "handarbeit-pruefer"}}, true)
			eng.wg.Wait()
			snap, _ := eng.store.Get("owner/repo", 31)
			if got := snap.ReviewBlockedCycles("Implement"); got != tc.wantBlocked {
				t.Errorf("ReviewBlockedCycles = %d, want %d", got, tc.wantBlocked)
			}
			if got := snap.DidNotRunReinvokes("Implement"); got != tc.wantTally {
				t.Errorf("DidNotRunReinvokes = %d, want %d", got, tc.wantTally)
			}
		})
	}
}

func TestCIFixAndRebaseReinvoke_DidNotRun_RefundedAndAfterSkipped(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, &claudeAPIErrorExit{TerminalReason: "api_error", NumTurns: 1}
		},
	}
	stgs := []*stages.Stage{{Name: "Implement", Order: 1, Prompt: "implement"}}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, stgs)
	if _, err := eng.worktreesFor("owner/repo").EnsureWorktree(31, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	item := gh.ProjectItem{Number: 31, Repo: "owner/repo", LinkedPRNumber: 100, Labels: []string{"fabrik:auto-merge-enabled"}}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	eng.store.Apply(itemstate.CIFixCycleIncremented{Repo: "owner/repo", Number: 31, StageName: "Implement"})
	eng.store.Apply(itemstate.RebaseCycleIncremented{Repo: "owner/repo", Number: 31, StageName: "Implement"})

	eng.dispatchCIFixReinvoke(context.Background(), board, item, stgs[0], PRSettleResult{})
	eng.wg.Wait()
	eng.dispatchRebaseReinvoke(context.Background(), board, item, stgs[0])
	eng.wg.Wait()

	snap, _ := eng.store.Get("owner/repo", 31)
	if got := snap.CIFixCycles("Implement"); got != 0 {
		t.Errorf("CIFixCycles = %d, want 0", got)
	}
	if got := snap.RebaseCycles("Implement"); got != 0 {
		t.Errorf("RebaseCycles = %d, want 0", got)
	}
	if got := snap.DidNotRunReinvokes("Implement"); got != 2 {
		t.Errorf("DidNotRunReinvokes = %d, want 2", got)
	}
	// after hooks skipped: no bogus CI-fix no-op debounce recorded.
	if sha := snap.LastCIFixNoOpSHA(); sha != "" {
		t.Errorf("LastCIFixNoOpSHA = %q, want empty (after hook must be skipped)", sha)
	}
}

func TestDidNotRunPauseNote(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	repo := "owner/repo"
	rec := func(n int) {
		for i := 0; i < n; i++ {
			eng.store.Apply(itemstate.DidNotRunReinvokeRecorded{Repo: repo, Number: 9, StageName: "Validate"})
		}
	}
	eng.store.Apply(itemstate.LocalLabelAdded{Repo: repo, Number: 9, Label: "x"})

	if note, dom := eng.didNotRunPauseNote(repo, 9, "Validate", 5); note != "" || dom {
		t.Errorf("zero tally: got (%q, %v), want empty", note, dom)
	}
	rec(2)
	if note, dom := eng.didNotRunPauseNote(repo, 9, "Validate", 5); dom || !strings.Contains(note, "2 re-invocation(s)") {
		t.Errorf("partial tally: got (%q, %v)", note, dom)
	}
	rec(3)
	note, dom := eng.didNotRunPauseNote(repo, 9, "Validate", 5)
	if !dom || !strings.Contains(note, "never ran") || !strings.Contains(note, "seconds") {
		t.Errorf("dominant tally: got (%q, %v)", note, dom)
	}
}

func TestPauseForReviewCycleLimit_DidNotRunMessage(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	stage := &stages.Stage{Name: "Validate"}
	item := gh.ProjectItem{Number: 9, Repo: "owner/repo"}

	// Zero tally: unchanged text, including the stable dedup fragment.
	eng.pauseForReviewCycleLimit(&gh.ProjectBoard{}, item, stage, 5, 5)
	body := lastCommentBody(client)
	if !strings.Contains(body, reviewCyclePauseFragment(stage)) || !strings.Contains(body, "repeatedly requesting changes") {
		t.Errorf("zero-tally pause message changed:\n%s", body)
	}

	// Dominant tally: misdiagnosis sentence replaced, fragment kept.
	item2 := gh.ProjectItem{Number: 10, Repo: "owner/repo"}
	for i := 0; i < 5; i++ {
		eng.store.Apply(itemstate.DidNotRunReinvokeRecorded{Repo: "owner/repo", Number: 10, StageName: "Validate"})
	}
	eng.pauseForReviewCycleLimit(&gh.ProjectBoard{}, item2, stage, 5, 5)
	body = lastCommentBody(client)
	if strings.Contains(body, "repeatedly requesting changes") {
		t.Errorf("did-not-run pause still misdiagnoses:\n%s", body)
	}
	if !strings.Contains(body, reviewCyclePauseFragment(stage)) || !strings.Contains(body, "never ran") {
		t.Errorf("did-not-run pause message missing fragment or cause:\n%s", body)
	}
}

func lastCommentBody(c *mockGitHubClient) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.addCommentCalls) == 0 {
		return ""
	}
	return c.addCommentCalls[len(c.addCommentCalls)-1].body
}
