package sim

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Sim twin for #2346. The engine's comment-review issue-body edit must reach the
// Store in the same step that advances the freshness baseline, or on a poll-only
// daemon (the sim engine has no webhook manager, so no issues.edited echo ever
// arrives) the next invocation's .fabrik-context/issue.md carries the pre-edit
// spec. BoardCache: true wires the real cache so the cache-hit path runs; the
// bare adapter reads live and cannot see this bug.

const (
	bodyWTOriginal = "## Problem\noriginal rough idea"
	bodyWTRewrite  = "## Problem\nanswer folded into the spec"
)

// runBodyWriteThroughScenario drives Specify (blocked, which warms the cache with
// a deep fetch), a human answer whose comment-review pass rewrites the body and
// completes the stage, then Research, and returns the issue.md Research saw.
func runBodyWriteThroughScenario(t *testing.T, writeThroughDisabled bool) string {
	t.Helper()
	env := NewEnv(t, EnvOptions{BoardCache: true, Stages: []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Research", Order: 2},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}})
	// The cache path admits items through the reactive mayNeedWork observer.
	t.Cleanup(env.Engine.RegisterObservers())
	env.Engine.SetIssueBodyWriteThroughDisabledForTest(writeThroughDisabled)
	// #2350: the feedback gate and the dependency decision point now apply their
	// live item reads to the Store, which heals the stale body this scenario needs
	// to observe; the #2346 defect is only visible with those applies off.
	env.Engine.SetLiveReadApplyDisabledForTest(writeThroughDisabled)

	var mu sync.Mutex
	researchSaw := ""
	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, dir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		return "FABRIK_BLOCKED_ON_INPUT\nFABRIK_SUMMARY_BEGIN\nwhat is the answer?\nFABRIK_SUMMARY_END\n", false, engine.TokenUsage{}, nil
	})
	env.Claude.ForStageComments("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, dir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		return "FABRIK_ISSUE_UPDATE_BEGIN\n" + bodyWTRewrite + "\nFABRIK_ISSUE_UPDATE_END\nFABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{}, nil
	})
	env.Claude.ForStage("Research", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, dir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		data, err := os.ReadFile(filepath.Join(dir, ".fabrik-context", "issue.md"))
		if err != nil {
			t.Errorf("Research: issue.md: %v", err)
		}
		mu.Lock()
		researchSaw = string(data)
		mu.Unlock()
		return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{}, nil
	})

	num := FileIssue(t, env, "Fold the answer in", bodyWTOriginal, "Specify")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-input", 80)

	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "the answer")
	WaitForIssueLabel(t, env, num, "stage:Research:complete", 120)

	mu.Lock()
	defer mu.Unlock()
	return researchSaw
}

func TestIssueBodyWriteThrough_CommentReviewUpdateReachesNextStage(t *testing.T) {
	t.Parallel() // R8: see poll.go's workerYield doc comment
	got := runBodyWriteThroughScenario(t, false)
	if !strings.Contains(got, "answer folded into the spec") {
		t.Errorf("Research's issue.md = %q; want the comment-review rewrite on a poll-only engine", got)
	}
}

// Non-vacuity: without the Store write-through the next stage reads the old spec,
// proving the scenario above exercises the cache-hit path rather than a live read.
func TestIssueBodyWriteThrough_NonVacuity_WithoutWriteThroughReadsStaleBody(t *testing.T) {
	t.Parallel() // R8: see poll.go's workerYield doc comment
	got := runBodyWriteThroughScenario(t, true)
	if !strings.Contains(got, "original rough idea") {
		t.Errorf("Research's issue.md = %q; with the write-through disabled the stale original body is expected (#2346)", got)
	}
}

// runStageOutputBodyScenario is the stage-output twin (R2): Specify's own run
// rewrites the body and completes, and Research must read the rewrite.
func runStageOutputBodyScenario(t *testing.T, writeThroughDisabled bool) string {
	t.Helper()
	env := NewEnv(t, EnvOptions{BoardCache: true, Stages: []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Research", Order: 2},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}})
	t.Cleanup(env.Engine.RegisterObservers())
	env.Engine.SetIssueBodyWriteThroughDisabledForTest(writeThroughDisabled)
	// #2350: the feedback gate and the dependency decision point now apply their
	// live item reads to the Store, which heals the stale body this scenario needs
	// to observe; the #2346 defect is only visible with those applies off.
	env.Engine.SetLiveReadApplyDisabledForTest(writeThroughDisabled)

	var mu sync.Mutex
	researchSaw := ""
	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, dir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		return "FABRIK_ISSUE_UPDATE_BEGIN\n" + bodyWTRewrite + "\nFABRIK_ISSUE_UPDATE_END\nFABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{}, nil
	})
	env.Claude.ForStage("Research", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, dir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		data, err := os.ReadFile(filepath.Join(dir, ".fabrik-context", "issue.md"))
		if err != nil {
			t.Errorf("Research: issue.md: %v", err)
		}
		mu.Lock()
		researchSaw = string(data)
		mu.Unlock()
		return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{}, nil
	})

	num := FileIssue(t, env, "Stage rewrites the body", bodyWTOriginal, "Specify")
	WaitForIssueLabel(t, env, num, "stage:Research:complete", 120)

	mu.Lock()
	defer mu.Unlock()
	return researchSaw
}

func TestIssueBodyWriteThrough_StageOutputUpdateReachesNextStage(t *testing.T) {
	t.Parallel() // R8: see poll.go's workerYield doc comment
	got := runStageOutputBodyScenario(t, false)
	if !strings.Contains(got, "answer folded into the spec") {
		t.Errorf("Research's issue.md = %q; want the stage-output rewrite on a poll-only engine", got)
	}
}

// Non-vacuity for the stage-output path. Later engine writes in the same run
// (stage comment, labels) advance the baseline past the body edit, so this path
// is masked on a poll-only engine exactly like comment review — it does not
// self-heal — and only the shared write-through keeps it correct.
func TestIssueBodyWriteThrough_NonVacuity_StageOutputWithoutWriteThroughReadsStaleBody(t *testing.T) {
	t.Parallel() // R8: see poll.go's workerYield doc comment
	got := runStageOutputBodyScenario(t, true)
	if !strings.Contains(got, "original rough idea") {
		t.Errorf("Research's issue.md = %q; with the write-through disabled the stale original body is expected (#2346)", got)
	}
}
