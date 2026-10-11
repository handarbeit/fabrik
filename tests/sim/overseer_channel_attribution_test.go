package sim

import (
	"strconv"
	"testing"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/tests/sim/simgh/ghfault"
)

// Channel label-event attribution (#2125, ADR 2125). These need
// EnvOptions.BoardCache: without it the engine has no cache, an engine label
// write never reaches the store through the write-through, and the event would
// only ever appear later as a reconcile-seen "unknown" — a test that passed or
// failed for the wrong reason. Every test here asserts an event was received.
//
// Serial for the same reason as channel_events_test.go.

// pausedTarget files an issue the engine leaves alone (paused), warms the cache
// on it and returns its number.
func pausedTarget(t *testing.T, env *Env, labels ...string) int {
	t.Helper()
	num := FileIssue(t, env, "overseer target", "body", "Specify", labels...)
	for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
		if err := env.Sim.Sim().AddLabelToIssue(env.Owner, env.Repo, num, l); err != nil {
			t.Fatalf("AddLabelToIssue(%s): %v", l, err)
		}
	}
	RunPolls(t, env, 2) // the daemon now holds the item
	return num
}

func labelEventFor(sink *simSink, typ channelevents.EventType, issue int, label string) (channelevents.Event, bool) {
	for _, ev := range sink.ofType(typ, issue) {
		if ev.Label == label {
			return ev, true
		}
	}
	return channelevents.Event{}, false
}

func TestOverseerActionLabelEventIsAttributedToOverseer(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), BoardCache: true})
	num := pausedTarget(t, env)
	_, sink := startChannel(t, env)

	if _, err := env.Engine.OverseerActorForTest().SetAutonomy(localapi.SetAutonomyParams{
		Subscriber: "x", Issue: env.OwnerRepo + "#" + strconv.Itoa(num), Mode: "cruise",
	}); err != nil {
		t.Fatalf("SetAutonomy: %v", err)
	}
	RunPoll(t, env)
	settleWait()

	ev, ok := labelEventFor(sink, channelevents.LabelApplied, num, "fabrik:cruise")
	if !ok {
		t.Fatalf("no label-applied fabrik:cruise event; got %+v", sink.all())
	}
	if ev.Meta["actor"] != "overseer" || ev.Meta["via"] != "x" {
		t.Fatalf("overseer action attributed wrongly: %+v", ev.Meta)
	}
	if n := len(sink.ofType(channelevents.LabelApplied, num)); n != 1 {
		// Exactly one push per change, not one from the write-through and one
		// from a later reconcile/echo.
		for _, e := range sink.ofType(channelevents.LabelApplied, num) {
			t.Logf("event: %s %v", e.Label, e.Meta)
		}
		t.Fatalf("label-applied events for the item = %d, want 1", n)
	}
}

func TestEngineAutonomousLabelWriteStaysFabrik(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), BoardCache: true})
	num := pausedTarget(t, env)
	_, sink := startChannel(t, env)

	// An operator applies the one-shot fabrik:revalidate command label to a
	// non-Validate item; the engine's settle scan consumes it with its own
	// label removal, which is an engine-autonomous write.
	if err := env.Sim.Sim().AddLabelToIssue(env.Owner, env.Repo, num, "fabrik:revalidate"); err != nil {
		t.Fatal(err)
	}
	RunPolls(t, env, 3)
	settleWait()

	ev, ok := labelEventFor(sink, channelevents.LabelRemoved, num, "fabrik:revalidate")
	if !ok {
		t.Fatalf("no label-removed fabrik:revalidate event; got %+v", sink.all())
	}
	if ev.Meta["actor"] != "fabrik" {
		t.Fatalf("autonomous removal: actor = %q, want fabrik (meta %v)", ev.Meta["actor"], ev.Meta)
	}
	if v, has := ev.Meta["via"]; has {
		t.Fatalf("autonomous write carries via=%q", v)
	}
}

