package sim

import (
	"context"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// AC10 (non-vacuity): every new test in this package must be demonstrated to
// fail under a deliberate regression in the behavior it covers. Most of the
// tests in this package are non-vacuous by construction — they pin an exact
// label set, an exact errors.As type match, or an exact (output, completed,
// usage, err) contract, any of which a generic "it completed" check would
// not catch (see e.g. TestPartialOutputThenKilled_MatchesProductionContract
// deliberately asserting the error is NOT a claudeerr type, or
// TestFailureShape_APIErrorExit asserting fabrik:claude-limit is absent,
// which only a wrong-handler regression would set). Where a config knob
// exists to prove this the way engine/ci_settle_test.go's
// "unmodified-engine-equivalent pauses (non-vacuous)" sub-tests do (flip a
// setting, show the old/broken behavior fires), that pattern is used inline
// at the test in question (see tests/sim/simclaude/invoker_test.go's
// TestPartialOutputThenKilled_MatchesProductionContract and siblings, which
// assert both the positive shape AND the specific negative it must not be).
//
// This file holds the one check AC10 calls out explicitly as the layer's
// signature false-pass and that has no config knob to flip: a scripted
// invoker that returns FABRIK_STAGE_COMPLETE without touching the worktree
// at all. There is no production setting to toggle for this — the "broken"
// behavior can only be demonstrated by actually registering a script shaped
// that way and showing the diff-emptiness check TestSmoke_FullPipelineToDone
// relies on (AC3) would have caught it.

// TestNonVacuous_MarkerOnlyScript_ProducesEmptyDiff is AC10's mandated check:
// "criterion 3 must fail if the scripted invoker is changed to emit markers
// without committing." It registers exactly that regression — a script
// returning FABRIK_STAGE_COMPLETE with zero git operations — in place of
// DefaultScript, drives it through a real stage dispatch and draft-PR
// creation, and confirms the resulting diff is empty.
//
// This is the proof that TestSmoke_FullPipelineToDone's AC3 assertion
// (tests/sim/smoke_test.go: "diff is empty — the scripted invoker's commits
// are not real") is not vacuous: had simclaude.DefaultScript itself
// regressed to this shape, that exact assertion — unchanged, no test edit
// needed — would fail exactly as this one now deliberately does.
func TestNonVacuous_MarkerOnlyScript_ProducesEmptyDiff(t *testing.T) {
	t.Parallel()
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, CreateDraftPR: true},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
	env := NewEnv(t, EnvOptions{Stages: stgs})

	// The regression under test: markers, no commit, no file write — the
	// exact false-pass shape AC10 names. Contrast with
	// simclaude.DefaultScript (invoker.go), which does the real work this
	// script skips.
	markerOnlyNoCommit := func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{TurnsUsed: 1, CostUSD: 0.01}, nil
	}
	env.Claude.ForStage("Implement", markerOnlyNoCommit)

	baseSHA, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, "main")
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	num := FileIssue(t, env, "Non-vacuity probe", "body", "Implement")
	WaitForIssueLabel(t, env, num, "stage:Implement:complete", 80)

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatal("expected a linked PR — the marker-only script still signals completion, so the engine still creates one; the point under test is the diff, not PR creation")
	}

	simBareDir, err := env.Sim.Sim().RepoBareDir(env.OwnerRepo)
	if err != nil {
		t.Fatalf("RepoBareDir: %v", err)
	}
	diff := gitDiffNames(t, simBareDir, baseSHA, pr.HeadSHA)
	if strings.TrimSpace(diff) != "" {
		t.Fatalf("expected an EMPTY diff from a script that never committed — got %q. If this fails, AC3's diff-emptiness check has stopped being able to distinguish a real commit from marker-only output, which means TestSmoke_FullPipelineToDone's AC3 assertion is no longer trustworthy either", diff)
	}

	t.Log("confirmed: a scripted invoker that emits FABRIK_STAGE_COMPLETE without committing produces an empty diff and the engine still reports the stage complete — proving TestSmoke_FullPipelineToDone's AC3 diff assertion is load-bearing, not vacuous: it would fail exactly this way if simclaude.DefaultScript ever regressed to this shape")
}
