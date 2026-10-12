package sim

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// The situation digest (#2171, ADR 2171): one rolled-up "what needs you" event,
// pushed on a subscription's cadence only when the view changed, on request, and
// as the catch-up on subscribe/attach. The sim clock is shared with the engine, so
// a cadence is crossed with Clock.Advance and nothing sleeps for it; the deriver's
// tick is shortened. Serial for the same reason as channel_events_test.go: the
// timing and the dedup seam are process-wide.

// startSituationChannel starts the hub with a fast tick and subscribes "ov" to
// the semantic default with a 60 s situation cadence, directly on the hub so the
// subscribe-time catch-up does not precede the first scheduled digest.
func startSituationChannel(t *testing.T, env *Env) *simSink {
	t.Helper()
	restore := engine.SetChannelTimingForTest(20*time.Millisecond, 25*time.Millisecond)
	t.Cleanup(restore)
	hub := env.Engine.StartChannelEventsForTest(t.TempDir())
	if hub == nil {
		t.Fatal("channel hub did not start")
	}
	t.Cleanup(env.Engine.StopChannelEventsForTest)
	if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "ov", EventsDefault: true, SituationSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	sink := &simSink{}
	hub.Attach("ov", sink)
	return sink
}

// situationEnv is the channel gate env with a 60 s poll interval, so that 61 s of
// sim time between polls is not an overdue poll (which is itself a change of the
// view, and would hide the dedup under test).
func situationEnv(t *testing.T) *Env {
	t.Helper()
	return newChannelGateEnv(t, false, func(cfg *engine.Config) { cfg.PollSeconds = 60 })
}

// cadenceTick runs a poll (so poll health is fresh), moves the engine clock past
// the cadence and gives the consumer a moment to evaluate it.
func cadenceTick(t *testing.T, env *Env) {
	t.Helper()
	RunPolls(t, env, 1)
	env.Clock.Advance(61 * time.Second)
	settleWait()
}

func digestsOf(sink *simSink) []channelevents.Event { return sink.ofType(channelevents.Situation, 0) }

func TestSituationDigest_TwoQuietTicksPublishOneAChangePublishesASecond(t *testing.T) {
	env := situationEnv(t)
	sink := startSituationChannel(t, env)

	cadenceTick(t, env)
	cadenceTick(t, env)
	got := digestsOf(sink)
	if len(got) != 1 {
		t.Fatalf("two cadence ticks with no change published %d digests, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Content, "nothing needs you") || got[0].Meta[channelevents.MetaTrigger] != "cadence" {
		t.Fatalf("first digest = %+v", got[0])
	}

	// A change between ticks: a cruise item settles at Validate, awaiting a merge
	// decision. Polls move the sim clock and may publish along the way; what is
	// asserted is the end state, not how many intermediate views there were.
	num := FileIssue(t, env, "settles", "Settles under cruise.", "Implement", "fabrik:cruise")
	settleOnCruise(t, env, num)
	cadenceTick(t, env)
	cadenceTick(t, env)
	got = digestsOf(sink)
	if len(got) < 2 {
		t.Fatalf("a change between ticks must publish a second digest, got %d", len(got))
	}
	last := got[len(got)-1]
	if !strings.Contains(last.Content, "Awaiting a merge decision") || !strings.Contains(last.Content, "#"+strconv.Itoa(num)) || last.Meta["merge_decisions"] != "1" {
		t.Fatalf("the digest after the change must carry the merge decision:\n%s\n%v", last.Content, last.Meta)
	}

	// And once more nothing changes: two further ticks add nothing.
	n := len(got)
	cadenceTick(t, env)
	cadenceTick(t, env)
	if all := digestsOf(sink); len(all) != n {
		for i, d := range all[n-1:] {
			t.Logf("digest %d:\n%s", n-1+i, d.Content)
		}
		t.Fatalf("an unchanged view was sent again: %d -> %d digests", n, len(all))
	}
}

// Non-vacuity: against a builder that ignores the dedup, the quiet-ticks
// assertion above fails — every tick publishes.
func TestSituationDigest_NonVacuous_DedupIgnoredPublishesEveryTick(t *testing.T) {
	restore := engine.SetSituationDedupDisabledForTest()
	defer restore()
	env := situationEnv(t)
	sink := startSituationChannel(t, env)
	cadenceTick(t, env)
	cadenceTick(t, env)
	if got := len(digestsOf(sink)); got < 2 {
		t.Fatalf("with the dedup ignored every tick publishes: want at least 2 digests, got %d", got)
	}
}

func TestSituationDigest_NoCadenceMeansNoScheduledDigest(t *testing.T) {
	env := situationEnv(t)
	restore := engine.SetChannelTimingForTest(20*time.Millisecond, 25*time.Millisecond)
	defer restore()
	hub := env.Engine.StartChannelEventsForTest(t.TempDir())
	t.Cleanup(env.Engine.StopChannelEventsForTest)
	if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "quiet", EventsDefault: true}); err != nil {
		t.Fatal(err)
	}
	sink := &simSink{}
	hub.Attach("quiet", sink)
	num := FileIssue(t, env, "no cadence", "body", "Implement", "fabrik:cruise")
	settleOnCruise(t, env, num)
	cadenceTick(t, env)
	cadenceTick(t, env)
	if got := digestsOf(sink); len(got) != 0 {
		t.Fatalf("a subscription with no cadence was pushed %d digests: %+v", len(got), got)
	}
}

