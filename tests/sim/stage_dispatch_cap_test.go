package sim

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
	"github.com/handarbeit/fabrik/tests/sim/simgh/ghfault"
)

// Sim twins for #2395 / ADR 2395 (mentioned for context: #2388): the per-episode
// stage dispatch bound and the unchanged-input resume exemption.
//
// The loop is reproduced exactly: a stage whose run completes, but whose
// stage:<X>:complete label write is refused, leaves nothing durable behind, so the
// next poll dispatches it again. max_retries never counts it (every run succeeded).
//
// LastAttemptAt is wall-clock while admission cooldowns are on the injected
// Clock, so these scenarios set a small real RetryBackoff and sleep it out
// between polls (as incomplete_retry_admission_test.go does).

const dispatchCapBackoff = 15 * time.Millisecond

func dispatchCapStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Research", Order: 1},
		{Name: "Plan", Order: 2},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

func dispatchCapEnv(t *testing.T, limit int) (*Env, string) {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages: dispatchCapStages(),
		Yolo:   boolPtr(false),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.RetryBackoff = dispatchCapBackoff
			cfg.MaxStageDispatches = limit
		},
	})
	sessRoot := t.TempDir()
	env.Engine.SetSessionDirForTest(sessRoot)
	return env, sessRoot
}

// refuseCompleteLabel makes the Research completion label write fail for ever.
func refuseCompleteLabel(env *Env) {
	env.Sim.Faults().FailWhen("AddLabelToIssue", func(a simgh.Args) bool {
		return a.Label == "stage:Research:complete"
	}, simgh.Always, ghfault.ServerError())
}

// pollUntil drives polls (sleeping out the retry backoff between them) until cond
// holds or maxPolls is spent; it does not fail on exhaustion, so a test can
// assert a bound was *not* exceeded.
func pollUntil(t *testing.T, env *Env, cond func() bool, maxPolls int) bool {
	t.Helper()
	for i := 0; i < maxPolls; i++ {
		if cond() {
			return true
		}
		time.Sleep(2 * dispatchCapBackoff)
		RunPoll(t, env)
	}
	return cond()
}

func isPaused(t *testing.T, env *Env, num int) bool {
	t.Helper()
	return hasLabel(IssueLabels(t, env, num), "fabrik:paused")
}

// R2/R5: a stage that completes but is never recorded stops at the cap and
// pauses, with one comment naming the stage, the count and the outcomes.
func TestStageDispatchCap_UnrecordedCompletionPausesAtCap(t *testing.T) {
	t.Parallel()
	const limit = 3
	env, _ := dispatchCapEnv(t, limit)
	refuseCompleteLabel(env)
	num := FileIssue(t, env, "unrecorded completion", "body", "Research")

	if !pollUntil(t, env, func() bool { return isPaused(t, env, num) }, 60) {
		t.Fatalf("never paused; stage ran %d time(s)\n\n%s", env.Claude.StageCallCount("Research"), diagnostics(env))
	}
	if env.Sim.Faults().FiredCount("AddLabelToIssue") == 0 {
		t.Fatal("the completion-label fault never fired; the scenario is vacuous")
	}
	if got := env.Claude.StageCallCount("Research"); got != limit {
		t.Errorf("stage ran %d time(s), want exactly %d", got, limit)
	}
	labels := IssueLabels(t, env, num)
	if !hasLabel(labels, "fabrik:awaiting-input") || hasLabel(labels, "stage:Research:failed") {
		t.Errorf("labels %v: want paused + awaiting-input and no failed label", labels)
	}

	// Further polls neither dispatch nor comment again.
	RunPolls(t, env, 6)
	if got := env.Claude.StageCallCount("Research"); got != limit {
		t.Errorf("stage ran %d time(s) after the pause, want %d", got, limit)
	}
	if got := countComments(t, env, num, "stage dispatch limit reached"); got != 1 {
		t.Fatalf("pause comments = %d, want 1", got)
	}
	var body string
	for _, c := range commentsOn(t, env, num) {
		if strings.Contains(c.Body, "stage dispatch limit reached") {
			body = c.Body
		}
	}
	for _, want := range []string{"Research", "3 time(s)", "limit of 3", "complete (completion not recorded)"} {
		if !strings.Contains(body, want) {
			t.Errorf("pause comment missing %q:\n%s", want, body)
		}
	}
}

