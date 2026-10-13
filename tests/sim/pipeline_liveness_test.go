package sim

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// Pipeline liveness scenarios (#2124, ADR 2124): an overseer reading only
// fabrik_health and the pushed events can tell that the engine, not an item, has
// stopped. The sim clock is shared with the engine, so "overdue" is controlled
// by Clock.Advance and nothing sleeps for a stall; the only real time spent is
// the deriver's shortened tick (startChannel).
//
// Serial for the same reason as channel_events_test.go.

// startPipelineChannel is startChannel with a short deriver tick: the stall
// events are tick-driven (the deriver never depends on the poll loop making
// progress), so the tick has to fire within the test.
func startPipelineChannel(t *testing.T, env *Env) (*channelevents.Hub, *simSink) {
	t.Helper()
	restore := engine.SetChannelTimingForTest(20*time.Millisecond, 25*time.Millisecond)
	t.Cleanup(restore)
	hub := env.Engine.StartChannelEventsForTest(t.TempDir())
	if hub == nil {
		t.Fatal("channel hub did not start")
	}
	t.Cleanup(env.Engine.StopChannelEventsForTest)
	none := []string{}
	if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "S", ExcludeLabels: &none}); err != nil {
		t.Fatal(err)
	}
	sink := &simSink{}
	hub.Attach("S", sink)
	return hub, sink
}

func healthOf(t *testing.T, env *Env) *localapi.HealthResult {
	t.Helper()
	h, err := env.Engine.LocalAPIBackend().Health(localapi.HealthParams{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	return h
}

// waitForEvents polls the sink until n events of typ have arrived (bounded), then
// waits a few more ticks and re-counts so a duplicate would be seen.
func waitForEvents(t *testing.T, sink *simSink, typ channelevents.EventType, n int) []channelevents.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.ofType(typ, 0)) >= n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	got := sink.ofType(typ, 0)
	if len(got) != n {
		t.Fatalf("%s events = %d, want exactly %d: %+v", typ, len(got), n, got)
	}
	return got
}

// holdBoardFetch makes the next FetchProjectBoard block until release is closed,
// and returns a channel that closes once the poll is inside it. The match
// closure never injects a fault: it only blocks, once.
func holdBoardFetch(env *Env) (entered <-chan struct{}, release func()) {
	in := make(chan struct{})
	rel := make(chan struct{})
	first := true
	env.Sim.Faults().FailWhen("FetchProjectBoard", func(simgh.Args) bool {
		if first {
			first = false
			close(in)
			<-rel
		}
		return false
	}, simgh.Always, errors.New("never injected: the match only blocks"))
	return in, func() { close(rel) }
}

func TestPipelineStalledWhilePollInProgress(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), Yolo: boolPtr(false)})
	_, sink := startPipelineChannel(t, env)
	RunPoll(t, env) // a healthy baseline poll

	if v := healthOf(t, env).Poll.Overdue; !v.Valid || v.V {
		t.Fatalf("a freshly polled engine must not be overdue: %+v", v)
	}

	entered, release := holdBoardFetch(env)
	started := env.Clock.Now()
	done := make(chan error, 1)
	go func() { done <- env.Engine.PollOnce(context.Background()) }()
	<-entered

	env.Clock.Advance(5 * time.Second) // > K x the 1 s interval
	h := healthOf(t, env).Poll
	if !h.InProgress || !h.Overdue.Valid || !h.Overdue.V {
		t.Fatalf("a poll held past K x interval must report in_progress and overdue: %+v", h)
	}
	if !h.CurrentStartedAt.Valid || !h.CurrentStartedAt.V.Equal(started.UTC()) {
		t.Fatalf("current_started_at = %+v, want %v", h.CurrentStartedAt, started)
	}
	if h.OverdueCause != "poll-in-progress" || !h.Step.Valid || h.Step.V != "fetching the project board" {
		t.Fatalf("cause/step = %q / %+v", h.OverdueCause, h.Step)
	}

	stalled := waitForEvents(t, sink, channelevents.PipelineStalled, 1)
	if m := stalled[0].Meta; m["scope"] != "poll" || m["cause"] != "poll-in-progress" || m["step"] != "fetching the project board" || m["interval_seconds"] != "1" {
		t.Fatalf("pipeline-stalled meta = %v", m)
	}
	if n := len(sink.ofType(channelevents.PipelineResumed, 0)); n != 0 {
		t.Fatalf("pipeline-resumed fired while still stalled: %d", n)
	}

	release()
	if err := <-done; err != nil {
		t.Fatalf("held poll: %v", err)
	}
	waitForEvents(t, sink, channelevents.PipelineResumed, 1)
	waitForEvents(t, sink, channelevents.PipelineStalled, 1)
	h = healthOf(t, env).Poll
	if h.InProgress || !h.Overdue.Valid || h.Overdue.V || !h.LastCompletedAt.Valid {
		t.Fatalf("recovered poll: %+v", h)
	}
}

