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

// TestSliceLimit_ManualUnpauseResetsSliceRetries verifies the review fix for #1199:
// pauseForSliceLimit applies itemstate.EnginePaused (unlike pauseForRebaseCycleLimit,
// whose counter has an independent path back to progress — a human resolves the
// underlying merge conflict directly, changing settle.Status). SliceRetries has no
// such independent signal, so without EnginePaused, processItem's unpause guard
// (wasPaused || hasFailedLabel — both false after a slice-limit pause, since no
// failed label is ever applied) would never fire clearFailedStage, SliceRetries
// would never reset, and the pause comment's own documented recovery ("remove
// fabrik:paused to resume") would be a no-op: the next dispatch takes exactly one
// more slice, re-checks a counter already at the limit, and re-pauses immediately.
//
// This test drives the real pauseForSliceLimit call site (via two turn-capped
// processItem calls, not a hand-rolled store mutation) to prove the escalation path
// itself sets PausedByEngine, then simulates the user removing fabrik:paused and
// asserts the COUNTER resets — not merely the pause flag/label, which the review
// flagged as the vacuous assertion a naive test would settle for.
func TestSliceLimit_ManualUnpauseResetsSliceRetries(t *testing.T) {
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
			MaxRetries: 2, MaxSliceRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 210, Title: "Slice unpause test", Status: "Research", ItemID: "PVTI_210"}

	// Call 1: SliceRetries 0 -> 1, below MaxSliceRetries(2), no pause.
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (1st): %v", err)
	}
	// Call 2: SliceRetries 1 -> 2, hits MaxSliceRetries — pauseForSliceLimit fires
	// for real, through the actual escalation code path.
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (2nd): %v", err)
	}

	snap, err := eng.store.Get("owner/repo", 210)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.SliceRetries("Research"); got != eng.cfg.MaxSliceRetries {
		t.Fatalf("SliceRetries after escalation = %d, want %d", got, eng.cfg.MaxSliceRetries)
	}
	if !snap.PausedByEngine("Research") {
		t.Fatal("expected PausedByEngine to be true after pauseForSliceLimit — without this, removing fabrik:paused can never reset SliceRetries (the fix under test)")
	}

	// Simulate the user removing fabrik:paused and fabrik:awaiting-input: a fresh
	// item value with no such labels, same issue number, same store. processItem
	// resets SliceRetries via clearFailedStage AND, since nothing else stops it,
	// immediately resumes dispatch within this same call — so the post-unpause
	// snapshot reflects "reset to 0, then one fresh slice" (1), not a bare 0. What
	// matters is that the counter did NOT stay stuck at the limit: if the reset had
	// not happened (the bug under test), this same turn-capped dispatch would have
	// pushed 2 -> 3 and immediately re-paused instead.
	unpausedItem := gh.ProjectItem{Number: 210, Title: "Slice unpause test", Status: "Research", ItemID: "PVTI_210", Labels: []string{}}
	if err := eng.processItem(context.Background(), board, unpausedItem); err != nil {
		t.Fatalf("processItem (unpause): %v", err)
	}

	snap, err = eng.store.Get("owner/repo", 210)
	if err != nil {
		t.Fatalf("store.Get after unpause: %v", err)
	}
	// The assertion the review specifically called out as missing: the COUNTER
	// must genuinely reset (and only accrue one fresh slice from the resumed
	// dispatch), not merely the pause label/flag, and not stay pinned at the limit.
	if got := snap.SliceRetries("Research"); got != 1 {
		t.Errorf("SliceRetries after manual unpause = %d, want 1 (reset to 0 by clearFailedStage, then one fresh slice from the resumed dispatch — not stuck at the limit)", got)
	}
	if snap.PausedByEngine("Research") {
		t.Error("expected PausedByEngine to be cleared after unpause, and not immediately re-paused by the resumed dispatch")
	}

	pausedCount := 0
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedCount++
		}
	}
	if pausedCount != 1 {
		t.Errorf("fabrik:paused applied %d time(s) across the whole test, want 1 (only the original escalation — a stuck counter would re-pause immediately after unpause)", pausedCount)
	}
}