// The catch-up on subscribe is the digest, equal in content to the digest the
// tool pushes on request for the same state.
func TestSituationCatchUp_IsTheDigest(t *testing.T) {
	env := situationEnv(t)
	restore := engine.SetChannelTimingForTest(20*time.Millisecond, time.Hour)
	defer restore()
	hub := env.Engine.StartChannelEventsForTest(t.TempDir())
	t.Cleanup(env.Engine.StopChannelEventsForTest)
	num := FileIssue(t, env, "waiting", "body", "Implement", "fabrik:cruise")
	settleOnCruise(t, env, num)

	st := env.Engine.ChannelStreamerForTest()
	if _, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "late"}); err != nil {
		t.Fatal(err)
	}
	sink := &simSink{}
	hub.Attach("late", sink)
	settleWait()
	catchUps := digestsOf(sink)
	if len(catchUps) != 1 || catchUps[0].Meta[channelevents.MetaTrigger] != "catch_up" || catchUps[0].Meta["catch_up"] != "true" {
		t.Fatalf("subscribing delivers exactly one situation catch-up: %+v", catchUps)
	}
	if !strings.Contains(catchUps[0].Content, "#"+strconv.Itoa(num)) {
		t.Fatalf("the catch-up names the settled item:\n%s", catchUps[0].Content)
	}

	// The same state through the tool, pushed on request (forced: the catch-up
	// already recorded this view).
	be, ok := env.Engine.LocalAPIBackend().(localapi.SituationBackend)
	if !ok {
		t.Fatal("the engine's backend does not serve situation")
	}
	res, err := be.Situation(localapi.SituationParams{Subscriber: "late", Push: true, Force: true})
	if err != nil || !res.Pushed {
		t.Fatalf("request: %+v %v", res, err)
	}
	settleWait()
	all := digestsOf(sink)
	if len(all) != 2 {
		t.Fatalf("catch-up plus the requested digest = %d, want 2", len(all))
	}
	if all[0].Content != all[1].Content {
		t.Fatalf("catch-up content differs from the digest content:\n%s\n---\n%s", all[0].Content, all[1].Content)
	}
	if res.View.MergeDecisions.Entries[0].Issue != env.OwnerRepo+"#"+strconv.Itoa(num) {
		t.Errorf("the tool's merge decisions = %+v", res.View.MergeDecisions)
	}

	// A request that changes nothing is not re-sent unless forced.
	res, _ = be.Situation(localapi.SituationParams{Subscriber: "late", Push: true})
	if res.Pushed {
		t.Fatalf("an unchanged view was pushed again: %+v", res)
	}
}
