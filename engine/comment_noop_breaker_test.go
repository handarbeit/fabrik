package engine

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// ---- effectiveMaxNoOpCommentCycles defaults (#1555) ----

func TestEffectiveMaxNoOpCommentCycles_Default(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	if got := eng.effectiveMaxNoOpCommentCycles(); got != 5 {
		t.Errorf("default MaxNoOpCommentCycles = %d, want 5", got)
	}
}

func TestEffectiveMaxNoOpCommentCycles_ConfigOverride(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxNoOpCommentCycles = 2

	if got := eng.effectiveMaxNoOpCommentCycles(); got != 2 {
		t.Errorf("MaxNoOpCommentCycles = %d, want 2", got)
	}
}

// ---- AC1/AC7: N consecutive successful no-op cycles trip the new breaker,
// where the old windowed breaker (#1089) alone would not ----
//
// TestNoOpCommentBreaker_TripsAfterThreshold_OldBreakerAlone_WouldNotTrip is
// the non-vacuous demonstration required by AC7 ("Criterion 1 must fail
// against current main"): MaxCommentCyclesPerWindow (the old breaker) is set
// far above the number of cycles this test drives, so if
// checkNoOpCommentCycle's increment/trip logic were removed (i.e. run
// against unmodified main, which has no success-agnostic breaker at all),
// nothing in this test would ever pause the issue — the assertion on
// fabrik:paused would fail. Only the new, never-time-pruned counter can trip
// within this test's cycle count.
func TestNoOpCommentBreaker_TripsAfterThreshold_OldBreakerAlone_WouldNotTrip(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := nonAdvancingClaude() // success (err==nil), completed=false, no commit — exactly AC3's shape
	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.MaxNoOpCommentCycles = 3
	// Old breaker's threshold set far above 3 so it can never be the one that
	// trips within this test — isolates the assertion to the new breaker.
	eng.cfg.MaxCommentCyclesPerWindow = 100
	eng.cfg.CommentCycleWindow = time.Hour

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Validate", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 60, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-bot", Body: "stale review body, already addressed"}}

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
		if strings.Contains(c.body, "no-op comment-processing circuit breaker") {
			tripComments++
			if !strings.Contains(c.body, "3 consecutive comment-processing invocation(s)") {
				t.Errorf("trip comment missing invocation count: %q", c.body)
			}
			if !strings.Contains(c.body, "some-bot") {
				t.Errorf("trip comment missing last comment author (R4): %q", c.body)
			}
		}
		// The old windowed breaker's distinct message must NOT have fired —
		// its threshold (100) was never reached.
		if strings.Contains(c.body, "comment-processing circuit breaker tripped**\n\nThis issue underwent **3 comment-processing invocations**") {
			t.Errorf("old windowed breaker should not have tripped (threshold 100, only 3 cycles): %q", c.body)
		}
	}
	if tripComments != 1 {
		t.Fatalf("expected exactly 1 no-op trip comment, got %d; addCommentCalls=%v", tripComments, client.addCommentCalls)
	}
}

func TestNoOpCommentBreaker_DoesNotTripBelowThreshold(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := nonAdvancingClaude()
	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.MaxNoOpCommentCycles = 3
	eng.cfg.MaxCommentCyclesPerWindow = 100
	eng.cfg.CommentCycleWindow = time.Hour

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Validate", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 61, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-bot", Body: "stale review body"}}

	for i := 0; i < 2; i++ {
		if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
			t.Fatalf("processComments call %d: %v", i, err)
		}
	}

	repoStr := "owner/repo"
	snap, err := eng.store.Get(repoStr, 61)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.NoOpCommentCycles("Validate"); got != 2 {
		t.Errorf("NoOpCommentCycles(Validate) = %d, want 2", got)
	}

	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused should not be added before the threshold is reached")
		}
	}
}

// ---- AC3: a successful (err==nil) invocation that produces no commit and no
// marker still counts — the core non-vacuous case ----

