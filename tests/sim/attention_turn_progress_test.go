package sim

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// #2604 scenarios, through a real Engine and the channel deriver: a worker that
// keeps taking turns past the stall threshold produces no `stalled` push; one
// whose turns stop does, with the turn-based wording. Neutralising the turn
// progress (SetTurnProgressDisabledForTest) makes the healthy run read stalled
// again, so the first scenario is not vacuous.

type turnRun struct {
	env   *Env
	sink  *simSink
	num   int
	turns chan struct{} // one message = one turn
	acked chan struct{}
	end   chan struct{}
}

func startTurnRun(t *testing.T) *turnRun {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:    failureShapeStages(),
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.StallThreshold = 30 * time.Minute
		},
	})
	r := &turnRun{env: env, turns: make(chan struct{}), acked: make(chan struct{}), end: make(chan struct{})}
	entered := make(chan struct{}, 1)
	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		opts.OnTurn(0, 100)
		entered <- struct{}{}
		n := 0
		for {
			select {
			case <-r.turns:
				n++
				opts.OnTurn(n, 100)
				r.acked <- struct{}{}
			case <-r.end:
				return simclaude.DefaultScript(ctx, stage, issue, newComments, resume, workDir, opts)
			case <-ctx.Done():
				return "", false, engine.TokenUsage{}, ctx.Err()
			}
		}
	})
	t.Cleanup(func() {
		select {
		case <-r.end:
		default:
			close(r.end)
		}
		// Releasing the held worker lets it finish its script, which commits and
		// pushes into the env's origin.git. Wait for it, or TempDir's RemoveAll
		// races the push ("directory not empty").
		waitForWorkerQuiescence(t, env)
	})
	r.num = FileIssue(t, env, "long healthy run", "body", "Specify")
	_, r.sink = startChannelAt(t, env, t.TempDir())

	// With the board cache the first polls only populate it; poll until the
	// worker is dispatched. PollOnce returns while the worker is held in the
	// scripted invocation (RunPoll would wait for it).
	for i := 0; i < 6; i++ {
		if env.PollInterval > 0 {
			env.Clock.Advance(env.PollInterval)
		}
		done := make(chan error, 1)
		go func() { done <- env.Engine.PollOnce(context.Background()) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("poll: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("poll did not return\n\n%s", diagnostics(env))
		}
		select {
		case <-entered:
			return r
		case <-time.After(2 * time.Second):
		}
	}
	t.Fatalf("worker did not reach the scripted invocation\n\n%s", diagnostics(env))
	return r
}

// turn advances the sim clock by d and delivers one turn.
func (r *turnRun) turn(t *testing.T, d time.Duration) {
	t.Helper()
	r.env.Clock.Advance(d)
	r.turns <- struct{}{}
	select {
	case <-r.acked:
	case <-time.After(30 * time.Second):
		t.Fatal("turn was not acknowledged")
	}
}

// tick lets the deriver evaluate at the current sim time (its ticker is real-time).
func (r *turnRun) tick(t *testing.T) {
	t.Helper()
	waitRealTime(t, 250*time.Millisecond, "the deriver's ticker is real-time and re-evaluates against the sim clock")
	settleWait(r.env)
}

func TestSimLongRunWithLiveTurnsProducesNoStalledPush(t *testing.T) {
	t.Parallel()
	r := startTurnRun(t)
	for i := 0; i < 10; i++ { // 10 × 6m = 60m, twice the stall threshold
		r.turn(t, 6*time.Minute)
		r.tick(t)
	}
	if got := r.sink.ofType(channelevents.Stalled, r.num); len(got) != 0 {
		t.Fatalf("a worker taking turns for 60m was announced as stalled: %+v", got)
	}
}

func TestSimLongRunWithTurnProgressNeutralisedReadsStalled(t *testing.T) {
	t.Parallel()
	r := startTurnRun(t)
	restore := r.env.Engine.SetTurnProgressDisabledForTest()
	defer restore()
	for i := 0; i < 10; i++ {
		r.turn(t, 6*time.Minute)
		r.tick(t)
	}
	if got := r.sink.ofType(channelevents.Stalled, r.num); len(got) == 0 {
		t.Fatal("with turn progress neutralised the healthy long run was not reported stalled: the twin proves nothing")
	}
}

func TestSimWorkerWhoseTurnsStopIsReportedStalled(t *testing.T) {
	t.Parallel()
	r := startTurnRun(t)
	for i := 0; i < 3; i++ {
		r.turn(t, 6*time.Minute)
		r.tick(t)
	}
	if got := r.sink.ofType(channelevents.Stalled, r.num); len(got) != 0 {
		t.Fatalf("stalled while turns were arriving: %+v", got)
	}
	// Turns stop; the clock passes the stall threshold since the last one.
	r.env.Clock.Advance(31 * time.Minute)
	r.tick(t)
	got := r.sink.ofType(channelevents.Stalled, r.num)
	if len(got) != 1 {
		t.Fatalf("stalled pushes = %d, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Content, "no turn for 31m0s (inactivity timeout 15m0s did not end it)") {
		t.Errorf("content = %q", got[0].Content)
	}
}
