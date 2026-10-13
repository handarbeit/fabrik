package sim

import (
	"testing"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// The semantic default (#2169 R3): a subscription created without an events list
// gets validate-settled and the rest of the semantic family, not the raw label
// events; naming label-applied restores them. Subscriptions are created through
// the daemon's own Subscribe, as the MCP shim does.
func TestChannelDefaultFamilyExcludesLabelEvents(t *testing.T) {
	env := newChannelGateEnv(t, false, nil)
	hub, _ := startChannel(t, env) // starts the hub; its "S" subscriber is not used here
	st := env.Engine.ChannelStreamerForTest()
	if st == nil {
		t.Fatal("no streamer")
	}
	if _, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "plain"}); err != nil {
		t.Fatal(err)
	}
	res, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "named", Events: []string{"label-applied", "validate-settled"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Subscription.EventsDefault {
		t.Fatal("a subscription that names events is not the default family")
	}
	plain, named := &simSink{}, &simSink{}
	hub.Attach("plain", plain)
	hub.Attach("named", named)

	// A label change on a matching item...
	num := FileIssue(t, env, "default family", "Default family.", "Implement", "fabrik:cruise")
	RunPolls(t, env, 3)
	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, "operator-note"); err != nil {
		t.Fatal(err)
	}
	// ...and the same item settling at Validate.
	settleOnCruise(t, env, num)

	if got := plain.ofType(channelevents.ValidateSettled, num); len(got) != 1 {
		t.Fatalf("the default family must include validate-settled: %+v", plain.all())
	}
	for _, typ := range []channelevents.EventType{channelevents.LabelApplied, channelevents.LabelRemoved} {
		if got := plain.ofType(typ, num); len(got) != 0 {
			t.Fatalf("the default family must not include %s: %+v", typ, got)
		}
	}
	// Non-vacuity: the same label change did happen and reached the subscription
	// that named label-applied.
	var sawLabel bool
	for _, ev := range named.ofType(channelevents.LabelApplied, num) {
		if ev.Label == "operator-note" {
			sawLabel = true
		}
	}
	if !sawLabel {
		t.Fatalf("naming label-applied must deliver it: %+v", named.all())
	}
	if got := named.ofType(channelevents.ValidateSettled, num); len(got) != 1 {
		t.Fatalf("named subscription also asked for validate-settled: %+v", named.all())
	}
}

// A subscription persisted before the change keeps meaning "every type"
// (grandfathered, ADR 2169), and re-subscribing with the same filters does not
// create a second subscription for either kind.
func TestChannelLegacySubscriptionKeepsLabelEvents(t *testing.T) {
	env := newChannelGateEnv(t, false, nil)
	hub, _ := startChannel(t, env)
	legacy, err := hub.Subscribe(channelevents.Subscription{Subscriber: "old"}) // no marker: as persisted before
	if err != nil {
		t.Fatal(err)
	}
	st := env.Engine.ChannelStreamerForTest()
	// The same filters through the new daemon: the existing subscription is returned.
	res, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Subscription.ID != legacy.ID || len(res.Subscriptions) != 1 || res.Subscription.EventsDefault {
		t.Fatalf("re-subscribe must be idempotent and leave the grandfathered subscription alone: %+v", res)
	}
	sink := &simSink{}
	hub.Attach("old", sink)
	num := FileIssue(t, env, "grandfathered", "Legacy.", "Implement", "fabrik:cruise")
	RunPolls(t, env, 3)
	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, "operator-note"); err != nil {
		t.Fatal(err)
	}
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait()
		return len(sink.ofType(channelevents.LabelApplied, num)) > 0
	}, 40)
	if len(sink.ofType(channelevents.LabelApplied, num)) == 0 {
		t.Fatalf("a grandfathered subscription must keep receiving label events: %+v", sink.all())
	}
}

func TestChannelSubscribeNotesLabelFiltersWithoutLabelEvents(t *testing.T) {
	env := newChannelGateEnv(t, false, nil)
	startChannel(t, env)
	st := env.Engine.ChannelStreamerForTest()
	res, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "n", Labels: []string{"fabrik:*"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) != 1 {
		t.Fatalf("labels without label events must be noted: %+v", res.Notes)
	}
	res, err = st.Subscribe(localapi.SubscribeParams{Subscriber: "m", Labels: []string{"fabrik:*"}, Events: []string{"label-applied"}})
	if err != nil || len(res.Notes) != 0 {
		t.Fatalf("no note when label events are named: %+v %v", res.Notes, err)
	}
}
