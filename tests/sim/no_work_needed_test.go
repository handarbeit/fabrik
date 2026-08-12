package sim

import (
	"context"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// noWorkNeededSkipCommentPrefix is the literal prefix shared by both variants
// of the engine's own no-work-needed skip comment (engine/no_work_needed_settle.go:
// noWorkNeededSkipComment — "🏭 **Fabrik — skipped: no work needed**"). Copied
// here rather than imported (the engine function is unexported) — mirrors
// tests/e2e/no_work_needed_marker_test.go's own copy of the same literal.
//
// Matching on HasPrefix rather than Contains ensures
// hasNoWorkNeededSkipComment can only be satisfied by a genuine
// engine-authored comment, not by a stage-output comment where an agent
// quotes the marker text in prose while reasoning about the feature — the
// same body-prefix convention findNewComments (engine/comments.go) uses to
// distinguish Fabrik's own output from prose, and the same fix pattern that
// already shipped for #1320's cycle-limit marker false-positive class.
const noWorkNeededSkipCommentPrefix = "🏭 **Fabrik — skipped: no work needed**"

// hasNoWorkNeededSkipComment reports whether comments contains a genuine
// engine-authored no-work-needed skip comment, matched by exact body prefix
// — never strings.Contains, per the discriminator's whole reason for
// existing (see the const doc comment and #1355/#1320).
func hasNoWorkNeededSkipComment(comments []gh.Comment) bool {
	for _, c := range comments {
		if strings.HasPrefix(c.Body, noWorkNeededSkipCommentPrefix) {
			return true
		}
	}
	return false
}

// noWorkNeededScript is a Script that emits both FABRIK_STAGE_COMPLETE and
// FABRIK_NO_WORK_NEEDED on their own lines — the exact shape
// engine/item.go's finalizeStageOutcome requires to set noWorkNeeded true
// (`err == nil && CheckNoWorkNeeded(output)`, gated on completed also being
// true). No commit is made: the no-work-needed path never creates a PR or
// otherwise touches the worktree (settleNoWorkNeeded moves straight to
// labels/comments/Done), so there is nothing for this script to commit.
func noWorkNeededScript(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
	output := "FABRIK_STAGE_COMPLETE\nFABRIK_NO_WORK_NEEDED\n"
	return output, true, engine.TokenUsage{TurnsUsed: 2, MaxTurns: 50, CostUSD: 0.02}, nil
}

// TestNoWorkNeeded ports tests/e2e/no_work_needed_test.go's TestNoWorkNeeded
// — the FABRIK_NO_WORK_NEEDED short-circuit (#733 the marker, #742
// close-on-no-work).
//
// The live test files an issue describing work that is already done,
// expects Plan to determine nothing actually needs implementing and emit
// FABRIK_NO_WORK_NEEDED, and asserts the engine marks all downstream stages
// complete and closes the issue — without ever opening a PR. This port
// scripts Plan's invocation to emit that exact marker pair instead of
// relying on a live agent's judgment call.
//
// Per handarbeit/fabrik#1355 (R2, carried forward from
// tests/e2e/no_work_needed_marker_test.go): WaitForIssueClosed alone cannot
// distinguish the no-work-needed path from an ordinary close — a broken
// settleNoWorkNeeded that somehow still closed the issue some other way
// would pass just as easily as a correctly-working one. The assertion below
// additionally requires the engine's own no-work-needed skip comment
// (noWorkNeededSkipComment, engine/no_work_needed_settle.go), matched by
// body PREFIX via hasNoWorkNeededSkipComment above — never
// strings.Contains — closing that vacuity gap.
//
// This scenario emits at Plan (order 3 in smokeStages), leaving Implement,
// Review, and Validate in range for settleNoWorkNeeded's skip-comment loop
// (`s.Order <= stage.Order || s.Order >= doneOrder || s.Unmanaged`) — three
// skip comments, one per stage, all sharing the identical body text (the
// comment names the emitting stage, not the individual skipped stage), so
// hasSkippedComment's per-decision (not per-stage) idempotency check is
// exercised exactly as the live test's four-skip-comment run against a
// larger board was.
func TestNoWorkNeeded(t *testing.T) {
	t.Parallel() // R8: this package's dominant real-time cost — see poll.go's workerYield doc comment
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Claude.ForStage("Plan", noWorkNeededScript)

	num := FileIssue(t, env, "e2e no-work-needed (ported)",
		`## Goal

Verify that the FABRIK_NO_WORK_NEEDED short-circuit closes the issue
without creating a PR.

## What this asks for

The work described by this issue is already done; there is nothing to
implement. Plan should recognise that and emit FABRIK_NO_WORK_NEEDED on a
line of its own (instead of decomposing or proceeding to Implement).

This is a regression scenario for #733 (the marker) and #742
(close-on-no-work).`,
		"Specify")

	// Specify and Research run normally first (each gets its own maxPolls
	// budget, mirroring smoke_test.go's per-stage waits — a single combined
	// budget spanning multiple stage transitions is not enough headroom).
	// Plan's own dispatch then short-circuits straight to a close within the
	// same worker goroutine (handleNoWorkNeeded is called synchronously from
	// finalizeStageOutcome, engine/item.go) — no separate wait for
	// stage:Plan:complete is needed, since the no-work-needed path never
	// applies it via the normal handleStageComplete route (settleNoWorkNeeded
	// applies it directly as part of the same settle pass that closes the
	// issue).
	for _, name := range []string{"Specify", "Research"} {
		WaitForIssueLabel(t, env, num, "stage:"+name+":complete", 80)
	}
	WaitForIssueClosed(t, env, num, 80)

	// The discriminating assertion (R2): the engine's own skip comment must
	// be present, matched by prefix — not merely "the issue closed somehow".
	item := projectItem(t, env, num)
	if !hasNoWorkNeededSkipComment(item.Comments) {
		t.Fatalf("no engine-authored no-work-needed skip comment (prefix %q) found on #%d — "+
			"the issue closed, but not provably via the no-work-needed path; comments: %+v",
			noWorkNeededSkipCommentPrefix, num, item.Comments)
	}

	// fabrik:awaiting-done is the durable in-flight marker (ADR-060) — it
	// must be cleared once settleNoWorkNeeded fully lands (label/comment
	// steps, Done move, and close all succeeded).
	if hasLabel(item.Labels, "fabrik:awaiting-done") {
		t.Error("fabrik:awaiting-done should have been cleared once the no-work-needed decision fully settled")
	}

	// The board must have moved all the way to Done, and every stage
	// strictly between Plan (the emitter) and Done must have been marked
	// complete via a skip label (Implement, Review, Validate — smokeStages'
	// shape), never actually dispatched.
	if item.Status != "Done" {
		t.Errorf("board status = %q, want %q", item.Status, "Done")
	}
	for _, name := range []string{"Plan", "Implement", "Review", "Validate"} {
		if !hasLabel(item.Labels, "stage:"+name+":complete") {
			t.Errorf("expected stage:%s:complete to have been applied (skip bookkeeping), labels=%v", name, item.Labels)
		}
	}
	for _, name := range []string{"Implement", "Review", "Validate"} {
		if got := env.Claude.StageCallCount(name); got != 0 {
			t.Errorf("StageCallCount(%s) = %d, want 0 — a skipped stage must never actually be dispatched to Claude", name, got)
		}
	}

	// Spot-check: no linked PR was ever created — the no-work-needed path
	// never opens one.
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr != nil {
		t.Errorf("expected no linked PR, got #%d", pr.Number)
	}
}

// TestNoWorkNeeded_NonVacuous is AC10/R2's non-vacuity companion, covering
// two independent regressions the positive test's assertions must catch:
//
//  1. hasNoWorkNeededSkipComment's own discriminating power (mirrors
//     tests/e2e/no_work_needed_marker_test.go's
//     TestHasNoWorkNeededSkipComment): a comment that merely quotes the
//     marker prefix in prose — the #1320-class false positive — must NOT
//     satisfy it, only a genuine prefix match does.
//  2. The FABRIK_NO_WORK_NEEDED marker itself is load-bearing: a Plan
//     invocation that completes ordinarily (FABRIK_STAGE_COMPLETE only, a
//     real commit, no NO_WORK_NEEDED) must proceed through the normal
//     pipeline instead — Implement actually gets dispatched, and no skip
//     comment or fabrik:awaiting-done ever appears.
func TestNoWorkNeeded_NonVacuous(t *testing.T) {
	t.Parallel()

	t.Run("prose quoting the marker text is not mistaken for the genuine comment", func(t *testing.T) {
		prose := "🏭 **Fabrik — stage: Research**\n" +
			"*branch: fabrik/issue-1450 | commit: abc123*\n\n" +
			"## Research Findings\n\n" +
			"When Plan emits FABRIK_NO_WORK_NEEDED, the engine posts a comment " +
			"beginning \"🏭 **Fabrik — skipped: no work needed**\" to record the decision.\n"
		if hasNoWorkNeededSkipComment([]gh.Comment{{Body: prose}}) {
			t.Fatal("prose quoting the marker prefix must not satisfy hasNoWorkNeededSkipComment — if it does, TestNoWorkNeeded's discriminating assertion is not discriminating")
		}
		genuine := noWorkNeededSkipCommentPrefix + "\n\n_Skipped: no work needed (FABRIK_NO_WORK_NEEDED emitted by Plan)._"
		if !hasNoWorkNeededSkipComment([]gh.Comment{{Body: genuine}}) {
			t.Fatal("a genuine engine-authored comment must satisfy hasNoWorkNeededSkipComment")
		}
	})

	t.Run("an ordinary Plan completion does not short-circuit the pipeline", func(t *testing.T) {
		env := NewEnv(t, EnvOptions{Stages: smokeStages()})
		env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
		// No ForStage("Plan", ...) override — DefaultScript performs an
		// ordinary, real completion with no FABRIK_NO_WORK_NEEDED marker.

		num := FileIssue(t, env, "No-work-needed non-vacuity probe", "body", "Specify")

		for _, name := range []string{"Specify", "Research"} {
			WaitForIssueLabel(t, env, num, "stage:"+name+":complete", 80)
		}
		WaitForIssueLabel(t, env, num, "stage:Plan:complete", 80)
		labels := IssueLabels(t, env, num)
		if hasLabel(labels, "fabrik:awaiting-done") {
			t.Error("an ordinary Plan completion must never apply fabrik:awaiting-done")
		}
		item := projectItem(t, env, num)
		if hasNoWorkNeededSkipComment(item.Comments) {
			t.Error("an ordinary Plan completion must never produce a no-work-needed skip comment")
		}
		if item.IsClosed {
			t.Error("an ordinary Plan completion must not close the issue")
		}

		// The pipeline proceeds normally: Implement actually gets dispatched.
		WaitForIssueLabel(t, env, num, "stage:Implement:complete", 80)
		if got := env.Claude.StageCallCount("Implement"); got == 0 {
			t.Error("expected Implement to have actually been dispatched — if this fires, the positive test's per-stage StageCallCount(0) assertions are not discriminating")
		}
	})
}
