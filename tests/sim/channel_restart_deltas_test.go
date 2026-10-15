package sim

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// Restart delivery (#2169 R1/R2). A subscriber attached across a daemon restart
// receives, in order, one catch-up snapshot of what needs it and then real deltas
// for changes made while the daemon was down - never a replay of unchanged state.
// (daemon-reachable, the first item of the sequence, is written by the MCP shim at
// the attach ack, before the hub drains; the shim's own tests cover it.)

// restartMode selects what the second daemon does.
type restartMode int

const (
	restartNew       restartMode = iota // production path: warming, diff, catch-up
	restartPreChange                    // behaviour before #2169: no diff, no restart catch-up
)

type restartRun struct {
	sink       *simSink
	open       []int
	downtimeID map[string]int // name -> issue, for the changes the scenario made
}

// runRestartScenario runs a first daemon that observes the board, applies
// downtime to the world while it is "down", then starts a second daemon on the
// same state directory and returns what the attached subscriber received.
func runRestartScenario(t *testing.T, mode restartMode, downtime func(env *Env, open []int) map[string]int) restartRun {
	t.Helper()
	env := NewEnv(t, EnvOptions{Stages: lifecycleStages(), BoardCache: true})
	env.Engine.SetChannelTimingForTest(20*time.Millisecond, time.Hour) // carried to the restarted engine by RestartEnv
	open, _ := restartWorld(t, env)
	// A label a person owns, to be removed while the daemon is down.
	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, open[0], "triage-hold"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	hub1 := env.Engine.StartChannelEventsForTest(dir)
	if hub1 == nil {
		t.Fatal("first hub did not start")
	}
	if _, err := hub1.Subscribe(channelevents.Subscription{Subscriber: "S"}); err != nil {
		t.Fatal(err)
	}
	first := &simSink{}
	hub1.Attach("S", first)
	RunPolls(t, env, 4)
	reconcile(t, env)
	RunPolls(t, env, 2)
	settleWait(env)
	env.Engine.StopChannelEventsForTest() // flushes the ledger, as Close or the pre-exec hook does

	changed := downtime(env, open)

	r := RestartEnv(t, env)
	r.BoardCache, r.Wake = r.Engine.UseBoardCacheForTest()
	var hub *channelevents.Hub
	if mode == restartNew {
		hub = r.Engine.StartChannelEventsWarmingForTest(dir)
	} else {
		restoreD := r.Engine.SetDowntimeDeltasDisabledForTest()
		t.Cleanup(restoreD)
		hub = r.Engine.StartChannelEventsForTest(dir)
	}
	if hub == nil {
		t.Fatal("second hub did not start")
	}
	t.Cleanup(r.Engine.StopChannelEventsForTest)
	sink := &simSink{}
	// Attach the way the shim does after a reconnect: not a fresh session, so no
	// catch_up flag. The restart catch-up must arrive anyway.
	detach, err := r.Engine.ChannelStreamerForTest().Attach(localapi.AttachParams{Subscriber: "S", CatchUp: false}, sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(detach)

	RunPolls(t, r, 4)
	reconcile(t, r)
	RunPolls(t, r, 2)
	settleWait(r)
	settleWait(r)
	return restartRun{sink: sink, open: open, downtimeID: changed}
}

func reconcile(t *testing.T, env *Env) {
	t.Helper()
	board, err := env.Sim.FetchProjectBoard(env.Owner, env.Repo, env.ProjectNum, env.cfg.OwnerType)
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	env.BoardCache.Reconcile(board)
}

func describe(evs []channelevents.Event) string {
	var b strings.Builder
	for _, ev := range evs {
		fmt.Fprintf(&b, "\n  %s #%d label=%q source=%s unverified=%s actor=%s", ev.Type, ev.Issue, ev.Label, ev.Meta["source"], ev.Meta["unverified"], ev.Meta["actor"])
	}
	return b.String()
}

func downtimeChanges(env *Env, open []int) map[string]int {
	// fabrik:paused is left alone so the engine does not resume the item and add
	// churn of its own; a hand-owned label stands in for the removed pause label.
	if err := env.Sim.RemoveLabelFromIssue(env.Owner, env.Repo, open[0], "triage-hold"); err != nil {
		env.T.Fatal(err)
	}
	env.Sim.Sim().SeedProjectItem(env.Owner, env.ProjectNum, env.OwnerRepo, open[1], false, "Research")
	return map[string]int{"label": open[0], "column": open[1]}
}

// catchUpPrefix splits the received events into the leading catch-up block and
// the rest.
func catchUpPrefix(evs []channelevents.Event) (catchUp, rest []channelevents.Event) {
	i := 0
	for i < len(evs) && evs[i].Meta["source"] == channelevents.SourceCatchUp {
		i++
	}
	return evs[:i], evs[i:]
}

// No change while the daemon is down: one catch-up snapshot and nothing else -
// in particular no label or column event, which the pre-#2169 legacy flood and a
// naive replay would both produce.
func TestChannelRestartDeliversCatchUpOnlyWhenNothingChanged(t *testing.T) {
	t.Parallel()
	run := runRestartScenario(t, restartNew, func(env *Env, open []int) map[string]int { return nil })
	evs := run.sink.all()
	catchUp, rest := catchUpPrefix(evs)
	if len(catchUp) == 0 {
		t.Fatalf("a reconnect after a daemon restart must receive the catch-up snapshot, even when it did not ask for one: %s", describe(evs))
	}
	if len(rest) != 0 {
		t.Fatalf("nothing changed while down, so nothing may follow the catch-up:%s", describe(rest))
	}
	for _, ev := range catchUp {
		if ev.Meta["catch_up"] != "true" {
			t.Errorf("catch-up event without catch_up=true: %+v", ev.Meta)
		}
	}
}

// One label removed and one card moved while the daemon was down: the catch-up
// snapshot first, then exactly those two changes marked downtime-delta, and
// nothing else.
func TestChannelRestartDeliversCatchUpThenExactlyTheDowntimeDeltas(t *testing.T) {
	t.Parallel()
	run := runRestartScenario(t, restartNew, downtimeChanges)
	evs := run.sink.all()
	catchUp, rest := catchUpPrefix(evs)
	if len(catchUp) == 0 {
		t.Fatalf("no catch-up snapshot:%s", describe(evs))
	}
	if len(rest) != 2 {
		t.Fatalf("want exactly the two downtime deltas after the catch-up, got %d:%s", len(rest), describe(rest))
	}
	var sawLabel, sawColumn bool
	for _, ev := range rest {
		if ev.Meta["source"] != channelevents.SourceDowntimeDelta {
			t.Errorf("source = %q, want downtime-delta: %+v", ev.Meta["source"], ev.Meta)
		}
		if ev.Meta["actor"] != "unknown" {
			t.Errorf("a downtime change has no known writer: actor = %q", ev.Meta["actor"])
		}
		if ev.Meta[channelevents.MetaUnverified] != "true" {
			t.Errorf("a downtime delta cannot be attributed to a write or webhook, so it is unverified: %+v", ev.Meta)
		}
		switch {
		case ev.Type == channelevents.LabelRemoved && ev.Issue == run.downtimeID["label"] && ev.Label == "triage-hold":
			sawLabel = true
		case ev.Type == channelevents.ColumnChanged && ev.Issue == run.downtimeID["column"] && ev.Meta["from"] == "Specify" && ev.Meta["to"] == "Research":
			sawColumn = true
		default:
			t.Errorf("unexpected event after the catch-up: %s", describe([]channelevents.Event{ev}))
		}
	}
	if !sawLabel || !sawColumn {
		t.Fatalf("missing delta (label=%v column=%v):%s", sawLabel, sawColumn, describe(rest))
	}
}

// Non-vacuity: with the downtime diff neutralised and no restart catch-up - how a
// restart behaved before #2169 - the same scenario delivers nothing, so the two
// deltas and the catch-up above are the new behaviour, not an accident of the
// fixture.
func TestChannelRestartPreChangeLosesDowntimeChanges(t *testing.T) {
	t.Parallel()
	run := runRestartScenario(t, restartPreChange, downtimeChanges)
	if evs := run.sink.all(); len(evs) != 0 {
		t.Fatalf("before #2169 a restart delivered nothing (the downtime changes were lost):%s", describe(evs))
	}
}

// A reconnect that is not a daemon restart gets no catch-up and no delta (the
// hub stays up, the ledger is untouched).
func TestChannelPlainReconnectGetsNoCatchUp(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: lifecycleStages(), BoardCache: true})
	env.Engine.SetChannelTimingForTest(20*time.Millisecond, time.Hour)
	restartWorld(t, env)
	hub := env.Engine.StartChannelEventsWarmingForTest(t.TempDir())
	t.Cleanup(env.Engine.StopChannelEventsForTest)
	if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "S"}); err != nil {
		t.Fatal(err)
	}
	st := env.Engine.ChannelStreamerForTest()
	sink := &simSink{}
	detach, err := st.Attach(localapi.AttachParams{Subscriber: "S"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	RunPolls(t, env, 4)
	reconcile(t, env)
	RunPolls(t, env, 2)
	settleWait(env)
	settleWait(env)
	before := len(sink.all())
	detach()
	again := &simSink{}
	if _, err := st.Attach(localapi.AttachParams{Subscriber: "S"}, again); err != nil {
		t.Fatal(err)
	}
	settleWait(env)
	if got := again.all(); len(got) != 0 {
		t.Fatalf("a reconnect without a restart must receive nothing new (first session saw %d):%s", before, describe(got))
	}
}