func TestNoOpCommentBreaker_SuccessfulNoOpInvocation_StillCounts(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := nonAdvancingClaude()
	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.MaxNoOpCommentCycles = 100 // never trips — isolates the counting assertion
	eng.cfg.MaxCommentCyclesPerWindow = 100
	eng.cfg.CommentCycleWindow = time.Hour

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Validate", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 62, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-bot", Body: "stale review body"}}

	if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	repoStr := "owner/repo"
	snap, err := eng.store.Get(repoStr, 62)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.NoOpCommentCycles("Validate"); got != 1 {
		t.Fatalf("NoOpCommentCycles(Validate) after one successful no-op invocation = %d, want 1 — a clean (err==nil) exit must not exempt this cycle from counting (the entire defect this counter closes)", got)
	}
}

// ---- AC2/R2: a cycle that lands a commit resets the counter ----

func TestNoOpCommentBreaker_CommitResetsCounter(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	var callCount int
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			if callCount <= 2 {
				return "still looking, no change yet.\n", false, TokenUsage{}, nil
			}
			cmd := exec.Command("git", "commit", "--allow-empty", "-m", "progress commit")
			cmd.Dir = workDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git commit: %s: %v", out, err)
			}
			return "pushed a fix.\n", false, TokenUsage{}, nil
		},
	}
	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.MaxNoOpCommentCycles = 5
	eng.cfg.MaxCommentCyclesPerWindow = 100
	eng.cfg.CommentCycleWindow = time.Hour

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Implement", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 63, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "testuser", Body: "please fix"}}

	for i := 0; i < 2; i++ {
		if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
			t.Fatalf("processComments call %d: %v", i, err)
		}
	}
	repoStr := "owner/repo"
	snap, err := eng.store.Get(repoStr, 63)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.NoOpCommentCycles("Implement"); got != 2 {
		t.Fatalf("NoOpCommentCycles(Implement) after 2 no-op cycles = %d, want 2", got)
	}

	// Third cycle lands a commit — must reset the counter to 0.
	if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
		t.Fatalf("processComments call 3: %v", err)
	}
	snap, err = eng.store.Get(repoStr, 63)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.NoOpCommentCycles("Implement"); got != 0 {
		t.Errorf("NoOpCommentCycles(Implement) after commit-landing cycle = %d, want 0 (reset, R2)", got)
	}
}

// ---- AC5/R4: the trip comment names the redelivered comment's author and the count ----
// (also asserted inline in TestNoOpCommentBreaker_TripsAfterThreshold_OldBreakerAlone_WouldNotTrip
// above; this test isolates it against a distinct author to guard against a
// hardcoded/stale string match.)

func TestNoOpCommentBreaker_TripComment_NamesAuthorAndCount(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := nonAdvancingClaude()
	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.MaxNoOpCommentCycles = 2
	eng.cfg.MaxCommentCyclesPerWindow = 100
	eng.cfg.CommentCycleWindow = time.Hour

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Validate", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 64, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "handarbeit-pruefer", Body: "stale review body"}}

	for i := 0; i < 2; i++ {
		if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
			t.Fatalf("processComments call %d: %v", i, err)
		}
	}

	var found bool
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "no-op comment-processing circuit breaker") {
			found = true
			if !strings.Contains(c.body, "handarbeit-pruefer") {
				t.Errorf("trip comment missing author %q: %q", "handarbeit-pruefer", c.body)
			}
			if !strings.Contains(c.body, "2 consecutive comment-processing invocation(s)") {
				t.Errorf("trip comment missing count: %q", c.body)
			}
			if !strings.Contains(c.body, "Validate") {
				t.Errorf("trip comment missing stage name: %q", c.body)
			}
		}
	}
	if !found {
		t.Fatal("expected a no-op circuit breaker trip comment")
	}
}

// ---- AC6/R5: no double-counting, and the old failure-path breaker (#1382)
// still trips independently on a failing invocation ----

