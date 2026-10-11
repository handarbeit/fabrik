package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// pipelineEngine is a channel engine with a 30 s poll interval (so K x interval
// is 60 s) and its adjustable clock. The deriver tick is an hour, so the test
// drives checkPipeline itself on the test goroutine.
func pipelineEngine(t *testing.T, sub *channelevents.Subscription) (*Engine, *chanSink, *adjClock, *channelEvents) {
	t.Helper()
	e, sink := channelEngine(t, 0, sub)
	e.cfg.PollSeconds = 30
	e.cfg.MergeTrain = "on"
	clk := e.clock.(*adjClock)
	e.health.markStarted(clk.Now())
	return e, sink, clk, e.channelEvents()
}

func drain(sink *chanSink, window time.Duration) []channelevents.Event {
	var out []channelevents.Event
	deadline := time.After(window)
	for {
		select {
		case ev := <-sink.ch:
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
}

func ofType(evs []channelevents.Event, typ channelevents.EventType) []channelevents.Event {
	var out []channelevents.Event
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func TestPipelineStallEpisodesPublishOncePerEpisode(t *testing.T) {
	// A narrowly scoped subscription still gets the account-wide events.
	e, sink, clk, ce := pipelineEngine(t, &channelevents.Subscription{Repos: []string{"nobody/else"}, Milestone: "v9"})
	t0 := clk.Now()
	verdictAt := func(at time.Time) pollVerdict { return e.pollVerdictFor(e.health.snapshot(), at) }

	// A poll in progress for less than K x interval: nothing.
	e.health.beginPoll(t0, pollVerdict{})
	e.setPollStep("fetching the project board")
	clk.set(t0.Add(30 * time.Second))
	ce.checkPipeline()
	if evs := drain(sink, 100*time.Millisecond); len(ofType(evs, channelevents.PipelineStalled)) != 0 {
		t.Fatalf("stalled before K x interval: %+v", evs)
	}

	// Past K x interval: exactly one pipeline-stalled, however often it is evaluated.
	clk.set(t0.Add(90 * time.Second))
	ce.checkPipeline()
	ce.checkPipeline()
	ce.checkPipeline()
	evs := drain(sink, 150*time.Millisecond)
	st := ofType(evs, channelevents.PipelineStalled)
	if len(st) != 1 {
		t.Fatalf("pipeline-stalled events = %d, want 1: %+v", len(st), evs)
	}
	m := st[0].Meta
	if m["scope"] != "poll" || m["cause"] != pollCauseInProgress || m["step"] != "fetching the project board" ||
		m["interval_seconds"] != "30" || m["blocked_seconds"] != "90" || m["event"] != "pipeline-stalled" {
		t.Fatalf("meta = %v", m)
	}
	if st[0].Issue != 0 {
		t.Fatalf("pipeline-stalled must be account-wide, got issue %d", st[0].Issue)
	}

	// Completion ends the episode: exactly one pipeline-resumed.
	e.health.endPoll(clk.Now(), verdictAt(clk.Now()), clk.Now().Add(30*time.Second))
	ce.checkPipeline()
	ce.checkPipeline()
	evs = drain(sink, 150*time.Millisecond)
	if rs := ofType(evs, channelevents.PipelineResumed); len(rs) != 1 || len(ofType(evs, channelevents.PipelineStalled)) != 0 {
		t.Fatalf("recovery events: %+v", evs)
	}

	// A later, separate episode raises its own pair.
	t1 := clk.Now().Add(time.Minute)
	clk.set(t1)
	e.health.beginPoll(t1, pollVerdict{})
	clk.set(t1.Add(2 * time.Minute))
	ce.checkPipeline()
	if len(ofType(drain(sink, 150*time.Millisecond), channelevents.PipelineStalled)) != 1 {
		t.Fatal("second episode must raise its own pipeline-stalled")
	}

	// A stall that ends and restarts between two evaluations is still two
	// episodes: resumed for the first, stalled for the second.
	end := clk.Now()
	e.health.endPoll(end, verdictAt(end), end.Add(30*time.Second))
	t2 := end.Add(time.Second)
	e.health.beginPoll(t2, pollVerdict{})
	clk.set(t2.Add(3 * time.Minute))
	ce.checkPipeline()
	evs = drain(sink, 150*time.Millisecond)
	if len(ofType(evs, channelevents.PipelineResumed)) != 1 || len(ofType(evs, channelevents.PipelineStalled)) != 1 {
		t.Fatalf("restall between evaluations: %+v", evs)
	}
}

func TestPipelineStalledWhenNoPollStarts(t *testing.T) {
	e, sink, clk, ce := pipelineEngine(t, nil)
	t0 := clk.Now()
	e.health.beginPoll(t0, pollVerdict{})
	e.health.endPoll(t0.Add(time.Second), pollVerdict{}, t0.Add(31*time.Second))

	clk.set(t0.Add(31*time.Second + 50*time.Second)) // under K x interval past due
	ce.checkPipeline()
	if evs := drain(sink, 100*time.Millisecond); len(evs) != 0 {
		t.Fatalf("stalled before K x interval past the due time: %+v", evs)
	}
	clk.set(t0.Add(31*time.Second + 70*time.Second))
	ce.checkPipeline()
	st := ofType(drain(sink, 150*time.Millisecond), channelevents.PipelineStalled)
	if len(st) != 1 || st[0].Meta["cause"] != pollCauseNoPoll {
		t.Fatalf("no-poll stall: %+v", st)
	}
	if _, has := st[0].Meta["step"]; has {
		t.Fatalf("no step to report when no poll is running: %v", st[0].Meta)
	}
	// The poll starting again resolves it.
	now := clk.Now()
	e.health.beginPoll(now, e.pollVerdictFor(e.health.snapshot(), now))
	ce.checkPipeline()
	if len(ofType(drain(sink, 150*time.Millisecond), channelevents.PipelineResumed)) != 1 {
		t.Fatal("a poll starting must end the no-poll episode with one pipeline-resumed")
	}
}

// Deliberate back-off is not a stall: the engine said the next poll is an hour away.
func TestPipelineBackoffIsNotAStall(t *testing.T) {
	e, sink, clk, ce := pipelineEngine(t, nil)
	t0 := clk.Now()
	e.health.beginPoll(t0, pollVerdict{})
	e.health.endPoll(t0.Add(time.Second), pollVerdict{}, t0.Add(31*time.Second))
	e.health.setNextPollDue(t0.Add(time.Hour), backoffReasonIdle)
	clk.set(t0.Add(50 * time.Minute))
	ce.checkPipeline()
	if evs := drain(sink, 100*time.Millisecond); len(evs) != 0 {
		t.Fatalf("idle back-off raised a stall: %+v", evs)
	}
	clk.set(t0.Add(time.Hour + 3*time.Minute))
	ce.checkPipeline()
	if len(ofType(drain(sink, 150*time.Millisecond), channelevents.PipelineStalled)) != 1 {
		t.Fatal("a poll missing its own due time by K x interval is a stall")
	}
}

func TestPipelinePartitionStallEpisode(t *testing.T) {
	e, sink, clk, ce := pipelineEngine(t, nil)
	// Healthy poll, nothing deliberate pending.
	t0 := clk.Now()
	e.health.beginPoll(t0, pollVerdict{})
	e.health.endPoll(t0, pollVerdict{}, t0.Add(30*time.Second))
	e.seedAPIItem(t, 1, "Queued")
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: gh.ProjectItem{ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Queued"}})

	// Queued for less than K x interval: no stall.
	clk.set(t0.Add(20 * time.Second))
	e.health.beginPoll(clk.Now(), pollVerdict{})
	e.health.endPoll(clk.Now(), pollVerdict{}, clk.Now().Add(30*time.Second))
	ce.checkPipeline()
	if evs := drain(sink, 100*time.Millisecond); len(ofType(evs, channelevents.PipelineStalled)) != 0 {
		t.Fatalf("early partition stall: %+v", evs)
	}

	// The member's queued-since is real wall time (the store stamps it), so the
	// clock moves past it by more than K x interval, with polls staying healthy.
	later := time.Now().Add(5 * time.Minute)
	clk.set(later)
	e.health.beginPoll(later, pollVerdict{})
	e.health.endPoll(later, pollVerdict{}, later.Add(30*time.Second))
	ce.checkPipeline()
	ce.checkPipeline()
	evs := drain(sink, 150*time.Millisecond)
	st := ofType(evs, channelevents.PipelineStalled)
	if len(st) != 1 {
		t.Fatalf("partition pipeline-stalled = %d, want 1: %+v", len(st), evs)
	}
	if m := st[0].Meta; m["scope"] != "merge-train" || m["partition"] != "owner/repo" || m["members"] != "1" || m["cause"] == "" {
		t.Fatalf("meta = %v", m)
	}

	// A run starting resolves it, once.
	e.store.EnterRepoWorker("owner/repo")
	ce.checkPipeline()
	ce.checkPipeline()
	if rs := ofType(drain(sink, 150*time.Millisecond), channelevents.PipelineResumed); len(rs) != 1 || rs[0].Meta["partition"] != "owner/repo" {
		t.Fatalf("partition resume: %+v", rs)
	}

	// The same members idle again later: a fresh episode, not swallowed by dedup.
	e.store.ExitRepoWorker("owner/repo")
	ce.checkPipeline()
	if len(ofType(drain(sink, 150*time.Millisecond), channelevents.PipelineStalled)) != 1 {
		t.Fatal("a second partition episode with the same anchor must still be published")
	}
}

// The infra-abandon cooldown is an intentional hold: shown in health, never a stall.
func TestPipelinePartitionInfraCooldownDoesNotStall(t *testing.T) {
	e, sink, clk, ce := pipelineEngine(t, nil)
	later := time.Now().Add(5 * time.Minute)
	e.seedAPIItem(t, 1, "Queued")
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: gh.ProjectItem{ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Queued"}})
	clk.set(later)
	e.health.beginPoll(later, pollVerdict{})
	e.health.endPoll(later, pollVerdict{}, later.Add(30*time.Second))
	e.markInfraAbandon("owner/repo")
	ce.checkPipeline()
	if evs := drain(sink, 150*time.Millisecond); len(ofType(evs, channelevents.PipelineStalled)) != 0 {
		t.Fatalf("stall inside the infra cooldown: %+v", evs)
	}
	// Once the hold expires with no run, the partition counts as stalled.
	clk.set(later.Add(10 * time.Minute))
	e.health.beginPoll(clk.Now(), pollVerdict{})
	e.health.endPoll(clk.Now(), pollVerdict{}, clk.Now().Add(30*time.Second))
	ce.checkPipeline()
	if len(ofType(drain(sink, 150*time.Millisecond), channelevents.PipelineStalled)) != 1 {
		t.Fatal("an expired hold with no run must raise the stall")
	}
}

func TestPipelineCatchUpIncludesOpenStall(t *testing.T) {
	e, _, clk, _ := pipelineEngine(t, nil)
	t0 := clk.Now()
	if got := e.pipelineCatchUpEvents(t0); len(got) != 0 {
		t.Fatalf("no stall, no catch-up: %+v", got)
	}
	e.health.beginPoll(t0, pollVerdict{})
	clk.set(t0.Add(5 * time.Minute))
	got := e.pipelineCatchUpEvents(clk.Now())
	if len(got) != 1 || got[0].Type != channelevents.PipelineStalled || got[0].Meta["catch_up"] != "true" || got[0].DedupKey != "" {
		t.Fatalf("catch-up = %+v", got)
	}
}