// TestSliceLimit_UnpauseThenJobCompletes is the operator-visible follow-through of
// TestSliceLimit_ManualUnpauseResetsSliceRetries: after a manual unpause resets
// SliceRetries, a job needing several more slices runs them without immediately
// re-pausing (each stays well under the freshly-reset MaxSliceRetries budget) and
// eventually completes — at which point StageRetryCleared fires again on normal
// completion, resetting both counters exactly as before this issue.
func TestSliceLimit_UnpauseThenJobCompletes(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	var callCount int
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			// Calls 1-2: turn-capped, crossing MaxSliceRetries(2) on call 2 (pause).
			// Call 3: turn-capped again, after the simulated unpause (SliceRetries 0->1).
			// Call 4: completes cleanly.
			if callCount <= 3 {
				return "partial output", false, TokenUsage{TurnsUsed: 50, MaxTurns: 50},
					&claudeTurnLimitError{TerminalReason: "max_turns", NumTurns: 50}
			}
			return "all done\nFABRIK_STAGE_COMPLETE", true, TokenUsage{TurnsUsed: 10, MaxTurns: 50}, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, MaxSliceRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 211, Title: "Unpause-then-complete test", Status: "Research", ItemID: "PVTI_211"}

	for i := 0; i < 2; i++ {
		if err := eng.processItem(context.Background(), board, item); err != nil {
			t.Fatalf("processItem (pre-pause call %d): %v", i+1, err)
		}
	}

	// Confirm the job is actually paused before proceeding, so the rest of this
	// test genuinely exercises "unpause, then finish" rather than passing
	// vacuously because the job never paused.
	snap, err := eng.store.Get("owner/repo", 211)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !snap.PausedByEngine("Research") {
		t.Fatal("expected the job to be paused before the unpause simulation")
	}

	// User removes fabrik:paused / fabrik:awaiting-input.
	unpausedItem := gh.ProjectItem{Number: 211, Title: "Unpause-then-complete test", Status: "Research", ItemID: "PVTI_211", Labels: []string{}}

	// Call 3 (post-unpause): one more turn-capped slice, well under the freshly
	// reset budget — must NOT immediately re-pause.
	if err := eng.processItem(context.Background(), board, unpausedItem); err != nil {
		t.Fatalf("processItem (post-unpause slice): %v", err)
	}
	snap, err = eng.store.Get("owner/repo", 211)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.SliceRetries("Research"); got != 1 {
		t.Errorf("SliceRetries after one post-unpause slice = %d, want 1 (fresh budget, not immediately re-pausing)", got)
	}
	if snap.PausedByEngine("Research") {
		t.Error("must not immediately re-pause on the first slice after a fresh unpause")
	}

	// Call 4: the job completes. Record the label-call count beforehand so the
	// "did this specific call re-pause" check isn't confused by the earlier,
	// expected pause from call 2.
	labelCallsBeforeCompletion := len(client.addLabelCalls)
	if err := eng.processItem(context.Background(), board, unpausedItem); err != nil {
		t.Fatalf("processItem (completion): %v", err)
	}

	snap, err = eng.store.Get("owner/repo", 211)
	if err != nil {
		t.Fatalf("store.Get after completion: %v", err)
	}
	if got := snap.SliceRetries("Research"); got != 0 {
		t.Errorf("SliceRetries after normal completion = %d, want 0 (StageRetryCleared resets both counters)", got)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts after normal completion = %d, want 0", got)
	}

	foundComplete := false
	for _, c := range client.addLabelCalls[labelCallsBeforeCompletion:] {
		if c.labelName == "stage:Research:complete" {
			foundComplete = true
		}
		if c.labelName == "fabrik:paused" {
			t.Error("must not re-pause on the completing call")
		}
	}
	if !foundComplete {
		t.Error("expected stage:Research:complete to be applied once the job finishes")
	}
}

// TestMaxRetriesEscalation_UnaffectedBySliceCounterChanges verifies acceptance
// item 4 from the #1199 review: genuine-failure escalation and its unpause
// recovery are unchanged by pauseForSliceLimit now applying itemstate.EnginePaused
// — the two pause paths (escalateFailedStage vs. pauseForSliceLimit) are
// independent, and a genuine failure still applies stage:<name>:failed, still
// pauses at MaxRetries, and unpausing still resets Attempts exactly as before.
func TestMaxRetriesEscalation_UnaffectedBySliceCounterChanges(t *testing.T) {
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
	item := gh.ProjectItem{Number: 212, Title: "Genuine failure escalation test", Status: "Research", ItemID: "PVTI_212"}

	for i := 0; i < 2; i++ {
		if err := eng.processItem(context.Background(), board, item); err != nil {
			t.Fatalf("processItem (call %d): %v", i+1, err)
		}
	}

	snap, err := eng.store.Get("owner/repo", 212)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != eng.cfg.MaxRetries {
		t.Fatalf("Attempts after escalation = %d, want %d", got, eng.cfg.MaxRetries)
	}
	if got := snap.SliceRetries("Research"); got != 0 {
		t.Errorf("SliceRetries after a genuine-failure escalation = %d, want 0 — unrelated to the slice counter", got)
	}

	foundFailed := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Research:failed" {
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Error("expected stage:Research:failed after MaxRetries exhausted — unchanged by the #1199 slice-counter work")
	}

	// User removes fabrik:paused. Same as the slice-limit case: clearFailedStage's
	// reset happens, and — since nothing else stops it — processItem immediately
	// resumes dispatch within this same call, so the post-unpause snapshot is
	// "reset to 0, then one fresh attempt" (1), not a bare 0. What matters is
	// Attempts did NOT stay pinned at MaxRetries (which would mean the reset never
	// took effect and this call would have immediately re-escalated instead).
	labelCallsBeforeUnpause := len(client.addLabelCalls)
	unpausedItem := gh.ProjectItem{Number: 212, Title: "Genuine failure escalation test", Status: "Research", ItemID: "PVTI_212", Labels: []string{}}
	if err := eng.processItem(context.Background(), board, unpausedItem); err != nil {
		t.Fatalf("processItem (unpause): %v", err)
	}

	snap, err = eng.store.Get("owner/repo", 212)
	if err != nil {
		t.Fatalf("store.Get after unpause: %v", err)
	}
	if got := snap.Attempts("Research"); got != 1 {
		t.Errorf("Attempts after unpause = %d, want 1 (reset to 0 by clearFailedStage, then one fresh attempt from the resumed dispatch — unchanged unpause behavior)", got)
	}

	foundRemoval := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "stage:Research:failed" {
			foundRemoval = true
		}
	}
	if !foundRemoval {
		t.Error("expected stage:Research:failed to be removed on unpause, exactly as before this issue")
	}
	for _, c := range client.addLabelCalls[labelCallsBeforeUnpause:] {
		if c.labelName == "stage:Research:failed" || c.labelName == "fabrik:paused" {
			t.Errorf("must not immediately re-escalate on the resumed dispatch (Attempts=1 is well under MaxRetries=%d), got label %q", eng.cfg.MaxRetries, c.labelName)
		}
	}
}
