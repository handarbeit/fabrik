package sim

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeerr"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Sim twins for #2300 / ADR 2300: the session resume decision for comment
// review and stage re-entry. The sim replaces the invoker, so it never runs the
// real size recording or --resume argv (unit tests cover those); a script seeds
// the saved session through Engine.RecordSessionForTest, as the real invoker
// would leave it, and the decision is observed above the interface — as
// InvokeOptions.NoResume on a comment pass and the resume bool on a stage run.

func sessionResumeStages(mode string, threshold int) []*stages.Stage {
	return []*stages.Stage{
		{Name: "Research", Order: 1, ResumeSession: mode, ResumeThresholdTokens: threshold},
		{Name: "Plan", Order: 2},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

func sessionResumeEnv(t *testing.T, mode string, threshold int) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{Stages: sessionResumeStages(mode, threshold), Yolo: boolPtr(false)})
	env.Engine.SetSessionDirForTest(t.TempDir())
	return env
}

// commentPassObservation records what the comment pass saw.
type commentPassObservation struct {
	mu            sync.Mutex
	noResume      []bool
	ownOutputSeen []bool
}

func (o *commentPassObservation) script(env *Env) simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		_, statErr := os.Stat(filepath.Join(workDir, ".fabrik-context", "stage-"+stage.Name+".md"))
		o.mu.Lock()
		o.noResume = append(o.noResume, opts.NoResume)
		o.ownOutputSeen = append(o.ownOutputSeen, statErr == nil)
		o.mu.Unlock()
		return simclaude.DefaultCommentScript(ctx, stage, issue, comments, workDir, opts)
	}
}

// runResearchThenComment completes Research with a saved session of the given
// size, then lets a human comment trigger one comment-review pass.
func runResearchThenComment(t *testing.T, env *Env, sessionTokens int) *commentPassObservation {
	t.Helper()
	obs := &commentPassObservation{}
	num := FileIssue(t, env, "resume decision", "body", "Research")
	env.Claude.ForStage("Research", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		env.Engine.RecordSessionForTest(issue, stage.Name, "S1", sessionTokens, false)
		out, completed, usage, err := simclaude.DefaultScript(ctx, stage, issue, newComments, resume, workDir, opts)
		// Real stage output, so the engine posts a stage comment for the context file.
		return "research findings for the offline case\n" + out, completed, usage, err
	})
	env.Claude.ForStageComments("Research", obs.script(env))

	WaitForIssueLabel(t, env, num, "stage:Research:complete", 40)
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "maintainer", "what about the offline case?")
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.CommentCallCount("Research") >= 1 }, 40)
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.noResume) < 1 {
		t.Fatal("no comment pass observed")
	}
	return obs
}

// auto + a small threshold: a large saved session starts the comment review
// fresh, and the fresh session still gets the stage's own output as context (R4).
func TestSessionResume_AutoLargeSessionStartsCommentReviewFresh(t *testing.T) {
	t.Parallel()
	env := sessionResumeEnv(t, "auto", 1000)
	obs := runResearchThenComment(t, env, 5000)
	if !obs.noResume[0] {
		t.Error("comment review resumed a session above the threshold: want a fresh session (NoResume)")
	}
	if !obs.ownOutputSeen[0] {
		t.Error("stage-Research.md missing from the fresh comment review's context")
	}
}

// The default (resume): the same scenario resumes.
func TestSessionResume_DefaultResumesTheSameScenario(t *testing.T) {
	t.Parallel()
	env := sessionResumeEnv(t, "", 1000)
	if obs := runResearchThenComment(t, env, 5000); obs.noResume[0] {
		t.Error("default resume_session must resume: NoResume was set")
	}
}

// auto with a session under the threshold resumes.
func TestSessionResume_AutoSmallSessionResumes(t *testing.T) {
	t.Parallel()
	env := sessionResumeEnv(t, "auto", 100000)
	if obs := runResearchThenComment(t, env, 5000); obs.noResume[0] {
		t.Error("auto resumed-or-not: a session under the threshold must resume")
	}
}

// Non-vacuity: forcing the decision to resume makes the auto scenario above fail.
func TestSessionResume_ForcedResumeDefeatsAuto(t *testing.T) {
	t.Parallel()
	env := sessionResumeEnv(t, "auto", 1000)
	env.Engine.SetSessionDecisionForcedResumeForTest(true)
	if obs := runResearchThenComment(t, env, 5000); obs.noResume[0] {
		t.Error("forced-resume seam did not force a resume")
	}
}

// R2: a turn-capped run's continuation resumes whatever resume_session says,
// also after a restart — the signal is durable, not the in-memory LastAttemptAt.
func runTurnCapThenContinue(t *testing.T, restart, disableExemption bool) (resumeOnContinuation bool) {
	t.Helper()
	sessRoot := t.TempDir()
	env := NewEnv(t, EnvOptions{Stages: sessionResumeStages("fresh", 0), Yolo: boolPtr(false)})
	env.Engine.SetSessionDirForTest(sessRoot)
	if disableExemption {
		env.Engine.SetContinuationExemptionDisabledForTest(true)
	}
	FileIssue(t, env, "continuation", "body", "Research")
	var mu sync.Mutex
	var resumes []bool
	script := func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		mu.Lock()
		resumes = append(resumes, resume)
		n := len(resumes)
		mu.Unlock()
		if n == 1 {
			// What the real invoker leaves behind after a turn-capped run.
			env.Engine.RecordSessionForTest(issue, stage.Name, "S1", 10, false)
			return "partial", false, engine.TokenUsage{TurnsUsed: 50, MaxTurns: 50}, &claudeerr.TurnLimitError{TerminalReason: "max_turns", NumTurns: 50}
		}
		return simclaude.DefaultScript(ctx, stage, issue, newComments, resume, workDir, opts)
	}
	env.Claude.ForStage("Research", script)
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Research") >= 1 }, 40)
	if restart {
		env = RestartEnv(t, env)
		// Only the engine's memory is gone; the saved session record is on disk.
		env.Engine.SetSessionDirForTest(sessRoot)
		if disableExemption {
			env.Engine.SetContinuationExemptionDisabledForTest(true)
		}
	}
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Research") >= 2 }, 40)
	mu.Lock()
	defer mu.Unlock()
	if len(resumes) < 2 {
		t.Fatalf("resume flags = %v, want two stage calls", resumes)
	}
	return resumes[1]
}

func TestSessionResume_TurnCappedContinuationResumesUnderFresh(t *testing.T) {
	t.Parallel()
	if !runTurnCapThenContinue(t, false, false) {
		t.Error("continuation after a time slice must resume under resume_session: fresh")
	}
}

// Non-vacuity: remove the continuation exemption and the same scenario fails.
func TestSessionResume_ContinuationExemptionRemovedStartsFresh(t *testing.T) {
	t.Parallel()
	if runTurnCapThenContinue(t, false, true) {
		t.Error("with the exemption removed the continuation should start fresh under resume_session: fresh")
	}
}

// The same continuation after a daemon restart (empty store, LastAttemptAt gone)
// still resumes: the decision rests on the durable record, not on uptime.
func TestSessionResume_TurnCappedContinuationResumesAfterRestart(t *testing.T) {
	t.Parallel()
	if !runTurnCapThenContinue(t, true, false) {
		t.Error("continuation after a restart must resume under resume_session: fresh")
	}
}

func TestSessionResume_RestartWithoutExemptionStartsFresh(t *testing.T) {
	t.Parallel()
	if runTurnCapThenContinue(t, true, true) {
		t.Error("with the exemption removed the post-restart continuation should start fresh")
	}
}
