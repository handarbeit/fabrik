package sim

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Startup resume of daemon clean-stop pauses (#2486, ADR 2486). A real clean-stop
// pause is written through Engine.PauseInFlightForShutdownForTest (the production
// shutdown order, driven without a signal) while a worker is in flight; the
// daemon is then restarted with RestartEnv.
//
//   - resumed: nothing else happens between stop and restart, so the first poll
//     of the new daemon lifts the pause, posts exactly one Fabrik comment, and the
//     stage is dispatched again without a human comment;
//   - a human re-pause after the stop (labels removed and re-applied, strictly
//     later than the stop comment — asserted, so the case cannot pass vacuously)
//     keeps the item paused with no engine write;
//   - SetCleanStopResumeDisabledForTest (and the opt-out) leave the pause in place,
//     which is what makes the first scenario fail without the feature.

const csResumeStage = "Specify"

type cleanStopWorld struct {
	env   *Env
	num   int
	calls atomic.Int32
}

// newCleanStopWorld files an issue, holds its Specify worker in flight, performs a
// clean stop and returns once the worker has wound down with the pause in place.
func newCleanStopWorld(t *testing.T, resumeEnabled bool) *cleanStopWorld {
	t.Helper()
	w := &cleanStopWorld{}
	w.env = NewEnv(t, EnvOptions{
		Stages:       smokeStages(),
		ConfigureCfg: func(cfg *engine.Config) { cfg.ResumeAfterCleanStop = resumeEnabled },
	})
	// The first invocation blocks until the stop cancels it; later ones (after the
	// restart) behave like an ordinary stage run.
	w.env.Claude.ForStage(csResumeStage, func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if w.calls.Add(1) == 1 {
			<-ctx.Done()
			return "", false, engine.TokenUsage{}, ctx.Err()
		}
		return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
	})

	w.num = FileIssue(t, w.env, "clean stop", "Interrupted by a clean stop.", csResumeStage)
	pollWithoutWaiting(t, w.env)
	deadline := time.Now().Add(10 * time.Second)
	for !w.env.Engine.HasInFlightWorker() || w.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the Specify worker never started\n\n%s", diagnostics(w.env))
		}
		time.Sleep(5 * time.Millisecond)
	}

	w.env.Engine.PauseInFlightForShutdownForTest()
	waitForWorkerQuiescence(t, w.env)

	labels := IssueLabels(t, w.env, w.num)
	if !hasLabel(labels, "fabrik:paused") || !hasLabel(labels, "fabrik:awaiting-input") {
		t.Fatalf("the clean stop did not pause the item: labels=%v", labels)
	}
	if got := w.stopComment(t); got.CreatedAt.IsZero() {
		t.Fatal("no daemon-stop marker comment was written by the clean stop")
	}
	return w
}

func (w *cleanStopWorld) comments(t *testing.T, env *Env) []gh.Comment {
	t.Helper()
	cs, err := env.Sim.FetchIssueComments(env.Owner, env.Repo, w.num)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	return cs
}

func (w *cleanStopWorld) stopComment(t *testing.T) gh.Comment {
	t.Helper()
	for _, c := range w.comments(t, w.env) {
		if strings.Contains(c.Body, "<!-- fabrik:pause-cause=daemon-stop -->") {
			return c
		}
	}
	return gh.Comment{}
}

func (w *cleanStopWorld) resumeComments(t *testing.T, env *Env) int {
	t.Helper()
	n := 0
	for _, c := range w.comments(t, env) {
		if strings.HasPrefix(c.Body, "🏭 **Fabrik — resumed after restart**") {
			n++
		}
	}
	return n
}

