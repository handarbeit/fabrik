package sim

import (
	"context"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// TestBlockedOnInput ports tests/e2e/blocked_on_input_test.go's TestBlockedOnInput
// — the FABRIK_BLOCKED_ON_INPUT pause-and-resume flow (#733), plus the
// awaiting-input label clear on stage complete (ed46b7fc).
//
// The live test files an issue with deliberately ambiguous requirements,
// expects Specify to recognise the ambiguity and emit FABRIK_BLOCKED_ON_INPUT
// (instead of proceeding with assumptions), then asserts the engine pauses
// with fabrik:awaiting-input, that a clarifying comment resumes the stage on
// the next poll, and that the pipeline ultimately completes (issue closes).
// This port scripts that exact shape instead of relying on a live agent's
// judgment call: Specify's first invocation is scripted to emit
// FABRIK_BLOCKED_ON_INPUT with no commit (mirroring
// TestFailureShape_CommentReviewInvocation in failure_shapes_test.go, the
// existing precedent for this exact pause/resume pattern), and the
// comment-triggered resume is scripted to complete normally.
//
// Deviation from the live test (see the final task report, R2/R5): the live
// test's clarifying comment asks Claude to short-circuit via
// FABRIK_NO_WORK_NEEDED, but engine/comments.go's processComments /
// finalizeComments path (the comment-triggered resume) never inspects its
// output for FABRIK_NO_WORK_NEEDED — that marker is only checked on the
// primary stage-invocation path (engine/item.go's finalizeStageOutcome,
// `noWorkNeeded := err == nil && CheckNoWorkNeeded(output)`). A
// comment-review completion can only ever produce an ordinary
// stage:Specify:complete, which is what this port exercises; the pipeline
// then continues (as it would for a live agent that ignored the shortcut
// suggestion and simply proceeded) all the way to a closed issue in Done —
// preserving the live test's terminal assertion (WaitForIssueClosed) via the
// genuinely-reachable production path rather than one the comment-resume
// mechanism cannot exercise.
//
// This also strengthens the live assertions (R2, "assert at least as much"):
// it additionally checks that the stage does NOT get stage:Specify:complete
// while blocked (blockOnInput, engine/item.go, never adds it — only
// unblockAwaitingInput's eventual real completion does), and that
// fabrik:paused (not just fabrik:awaiting-input — see blockOnInput's
// dual-label pause) is applied and cleared in lockstep with
// fabrik:awaiting-input.
func TestBlockedOnInput(t *testing.T) {
	t.Parallel() // R8: this package's dominant real-time cost — see poll.go's workerYield doc comment
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})

	// Deterministic direct-merge fallback at Validate, exactly as
	// TestSmoke_FullPipelineToDone does — the scenario runs the full pipeline
	// under yolo to a genuine close, and native auto-merge never spontaneously
	// resolves in the sim (see simgh/FIDELITY.md).
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})

	// First (and only, until unblocked) Invoke call for Specify emits
	// FABRIK_BLOCKED_ON_INPUT with no commit — mirrors a real stage recognising
	// ambiguous requirements and asking a clarifying question before proceeding,
	// exactly like TestFailureShape_CommentReviewInvocation's blockedScript.
	blockedScript := func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		return "FABRIK_BLOCKED_ON_INPUT\n", false, engine.TokenUsage{}, nil
	}
	env.Claude.ForStage("Specify", blockedScript)
	// The comment-triggered resume completes normally (a real commit + marker),
	// letting the pipeline proceed exactly as an ordinary Specify completion
	// would — see the deviation note above.
	env.Claude.ForStageComments("Specify", simclaude.CommentReviewCompleted())

	num := FileIssue(t, env, "e2e blocked-on-input (ported)",
		`## Goal

Verify FABRIK_BLOCKED_ON_INPUT pause + resume.

## Deliberately ambiguous

Make the README "better".

That is all. Specify should immediately notice this is wildly under-specified
and emit FABRIK_BLOCKED_ON_INPUT asking for clarification (instead of
proceeding to Research with assumptions). The engine should then pause the
issue with fabrik:paused and fabrik:awaiting-input.

Regression coverage:
- #733 / FABRIK_BLOCKED_ON_INPUT marker
- ed46b7fc — fabrik:awaiting-input clears on FABRIK_STAGE_COMPLETE`,
		"Specify")

	// The pause lands: fabrik:awaiting-input AND fabrik:paused, per
	// blockOnInput's dual-label contract.
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-input", 80)
	labels := IssueLabels(t, env, num)
	if !hasLabel(labels, "fabrik:paused") {
		t.Error("blockOnInput pauses with BOTH fabrik:paused and fabrik:awaiting-input — fabrik:paused is missing")
	}
	// The blocked stage must not be marked complete — a blocked invocation is
	// mutually exclusive with FABRIK_STAGE_COMPLETE (CLAUDE.md's marker doc).
	if hasLabel(labels, "stage:Specify:complete") {
		t.Error("stage:Specify:complete must not be present while the issue is awaiting input")
	}

	// Post a clarifying comment — the resolving action a human operator takes.
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator",
		"Clarification: the README already covers what it needs to cover; please just proceed.")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}

	// On next poll, Fabrik re-invokes Specify via InvokeForComments and resumes.
	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-input", 80)
	labels = IssueLabels(t, env, num)
	if hasLabel(labels, "fabrik:paused") {
		t.Error("unblockAwaitingInput removes both fabrik:paused and fabrik:awaiting-input — fabrik:paused is still present")
	}

	// CommentReviewCompleted's real commit unblocks and completes the stage.
	// Checked via the stage's own completion label, not just the label
	// removal above — see TestFailureShape_CommentReviewInvocation's doc
	// comment for why the label-removal moment alone is not proof the
	// InvokeForComments call has actually finished.
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 80)
	if got := env.Claude.CommentCallCount("Specify"); got != 1 {
		t.Errorf("CommentCallCount(Specify) = %d, want 1 — InvokeForComments should have run exactly once to process the clarification", got)
	}

	// The pipeline continues normally from here (R2's "resume ... lets the
	// pipeline continue") all the way to a closed issue in Done — the live
	// test's terminal assertion.
	stageOrder := []string{"Research", "Plan", "Implement", "Review", "Validate"}
	for _, name := range stageOrder {
		WaitForIssueLabel(t, env, num, "stage:"+name+":complete", 80)
	}
	WaitForProjectStatus(t, env, num, "Done", 80)
	WaitForIssueClosed(t, env, num, 80)
}

// TestBlockedOnInput_NonVacuous is AC10/R2's non-vacuity companion for
// TestBlockedOnInput: it neutralizes the FABRIK_BLOCKED_ON_INPUT mechanism
// (Specify completes normally instead of blocking) and confirms
// fabrik:awaiting-input / fabrik:paused are never applied, and the stage
// reaches stage:Specify:complete directly with no pause at all. This proves
// TestBlockedOnInput's assertions ("fabrik:awaiting-input appears while
// blocked", "stage:Specify:complete is absent while blocked") are not
// vacuously true — an ordinary completion, unchanged, produces neither.
func TestBlockedOnInput_NonVacuous(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
	// No ForStage override — DefaultScript performs an ordinary, real
	// completion (commit + FABRIK_STAGE_COMPLETE, no BLOCKED_ON_INPUT marker).

	num := FileIssue(t, env, "Blocked-on-input non-vacuity probe", "body", "Specify")

	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 80)
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "fabrik:awaiting-input") {
		t.Error("an ordinary completion must never apply fabrik:awaiting-input — if this fires, the positive test's assertion is not discriminating")
	}
	if hasLabel(labels, "fabrik:paused") {
		t.Error("an ordinary completion must never apply fabrik:paused")
	}
}