func TestPipelineOverdueWithNoPollStarted(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), Yolo: boolPtr(false)})
	_, sink := startPipelineChannel(t, env)
	RunPoll(t, env)

	env.Clock.Advance(30 * time.Second) // no poll within K x interval of its due time
	h := healthOf(t, env).Poll
	if h.InProgress || !h.Overdue.Valid || !h.Overdue.V || h.OverdueCause != "no-poll-started" {
		t.Fatalf("absent poll must be overdue (not in progress): %+v", h)
	}
	if got := waitForEvents(t, sink, channelevents.PipelineStalled, 1); got[0].Meta["cause"] != "no-poll-started" {
		t.Fatalf("meta = %v", got[0].Meta)
	}

	RunPoll(t, env) // the poll comes back
	waitForEvents(t, sink, channelevents.PipelineResumed, 1)

	// A later, separate episode raises its own pair.
	env.Clock.Advance(time.Minute)
	waitForEvents(t, sink, channelevents.PipelineStalled, 2)
	RunPoll(t, env)
	waitForEvents(t, sink, channelevents.PipelineResumed, 2)
}

func TestPipelineGaugeAsOfMovesOnlyWhenPollRefreshes(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), Yolo: boolPtr(false)})
	if g := healthOf(t, env).Backoff; g.AsOf.Valid {
		t.Fatalf("a never-refreshed gauge must be unknown, got %+v", g.AsOf)
	}
	env.Sim.Sim().SeedRateLimits(5000, 5000, 5000, 4673)
	runPollWithBackoff(t, env, env.PollInterval)
	first := healthOf(t, env).Backoff
	if !first.AsOf.Valid || !first.Remaining.Valid || first.Remaining.V != 4673 {
		t.Fatalf("gauge after a refreshing poll: %+v", first)
	}

	env.Clock.Advance(time.Hour)
	if g := healthOf(t, env).Backoff; !g.AsOf.V.Equal(first.AsOf.V) {
		t.Fatalf("as-of moved with time alone: %v -> %v", first.AsOf.V, g.AsOf.V)
	}
	// PollOnce does not refresh the gauge either (only PollWithBackoff reads it).
	RunPoll(t, env)
	if g := healthOf(t, env).Backoff; !g.AsOf.V.Equal(first.AsOf.V) {
		t.Fatalf("as-of moved without a gauge refresh: %v -> %v", first.AsOf.V, g.AsOf.V)
	}

	env.Sim.Sim().SeedRateLimits(5000, 5000, 5000, 4000)
	runPollWithBackoff(t, env, env.PollInterval)
	if g := healthOf(t, env).Backoff; !g.AsOf.V.After(first.AsOf.V) || g.Remaining.V != 4000 {
		t.Fatalf("as-of must advance when the poll refreshes the gauge: %+v", g)
	}
}

// stuckItemEnv files an item at a completed Validate stage under a manual
// pipeline (yolo off) and polls twice, so it is cached with a short dispatch
// cooldown. Only the passage of time can then make it overdue. It is filed at
// Validate because a completed earlier stage with no autonomy is a pending
// decision (suspended, #2120), which is never stalled; Validate is excluded.
func stuckItemEnv(t *testing.T) (*Env, string) {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:     smokeStages(),
		Yolo:       boolPtr(false),
		BoardCache: true,       // StatusEnteredAt (the progress anchor) is only observed with the cache,
		StartTime:  time.Now(), // and it is stamped from the wall clock, so the sim clock must start there
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.StallThreshold = 20 * time.Second
		},
	})
	num := FileIssue(t, env, "stuck item", "body", "Validate", "stage:Validate:complete")
	ref := env.OwnerRepo + "#" + strconv.Itoa(num)
	for i := 0; i < 2; i++ {
		if err := env.Engine.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return env, ref
}

func attentionOf(t *testing.T, env *Env, ref string) localapi.AttentionInfo {
	t.Helper()
	st, err := env.Engine.LocalAPIBackend().Status(localapi.StatusParams{Issue: ref})
	if err != nil {
		t.Fatalf("Status(%s): %v", ref, err)
	}
	return st.Attention
}

// The incident: no poll ran, so the item's dispatch cooldown expired and nothing
// re-dispatched it. Without attribution that reads as the item being
// stalled/overdue and pushes a stalled event; with it, the poll is blamed once
// (pipeline-stalled) and the item waits for it.
func TestStalledPollIsBlamedNotTheItem(t *testing.T) {
	env, ref := stuckItemEnv(t)
	_, sink := startPipelineChannel(t, env)
	env.Clock.Advance(time.Minute) // cooldown (10 s) expired 50 s ago: past the 20 s stall threshold

	if !healthOf(t, env).Poll.Overdue.V {
		t.Fatal("test setup: the poll must be overdue")
	}
	a := attentionOf(t, env, ref)
	if a.State != "waiting" || a.Code != "waiting-for-poll" {
		t.Fatalf("attention = %s/%s (%s), want waiting/waiting-for-poll", a.State, a.Code, a.Summary)
	}
	if want := "waiting for the poll: blocked for "; len(a.Summary) < len(want) || a.Summary[:len(want)] != want {
		t.Fatalf("summary = %q", a.Summary)
	}
	waitForEvents(t, sink, channelevents.PipelineStalled, 1)
	if got := sink.ofType(channelevents.Stalled, 0); len(got) != 0 {
		t.Fatalf("an item-level stalled push was published while the poll was overdue: %+v", got)
	}

	// The poll comes back: the item is re-evaluated by it and the cause is cleared.
	RunPoll(t, env)
	waitForEvents(t, sink, channelevents.PipelineResumed, 1)
	if a := attentionOf(t, env, ref); a.Code == "waiting-for-poll" {
		t.Fatalf("attribution outlived the stall: %s/%s", a.State, a.Code)
	}
	if got := sink.ofType(channelevents.Stalled, 0); len(got) != 0 {
		t.Fatalf("a stalled push fired on recovery: %+v", got)
	}
}