func TestCleanStopResume_InFlightIssueResumesAfterRestartWithoutAComment(t *testing.T) {
	t.Parallel()
	w := newCleanStopWorld(t, true)
	commentsBefore := len(w.comments(t, w.env))

	w.env.Clock.Advance(5 * time.Minute)
	restarted := RestartEnv(t, w.env)

	for i := 0; i < 8 && w.calls.Load() < 2; i++ {
		RunPoll(t, restarted)
	}

	if labels := IssueLabels(t, restarted, w.num); hasLabel(labels, "fabrik:paused") || hasLabel(labels, "fabrik:awaiting-input") || hasLabel(labels, "fabrik:pause:daemon-stop") {
		t.Errorf("the clean-stop pause was not lifted: labels=%v", labels)
	}
	if got := w.resumeComments(t, restarted); got != 1 {
		t.Errorf("resume comments = %d, want exactly 1", got)
	}
	// The only comments added since the stop are the engine's: no human comment.
	for _, c := range w.comments(t, restarted)[commentsBefore:] {
		if !strings.HasPrefix(c.Body, "🏭 **Fabrik") {
			t.Errorf("a non-Fabrik comment appeared: %q", c.Body)
		}
	}
	if got := w.calls.Load(); got < 2 {
		t.Errorf("Specify invocations = %d, want the interrupted stage redispatched", got)
	}

	// A single pause is resumed once: more polls add no second comment.
	RunPolls(t, restarted, 3)
	if got := w.resumeComments(t, restarted); got != 1 {
		t.Errorf("resume comments after more polls = %d, want 1", got)
	}
}

func TestCleanStopResume_HumanRepauseAfterTheStopStaysPaused(t *testing.T) {
	t.Parallel()
	w := newCleanStopWorld(t, true)
	stopAt := w.stopComment(t).CreatedAt

	// A human lifts the pause and pauses it again while the daemon is down.
	w.env.Clock.Advance(time.Minute)
	sim := w.env.Sim.Sim()
	for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input", "fabrik:pause:daemon-stop"} {
		if err := sim.RemoveLabelFromIssue(w.env.Owner, w.env.Repo, w.num, l); err != nil {
			t.Fatalf("remove %s: %v", l, err)
		}
	}
	w.env.Clock.Advance(time.Minute)
	for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
		if err := sim.AddLabelToIssue(w.env.Owner, w.env.Repo, w.num, l); err != nil {
			t.Fatalf("add %s: %v", l, err)
		}
	}
	// Non-vacuity: the human's pause event is strictly later than the stop comment.
	pausedAt, err := w.env.Sim.FetchLabelAppliedAt(w.env.Owner, w.env.Repo, w.num, "fabrik:paused")
	if err != nil {
		t.Fatalf("FetchLabelAppliedAt: %v", err)
	}
	if !pausedAt.After(stopAt) {
		t.Fatalf("non-vacuity: the human pause (%s) is not after the stop comment (%s)", pausedAt, stopAt)
	}
	commentsBefore := len(w.comments(t, w.env))

	restarted := RestartEnv(t, w.env)
	RunPolls(t, restarted, 4)

	if !hasLabel(IssueLabels(t, restarted, w.num), "fabrik:paused") {
		t.Error("the human's pause was lifted by the startup resume")
	}
	if got := w.calls.Load(); got != 1 {
		t.Errorf("Specify invocations = %d, want 1 (no redispatch of a human-paused item)", got)
	}
	if n := len(w.comments(t, restarted)); n != commentsBefore {
		t.Errorf("the engine wrote %d comment(s) on a human-paused item", n-commentsBefore)
	}
}

// Neutralising the resume (the seam, or the opt-out setting) leaves the item
// paused: this is the negative of the first scenario, so it fails without the feature.
func TestCleanStopResume_Neutralised_LeavesThePauseInPlace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		enabled bool
		seam    bool
	}{
		{"seam", true, true},
		{"opt-out", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newCleanStopWorld(t, tc.enabled)
			w.env.Clock.Advance(5 * time.Minute)
			restarted := RestartEnv(t, w.env)
			if tc.seam {
				restarted.Engine.SetCleanStopResumeDisabledForTest(true)
			}
			RunPolls(t, restarted, 4)

			if !hasLabel(IssueLabels(t, restarted, w.num), "fabrik:paused") {
				t.Error("the item was resumed although the resume is switched off")
			}
			if got := w.resumeComments(t, restarted); got != 0 {
				t.Errorf("resume comments = %d, want 0", got)
			}
			if got := w.calls.Load(); got != 1 {
				t.Errorf("Specify invocations = %d, want 1", got)
			}
		})
	}
}
