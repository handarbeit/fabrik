package sim

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Sim twins for #2499 (ADR 2499): a stage whose Claude process fails to start
// owns an admission cooldown, and a persistent start failure backs off instead
// of looping at poll rate. The warnings-panel entry is asserted by the engine
// unit tests (the panel file is process-wide, which parallel sim tests share).
// The consecutive-failure hold's non-vacuity is likewise proven there: the sim
// has no extra admission signal, so the recorded cooldown alone already bounds
// the loop here, while the hold bounds it through any admission route.

func startFailureEnv(t *testing.T) *Env {
	t.Helper()
	return NewEnv(t, EnvOptions{
		Stages: failureShapeStages(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.PollSeconds = incompleteRetryPollSeconds // 180 s injected per poll
			cfg.RetryBackoff = 10 * time.Minute
		},
	})
}

// A transient start failure is retried on the next poll, not after the periodic
// re-evaluation (10 × poll), and does not mark the stage failed.
func TestStartFailureTransientRetriesOnNextPoll(t *testing.T) {
	t.Parallel()
	env := startFailureEnv(t)
	env.Claude.ForStage("Specify", simclaude.StartFailure(), simclaude.DefaultScript)
	num := FileIssue(t, env, "start failure", "body", "Specify")

	RunPoll(t, env)
	if got := env.Claude.StageCallCount("Specify"); got != 1 {
		t.Fatalf("after the first poll: %d invocation(s), want 1\n\n%s", got, diagnostics(env))
	}
	for poll := 1; poll <= 6; poll++ {
		RunPoll(t, env)
		if env.Claude.StageCallCount("Specify") >= 2 {
			if poll > 3 {
				t.Fatalf("retried only on poll %d after the failure, want <= 3 (10 × poll is the unfixed wait)\n\n%s", poll, diagnostics(env))
			}
			if hasLabel(IssueLabels(t, env, num), "stage:Specify:failed") {
				t.Errorf("stage:Specify:failed applied after a start failure")
			}
			return
		}
	}
	t.Fatalf("stage never retried within 6 polls\n\n%s", diagnostics(env))
}

// A start failure that never clears does not loop at poll rate: launches over a
// fixed window are bounded well below one per poll, the stage is never failed
// or paused, and a later successful start completes it.
func TestStartFailurePersistentIsBoundedAndRecovers(t *testing.T) {
	t.Parallel()
	env := startFailureEnv(t)
	env.Claude.ForStage("Specify", simclaude.StartFailure())
	num := FileIssue(t, env, "persistent start failure", "body", "Specify")

	const polls = 20
	RunPolls(t, env, polls)
	launches := env.Claude.StageCallCount("Specify")
	if launches < 2 {
		t.Fatalf("only %d launch(es) in %d polls; the retry never happened\n\n%s", launches, polls, diagnostics(env))
	}
	if launches > polls/2 {
		t.Fatalf("%d launches in %d polls: start failures loop near poll rate\n\n%s", launches, polls, diagnostics(env))
	}
	for _, l := range []string{"stage:Specify:failed", "fabrik:paused"} {
		if hasLabel(IssueLabels(t, env, num), l) {
			t.Errorf("label %q applied by a start failure", l)
		}
	}

	// Recovery: the binary is back; the next admitted launch completes the stage.
	env.Claude.ForStage("Specify", simclaude.DefaultScript)
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 40)
	if hasLabel(IssueLabels(t, env, num), "stage:Specify:failed") {
		t.Errorf("stage failed after recovery")
	}
}