// Non-vacuity: with the cap neutralised the same stage runs on past it.
func TestStageDispatchCap_NeutralisedRunsPastCap(t *testing.T) {
	t.Parallel()
	const limit = 3
	env, _ := dispatchCapEnv(t, limit)
	env.Engine.SetStageDispatchCapDisabledForTest(true)
	refuseCompleteLabel(env)
	num := FileIssue(t, env, "unrecorded completion", "body", "Research")

	if !pollUntil(t, env, func() bool { return env.Claude.StageCallCount("Research") > limit+2 }, 80) {
		t.Fatalf("stage ran only %d time(s) with the cap neutralised\n\n%s", env.Claude.StageCallCount("Research"), diagnostics(env))
	}
	if isPaused(t, env, num) {
		t.Error("a neutralised cap must not pause")
	}
}

// 0 disables the bound.
func TestStageDispatchCap_ZeroDisables(t *testing.T) {
	t.Parallel()
	env, _ := dispatchCapEnv(t, 0)
	refuseCompleteLabel(env)
	num := FileIssue(t, env, "unrecorded completion", "body", "Research")

	if !pollUntil(t, env, func() bool { return env.Claude.StageCallCount("Research") > 6 }, 80) {
		t.Fatalf("stage ran only %d time(s) with the bound disabled", env.Claude.StageCallCount("Research"))
	}
	if isPaused(t, env, num) {
		t.Error("cap 0 must never pause")
	}
}

// A human comment on the paused item resumes it and starts the count again: the
// stage is dispatched another full allowance, then pauses a second time.
func TestStageDispatchCap_HumanCommentStartsNewEpisode(t *testing.T) {
	t.Parallel()
	const limit = 2
	env, _ := dispatchCapEnv(t, limit)
	refuseCompleteLabel(env)
	num := FileIssue(t, env, "resume after cap", "body", "Research")

	if !pollUntil(t, env, func() bool { return isPaused(t, env, num) }, 60) {
		t.Fatalf("never paused\n\n%s", diagnostics(env))
	}
	if got := env.Claude.StageCallCount("Research"); got != limit {
		t.Fatalf("stage ran %d time(s) before the pause, want %d", got, limit)
	}

	env.Clock.Advance(time.Minute)
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "fixed the permissions, please continue")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	if !pollUntil(t, env, func() bool { return countComments(t, env, num, "stage dispatch limit reached") >= 2 }, 80) {
		t.Fatalf("no second pause after the resume; stage ran %d time(s)\n\n%s", env.Claude.StageCallCount("Research"), diagnostics(env))
	}
	// The resume gave a fresh allowance of `limit` dispatches, not an immediate re-pause.
	if got := env.Claude.StageCallCount("Research"); got < 2*limit {
		t.Errorf("stage ran %d time(s) in total, want at least %d (a full new episode)", got, 2*limit)
	}
}

// R3: the count survives a restart. The total dispatches before the pause still
// equal the cap, whatever the restart did to the engine's memory.
func TestStageDispatchCap_CountSurvivesRestart(t *testing.T) {
	t.Parallel()
	const limit = 4
	env, sessRoot := dispatchCapEnv(t, limit)
	refuseCompleteLabel(env)
	num := FileIssue(t, env, "restart mid-episode", "body", "Research")

	if !pollUntil(t, env, func() bool { return env.Claude.StageCallCount("Research") >= 2 }, 60) {
		t.Fatalf("stage ran %d time(s), want 2 before the restart", env.Claude.StageCallCount("Research"))
	}
	if env.Claude.StageCallCount("Research") != 2 {
		t.Fatalf("stage ran %d time(s) before the restart, want exactly 2", env.Claude.StageCallCount("Research"))
	}

	env = RestartEnv(t, env)
	// Only the engine's memory is gone; the dispatch record is on disk.
	env.Engine.SetSessionDirForTest(sessRoot)
	refuseCompleteLabel(env)

	if !pollUntil(t, env, func() bool { return isPaused(t, env, num) }, 60) {
		t.Fatalf("never paused after the restart; stage ran %d time(s)\n\n%s", env.Claude.StageCallCount("Research"), diagnostics(env))
	}
	if got := env.Claude.StageCallCount("Research"); got != limit {
		t.Errorf("stage ran %d time(s) in total across the restart, want %d", got, limit)
	}
}

