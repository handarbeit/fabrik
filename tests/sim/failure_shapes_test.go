package sim

import (
	"context"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// retryCooldownPolls bounds a WaitFor call that needs a "did-not-run" exit's
// retry to actually fire. processItem's own retry-cooldown check
// (engine/item.go: "if time.Since(lastAttempt) < cooldown") is deliberately
// outside the R3 clock seam's scope — LastAttemptAt is stamped with real
// time.Now() by handleUsageLimitExit/handleAPIErrorExit and by the ordinary
// turn-limited path alike, and compared with real time.Since, not e.now().
// So this cooldown (cfg.PollSeconds*10 = 10s at this package's default
// PollSeconds: 1) elapses in real wall-clock time regardless of the
// injected Clock, and a WaitFor call spanning it needs
// retryCooldownPolls*workerYield (poll.go) to comfortably exceed 10s.
const retryCooldownPolls = 150

// failureShapeStages is a minimal single-stage pipeline — these scenarios
// only care about one stage's response to one scripted invocation, not a
// full pipeline traversal (that's smoke_test.go's job).
func failureShapeStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
}

// TestFailureShape_TurnLimitExhausted is AC5's turn-limit-exhaustion case:
// the engine's distinct response is "incomplete but resumable" — no
// stage:Specify:failed, no fabrik:paused, no fabrik:claude-limit — and the
// stage retries and eventually completes. Mirrors
// engine.TestTurnCapPreemption_IncrementsSliceRetriesOnly's assertions,
// exercised here through real poll cycles against the sim instead of a
// direct processItem call.
func TestFailureShape_TurnLimitExhausted(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
	env.Claude.ForStage("Specify", simclaude.TurnLimitExhausted(50), simclaude.DefaultScript)

	num := FileIssue(t, env, "Turn-limit exhaustion", "body", "Specify")

	// First invocation exhausts its turn budget — must not be treated as a
	// genuine failure.
	AdvanceUntil(t, env, func(env *Env) bool {
		return env.Claude.StageCallCount("Specify") >= 1
	}, 40)
	WaitForLabelAbsent(t, env, num, "stage:Specify:failed", 5)
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "stage:Specify:failed") {
		t.Error("a turn-limit exit must not apply stage:Specify:failed")
	}
	if hasLabel(labels, "fabrik:paused") {
		t.Error("a turn-limit exit must not apply fabrik:paused on a single slice")
	}
	if hasLabel(labels, "fabrik:claude-limit") {
		t.Error("a turn-limit exit is not an account usage-limit exit — must not apply fabrik:claude-limit")
	}

	// Second invocation (DefaultScript) completes normally — proves the
	// engine kept the stage dispatchable after the turn-cap preemption
	// rather than wedging it.
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", retryCooldownPolls)
}

// TestFailureShape_UsageLimitExit is AC5's usage-limit-exit case: the stage
// never ran, so StageAttempted is recorded but never StageRetryIncremented —
// observable here as fabrik:claude-limit being applied without
// stage:Specify:failed or fabrik:paused, per CLAUDE.md's documented
// contract for this label.
func TestFailureShape_UsageLimitExit(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
	env.Claude.ForStage("Specify", simclaude.UsageLimitExit(), simclaude.DefaultScript)

	num := FileIssue(t, env, "Usage-limit exit", "body", "Specify")

	WaitForIssueLabel(t, env, num, "fabrik:claude-limit", 40)
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "stage:Specify:failed") {
		t.Error("a usage-limit exit must not apply stage:Specify:failed — the stage never ran")
	}
	if hasLabel(labels, "fabrik:paused") {
		t.Error("a usage-limit exit must not apply fabrik:paused")
	}

	// A genuine usage-limit exit also activates an account-wide Claude
	// dispatch suspension (ADR-1120) anchored on real time.Now(), not the
	// injected Clock (claudeUsageLimitFallbackBackoff is 1 real hour) — not
	// something this harness can fast-forward. fabrik:clear-claude-limit is
	// the documented operator escape hatch (CLAUDE.md) for exactly this
	// situation; applying it directly exercises settleClaudeLimitClearRequests
	// rather than waiting out an hour of real wall-clock time.
	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, "fabrik:clear-claude-limit"); err != nil {
		t.Fatalf("AddLabelToIssue(fabrik:clear-claude-limit): %v", err)
	}

	// The label clears once a later invocation is not itself a usage-limit
	// exit (per CLAUDE.md's fabrik:claude-limit entry).
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", retryCooldownPolls)
	WaitForLabelAbsent(t, env, num, "fabrik:claude-limit", 10)
}

