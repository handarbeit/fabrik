package sim

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeerr"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Sim twins for #2232 (ADR 2232): an engine-scheduled retry owns an admission
// cooldown, and feedback already cached when a worker ends is admitted on the
// next poll, instead of both waiting for the 10 × PollSeconds periodic
// re-evaluation (mentioned for context: #1831, #2096).
//
// LastAttemptAt is wall-clock while cooldowns are on the injected Clock, so
// these scenarios set a small real RetryBackoff; the 10-poll periodic interval
// (1800 injected seconds at PollSeconds 180) is what an unfixed engine waits.

const incompleteRetryPollSeconds = 180

func incompleteRetryEnv(t *testing.T, retryBackoff time.Duration, boardCache bool) *Env {
	t.Helper()
	return NewEnv(t, EnvOptions{
		Stages:     failureShapeStages(),
		BoardCache: boardCache,
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.PollSeconds = incompleteRetryPollSeconds
			cfg.RetryBackoff = retryBackoff
		},
	})
}

// SC-001: a stage that ends incomplete is re-dispatched on one of the first polls
// after its backoff, not after 10 × poll.
func TestIncompleteStageRetriesAfterBackoffNotPeriodicReEval(t *testing.T) {
	t.Parallel()
	env := incompleteRetryEnv(t, 20*time.Millisecond, false)
	env.Claude.ForStage("Specify", simclaude.WallTimeKilled(), simclaude.DefaultScript)
	FileIssue(t, env, "wall-time stop", "body", "Specify")

	RunPoll(t, env) // dispatches; the worker is stopped by max_wall_time
	if got := env.Claude.StageCallCount("Specify"); got != 1 {
		t.Fatalf("after the first poll: %d invocation(s), want 1", got)
	}
	for poll := 1; poll <= 12; poll++ {
		RunPoll(t, env)
		if env.Claude.StageCallCount("Specify") >= 2 {
			if poll > 3 {
				t.Fatalf("retried only on poll %d after the stop, want <= 3 (10 × poll is the unfixed wait)\n\n%s", poll, diagnostics(env))
			}
			return
		}
	}
	t.Fatalf("stage never retried within 12 polls\n\n%s", diagnostics(env))
}

// SC-004: a human comment that reaches the cache while the worker runs is
// handled by the first poll after the worker ends. The retry backoff is an hour
// so the stage's own retry cannot be what admits the item.
func TestFeedbackCachedDuringRunIsAdmittedAfterWorkerEnds(t *testing.T) {
	t.Parallel()
	env := incompleteRetryEnv(t, time.Hour, true)
	t.Cleanup(env.Engine.RegisterObservers())
	var num int
	arrive := func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		// The human comments while the worker runs; the webhook path reads it into the cache.
		env.Sim.Sim().SeedComment(env.OwnerRepo, num, "maintainer", "please also handle the empty case")
		// Hand the cache exactly what GitHub now holds, as the webhook path would.
		item := issue
		if err := env.Sim.FetchItemDetails(&item); err != nil || len(item.Comments) == 0 {
			t.Errorf("reading the seeded comment: %v (%d comments)", err, len(item.Comments))
			return "", false, engine.TokenUsage{}, nil
		}
		env.BoardCache.ApplyCommentAdded(fmt.Sprintf("%s#%d", env.OwnerRepo, num), item.Comments[len(item.Comments)-1])
		// The comment's wake runs a poll while the worker is still in flight: it
		// admits the item, drains the signal and drops it because a worker runs
		// (the #2210 shape). Nothing signals the comment again once the run ends.
		if err := env.Engine.PollOnce(ctx); err != nil {
			t.Errorf("mid-run poll: %v", err)
		}
		// Killed with no output: the engine then writes no stage comment on its way
		// out, whose own CommentsChanged would re-admit the item and mask the loss.
		return "", false, engine.TokenUsage{TurnsUsed: 12, MaxTurns: 50}, &claudeerr.KilledError{Cause: claudeerr.KillWallTime, Err: fmt.Errorf("claude exited with error: signal: killed")}
	}
	env.Claude.ForStage("Specify", arrive, simclaude.DefaultScript)
	num = FileIssue(t, env, "comment arrives mid-run", "body", "Specify")

	// The board cache warms over the first polls; the worker then ends incomplete
	// with the comment cached.
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 10)
	for poll := 1; poll <= 12; poll++ {
		RunPoll(t, env)
		if env.Claude.CommentCallCount("Specify") >= 1 || env.Claude.StageCallCount("Specify") >= 2 {
			if poll > 3 {
				t.Fatalf("comment pass started only on poll %d after the worker ended, want <= 3\n\n%s", poll, diagnostics(env))
			}
			// Admission is not a hot loop: once the comment is handled, the stage's
			// own retry still waits out its (hour-long) backoff.
			stageCalls, commentCalls := env.Claude.StageCallCount("Specify"), env.Claude.CommentCallCount("Specify")
			for i := 0; i < 4; i++ {
				RunPoll(t, env)
			}
			if got := env.Claude.StageCallCount("Specify"); got != stageCalls {
				t.Fatalf("stage re-dispatched inside its backoff: %d -> %d invocations\n\n%s", stageCalls, got, diagnostics(env))
			}
			if got := env.Claude.CommentCallCount("Specify"); got > commentCalls+1 {
				t.Fatalf("comment pass kept re-running: %d -> %d invocations\n\n%s", commentCalls, got, diagnostics(env))
			}
			return
		}
	}
	t.Fatalf("comment never processed within 12 polls\n\n%s", diagnostics(env))
}