// TestNoOpCommentBreaker_OldBreakerStillTripsOnSetupFailure verifies the old
// #1382 failure-keyed breaker is unaffected by the new success-agnostic one:
// a persistently failing setup step (editing-label add) still trips the old
// breaker at its own threshold, with the new breaker's higher threshold never
// reached — regression guard for R5's "must not regress #1382."
func TestNoOpCommentBreaker_OldBreakerStillTripsOnSetupFailure(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:editing" {
				return errors.New("simulated GraphQL budget exhaustion")
			}
			return nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.MaxCommentCyclesPerWindow = 3
	eng.cfg.CommentCycleWindow = time.Hour
	eng.cfg.MaxNoOpCommentCycles = 100 // must not be the one that trips

	item := gh.ProjectItem{Number: 65, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-bot", Body: "still stuck?"}}

	for i := 0; i < 3; i++ {
		if err := eng.processComments(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Review", Order: 1}, comments); err == nil {
			t.Fatalf("processComments call %d: expected error from failing editing-label add, got nil", i)
		}
	}

	var paused bool
	var oldTrip, newTrip int
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			paused = true
		}
	}
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "no-op comment-processing circuit breaker") {
			newTrip++
		} else if strings.Contains(c.body, "circuit breaker") {
			oldTrip++
		}
	}
	if !paused {
		t.Fatal("expected fabrik:paused after the old breaker trips")
	}
	if oldTrip != 1 {
		t.Errorf("expected exactly 1 old-breaker trip comment, got %d", oldTrip)
	}
	if newTrip != 0 {
		t.Errorf("expected 0 new-breaker trip comments (threshold 100 never reached), got %d", newTrip)
	}

	// The new counter still recorded every cycle independently (R5: both
	// counters record every invocation; only the pause/escalation is
	// mutually exclusive).
	repoStr := "owner/repo"
	snap, err := eng.store.Get(repoStr, 65)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.NoOpCommentCycles("Review"); got != 3 {
		t.Errorf("NoOpCommentCycles(Review) = %d, want 3 (recorded independently even though the old breaker tripped first)", got)
	}
}

// TestNoOpCommentBreaker_NewBreakerTrip_SuppressesOldBreakerCheckSameCycle
// verifies R5's ordering resolution directly: when the new breaker's
// threshold is reached on a cycle, the old breaker's check is skipped for
// that same cycle — at most one pause comment/escalation per cycle. The old
// breaker's own threshold is set low enough that, absent the skip, it too
// would have plenty of invocations recorded and could double-pause.
func TestNoOpCommentBreaker_NewBreakerTrip_SuppressesOldBreakerCheckSameCycle(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := nonAdvancingClaude()
	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.MaxNoOpCommentCycles = 3
	eng.cfg.MaxCommentCyclesPerWindow = 3 // same threshold — both would be satisfied on cycle 3
	eng.cfg.CommentCycleWindow = time.Hour

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Validate", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 66, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-bot", Body: "stale review body"}}

	for i := 0; i < 3; i++ {
		if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
			t.Fatalf("processComments call %d: %v", i, err)
		}
	}

	var pauseLabelCount int
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pauseLabelCount++
		}
	}
	if pauseLabelCount != 1 {
		t.Fatalf("expected fabrik:paused applied exactly once (not double-escalated), got %d applications", pauseLabelCount)
	}

	var oldTrip, newTrip int
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "no-op comment-processing circuit breaker") {
			newTrip++
		} else if strings.Contains(c.body, "🏭 **Fabrik — comment-processing circuit breaker tripped**") {
			oldTrip++
		}
	}
	if newTrip != 1 {
		t.Errorf("expected exactly 1 new-breaker trip comment, got %d", newTrip)
	}
	if oldTrip != 0 {
		t.Errorf("expected 0 old-breaker trip comments — the new breaker's trip must short-circuit the old check for the same cycle (R5), got %d", oldTrip)
	}
}

// ---- Manual unpause resets the counter (mirrors TestCommentBreaker_ResetOnManualUnpause) ----

func TestNoOpCommentBreaker_ResetOnManualUnpause(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	item := gh.ProjectItem{
		Number: 67,
		Repo:   "owner/repo",
		Labels: []string{"stage:Review:failed", "fabrik:paused"},
	}
	stage := &stages.Stage{Name: "Review", Order: 1}

	for i := 0; i < 4; i++ {
		eng.checkNoOpCommentCycle(item, stage, false, "some-bot")
	}
	repoStr := "owner/repo"
	snap, err := eng.store.Get(repoStr, 67)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.NoOpCommentCycles("Review"); got != 4 {
		t.Fatalf("NoOpCommentCycles(Review) before manual unpause = %d, want 4", got)
	}

	eng.clearFailedStage(item, stage)

	snap, err = eng.store.Get(repoStr, 67)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.NoOpCommentCycles("Review"); got != 0 {
		t.Errorf("NoOpCommentCycles(Review) after clearFailedStage = %d, want 0 (reset)", got)
	}
}