// TestFailureShape_APIErrorExit is AC5's transient-api_error case: a
// deliberately more minimal response than UsageLimitExit's — log only, no
// label, no comment (#1458 R4). In particular fabrik:claude-limit must NOT
// be applied — a common mistake would be routing this through the same
// handler as UsageLimitExit, since both are "did-not-run" exits.
func TestFailureShape_APIErrorExit(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
	env.Claude.ForStage("Specify", simclaude.APIErrorExit(), simclaude.DefaultScript)

	num := FileIssue(t, env, "API error exit", "body", "Specify")

	AdvanceUntil(t, env, func(env *Env) bool {
		return env.Claude.StageCallCount("Specify") >= 1
	}, 40)
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "fabrik:claude-limit") {
		t.Error("an api_error exit is not an account usage-limit exit — must not apply fabrik:claude-limit")
	}
	if hasLabel(labels, "stage:Specify:failed") {
		t.Error("an api_error exit must not apply stage:Specify:failed — the stage never ran")
	}
	if hasLabel(labels, "fabrik:paused") {
		t.Error("an api_error exit must not apply fabrik:paused")
	}

	WaitForIssueLabel(t, env, num, "stage:Specify:complete", retryCooldownPolls)
}

// TestFailureShape_PartialOutputThenKilled is AC5's non-zero-exit-with-
// partial-output case: the engine treats a completion marker present in
// output as genuine completion despite the trailing process error
// (engine/claude.go's stageCompleteRE branch) — the stage completes on the
// very first invocation, with no retry needed.
func TestFailureShape_PartialOutputThenKilled(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
	env.Claude.ForStage("Specify", simclaude.PartialOutputThenKilled())

	num := FileIssue(t, env, "Partial output then killed", "body", "Specify")

	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 40)
	if got := env.Claude.StageCallCount("Specify"); got != 1 {
		t.Errorf("StageCallCount(Specify) = %d, want 1 — the marker-bearing output should complete the stage on the first attempt, no retry needed", got)
	}
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "stage:Specify:failed") {
		t.Error("output containing FABRIK_STAGE_COMPLETE must be treated as genuine completion, not a failure")
	}
}

// TestFailureShape_CommentReviewInvocation is AC5's "comment-review
// invocation" case: a human comment on an issue paused via
// FABRIK_BLOCKED_ON_INPUT triggers InvokeForComments, and the scripted
// CommentReviewCompleted response (a real commit + completion marker)
// resolves the pause and completes the stage — proving the
// InvokeForComments path is wired end to end, not just unit-tested in
// isolation (invoker_test.go).
func TestFailureShape_CommentReviewInvocation(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})

	// First (and only, until unblocked) Invoke call emits FABRIK_BLOCKED_ON_INPUT
	// — mirrors a real stage asking a clarifying question before it can proceed.
	blockedScript := func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		return "FABRIK_BLOCKED_ON_INPUT\n", false, engine.TokenUsage{}, nil
	}
	env.Claude.ForStage("Specify", blockedScript)
	env.Claude.ForStageComments("Specify", simclaude.CommentReviewCompleted())

	num := FileIssue(t, env, "Comment review invocation", "body", "Specify")

	WaitForIssueLabel(t, env, num, "fabrik:awaiting-input", 40)

	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "Here's the answer to your question.")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}

	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-input", 40)

	// CommentReviewCompleted's real commit should have unblocked and
	// completed the stage. Checked before CommentCallCount below: the label
	// removal above (unblockAwaitingInput) and the InvokeForComments call it
	// leads to (processComments) both happen inside the same dispatched
	// worker goroutine, so WaitForLabelAbsent returning is not itself proof
	// the call has completed yet — waiting for the stage's own completion
	// label is.
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 40)

	if got := env.Claude.CommentCallCount("Specify"); got != 1 {
		t.Errorf("CommentCallCount(Specify) = %d, want 1 — InvokeForComments should have been invoked exactly once to process the human's reply", got)
	}
}