func TestFailedOverseerWriteLeavesNoAttribution(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), BoardCache: true})
	num := pausedTarget(t, env)
	_, sink := startChannel(t, env)

	env.Sim.Faults().FailOnce("AddLabelToIssue", ghfault.ServerError())
	if _, err := env.Engine.OverseerActorForTest().SetAutonomy(localapi.SetAutonomyParams{
		Subscriber: "x", Issue: env.OwnerRepo + "#" + strconv.Itoa(num), Mode: "cruise",
	}); err == nil {
		t.Fatal("expected the failed write to be reported")
	}
	RunPoll(t, env)
	settleWait()
	if _, ok := labelEventFor(sink, channelevents.LabelApplied, num, "fabrik:cruise"); ok {
		t.Fatalf("failed write produced an event: %+v", sink.all())
	}

	// The same label written later by someone else is not attributed to x: a
	// human adds it in the UI, the next poll first sees it, and nobody told us who.
	if err := env.Sim.Sim().AddLabelToIssue(env.Owner, env.Repo, num, "fabrik:cruise"); err != nil {
		t.Fatal(err)
	}
	RunPolls(t, env, 2)
	settleWait()
	ev, ok := labelEventFor(sink, channelevents.LabelApplied, num, "fabrik:cruise")
	if !ok {
		t.Fatalf("no event for the later write; got %+v", sink.all())
	}
	if ev.Meta["actor"] == "overseer" || ev.Meta["via"] != "" {
		t.Fatalf("stale overseer attribution leaked onto a later write: %+v", ev.Meta)
	}
	if ev.Meta["actor"] != "unknown" {
		t.Errorf("reconcile-seen label: actor = %q, want unknown (no cheap read names the actor)", ev.Meta["actor"])
	}
}

func TestPartialSetAutonomyAttributesLandedWrite(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), BoardCache: true})
	num := pausedTarget(t, env, "fabrik:yolo")
	_, sink := startChannel(t, env)

	// yolo -> cruise adds cruise first, then removes yolo; fail the removal.
	env.Sim.Faults().FailOnce("RemoveLabelFromIssue", ghfault.ServerError())
	_, err := env.Engine.OverseerActorForTest().SetAutonomy(localapi.SetAutonomyParams{
		Subscriber: "x", Issue: env.OwnerRepo + "#" + strconv.Itoa(num), Mode: "cruise",
	})
	t.Logf("SetAutonomy err (partial): %v", err)
	settleWait()

	ev, ok := labelEventFor(sink, channelevents.LabelApplied, num, "fabrik:cruise")
	if !ok {
		t.Fatalf("landed write produced no event; got %+v", sink.all())
	}
	if ev.Meta["actor"] != "overseer" || ev.Meta["via"] != "x" {
		t.Fatalf("landed write of a partial set_autonomy: %+v", ev.Meta)
	}
	if _, removed := labelEventFor(sink, channelevents.LabelRemoved, num, "fabrik:yolo"); removed {
		t.Fatal("the failed removal must produce no event")
	}
}

// R2: with no webhook sender, a reconcile-seen label stays unknown, and the
// channel layer makes no GitHub call to find out.
func TestUnattributedLabelStaysUnknownWithoutExtraReads(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), BoardCache: true})
	num := pausedTarget(t, env)
	_, sink := startChannel(t, env)

	before := len(env.Sim.Log().ByMethod("FetchLabelAppliedAt")) + len(env.Sim.Log().ByMethod("FetchLabelRemovedAt"))
	if err := env.Sim.Sim().AddLabelToIssue(env.Owner, env.Repo, num, "operator-label"); err != nil {
		t.Fatal(err)
	}
	RunPolls(t, env, 2)
	settleWait()

	ev, ok := labelEventFor(sink, channelevents.LabelApplied, num, "operator-label")
	if !ok {
		t.Fatalf("no event; got %+v", sink.all())
	}
	if ev.Meta["actor"] != "unknown" {
		t.Fatalf("actor = %q, want unknown", ev.Meta["actor"])
	}
	if _, has := ev.Meta["actor_login"]; has {
		t.Fatalf("unknown actor carries a login: %v", ev.Meta)
	}
	after := len(env.Sim.Log().ByMethod("FetchLabelAppliedAt")) + len(env.Sim.Log().ByMethod("FetchLabelRemovedAt"))
	if after != before {
		t.Fatalf("attribution caused %d label-event reads", after-before)
	}
}