// Non-vacuity for R3: without the persisted record a restart would hand the stage
// a fresh allowance. Deleting the record between the halves shows the test above
// is sensitive to persistence.
func TestStageDispatchCap_RestartWithoutRecordGetsFreshAllowance(t *testing.T) {
	t.Parallel()
	const limit = 4
	env, _ := dispatchCapEnv(t, limit)
	refuseCompleteLabel(env)
	num := FileIssue(t, env, "restart loses record", "body", "Research")

	if !pollUntil(t, env, func() bool { return env.Claude.StageCallCount("Research") >= 2 }, 60) {
		t.Fatalf("stage ran %d time(s), want 2 before the restart", env.Claude.StageCallCount("Research"))
	}
	env = RestartEnv(t, env)
	env.Engine.SetSessionDirForTest(t.TempDir()) // a different, empty directory: the record is gone
	refuseCompleteLabel(env)

	if !pollUntil(t, env, func() bool { return isPaused(t, env, num) }, 80) {
		t.Fatalf("never paused\n\n%s", diagnostics(env))
	}
	if got := env.Claude.StageCallCount("Research"); got <= limit {
		t.Errorf("stage ran %d time(s); a lost record should have allowed more than %d", got, limit)
	}
}

// durable completion ends the episode: a stage that records its completion is
// never paused, however often the item is later re-entered.
func TestStageDispatchCap_RecordedCompletionIsNotBounded(t *testing.T) {
	t.Parallel()
	env, _ := dispatchCapEnv(t, 1)
	num := FileIssue(t, env, "healthy", "body", "Research")
	WaitForIssueLabel(t, env, num, "stage:Research:complete", 40)
	if isPaused(t, env, num) {
		t.Error("a stage that completes and records must not be paused by a cap of 1")
	}
	if got := env.Claude.StageCallCount("Research"); got != 1 {
		t.Errorf("stage ran %d time(s), want 1", got)
	}
}

// R4: a re-dispatch with unchanged inputs (same head, body and comments) does not
// resume a saved session whose measured context is over the threshold — also when
// the session is otherwise a continuation. Neutralising the exemption resumes it.
func runUnchangedRedispatch(t *testing.T, disableExemption bool, sessionTokens int) (secondResume bool) {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages: []*stages.Stage{
			{Name: "Research", Order: 1, ResumeThresholdTokens: 1000},
			{Name: "Done", Order: 2, CleanupWorktree: true},
		},
		Yolo:         boolPtr(false),
		ConfigureCfg: func(cfg *engine.Config) { cfg.RetryBackoff = dispatchCapBackoff },
	})
	env.Engine.SetSessionDirForTest(t.TempDir())
	env.Engine.SetUnchangedInputExemptionDisabledForTest(disableExemption)
	FileIssue(t, env, "unchanged re-dispatch", "body", "Research")

	var mu sync.Mutex
	var resumes []bool
	env.Claude.ForStage("Research", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		mu.Lock()
		resumes = append(resumes, resume)
		n := len(resumes)
		mu.Unlock()
		if n == 1 {
			// What the real invoker leaves behind: a saved session of this size, and a
			// run that ended without the completion marker (nothing committed).
			env.Engine.RecordSessionForTest(issue, stage.Name, "S1", sessionTokens, false)
			return "still working\n", false, engine.TokenUsage{TurnsUsed: 3, MaxTurns: 50}, nil
		}
		return simclaude.DefaultScript(ctx, stage, issue, newComments, resume, workDir, opts)
	})
	pollUntil(t, env, func() bool { return env.Claude.StageCallCount("Research") >= 2 }, 60)

	mu.Lock()
	defer mu.Unlock()
	if len(resumes) < 2 {
		t.Fatalf("resume flags = %v, want two stage calls\n\n%s", resumes, diagnostics(env))
	}
	return resumes[1]
}

func TestUnchangedRedispatch_OversizedSessionStartsFresh(t *testing.T) {
	t.Parallel()
	if runUnchangedRedispatch(t, false, 5000) {
		t.Error("an unchanged re-dispatch of an oversized session must start fresh")
	}
}

func TestUnchangedRedispatch_SmallSessionResumes(t *testing.T) {
	t.Parallel()
	if !runUnchangedRedispatch(t, false, 500) {
		t.Error("a session within the threshold must resume")
	}
}

func TestUnchangedRedispatch_ExemptionNeutralisedResumes(t *testing.T) {
	t.Parallel()
	if !runUnchangedRedispatch(t, true, 5000) {
		t.Error("with the exemption neutralised the continuation must resume (non-vacuity)")
	}
}
