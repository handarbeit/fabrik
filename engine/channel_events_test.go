package engine

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
)

type chanSink struct {
	mu  sync.Mutex
	got []channelevents.Event
	ch  chan channelevents.Event
}

func newChanSink() *chanSink { return &chanSink{ch: make(chan channelevents.Event, 256)} }

func (s *chanSink) Deliver(ev channelevents.Event) error {
	s.mu.Lock()
	s.got = append(s.got, ev)
	s.mu.Unlock()
	s.ch <- ev
	return nil
}
func (s *chanSink) Superseded() {}

// next waits for one delivered event.
func (s *chanSink) next(t *testing.T) channelevents.Event {
	t.Helper()
	select {
	case ev := <-s.ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a channel event")
		return channelevents.Event{}
	}
}

// nextOfType skips events of other types (state-derived events may interleave).
func (s *chanSink) nextOfType(t *testing.T, typ channelevents.EventType) channelevents.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-s.ch:
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", typ)
			return channelevents.Event{}
		}
	}
}

// quiet asserts no further event arrives for a short window.
func (s *chanSink) quiet(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case ev := <-s.ch:
		t.Fatalf("unexpected event: %+v", ev)
	case <-time.After(window):
	}
}

// channelEngine builds an engine with the channel hub running, short debounce
// and tick, and one subscriber "T" attached with the supplied subscription
// (everything, opted into all labels, when sub is nil).
func channelEngine(t *testing.T, ahead time.Duration, sub *channelevents.Subscription) (*Engine, *chanSink) {
	t.Helper()
	return channelEngineTick(t, ahead, sub, time.Hour)
}

// channelEngineTick is channelEngine with a chosen ticker period, for the
// time-driven events.
func channelEngineTick(t *testing.T, ahead time.Duration, sub *channelevents.Subscription, tick time.Duration) (*Engine, *chanSink) {
	t.Helper()
	oldD, oldT := channelDebounce, channelTick
	channelDebounce, channelTick = 20*time.Millisecond, tick
	t.Cleanup(func() { channelDebounce, channelTick = oldD, oldT })

	e := apiEngine(t, ahead)
	// An adjustable clock installed before any goroutine starts, so a test can
	// move time (a deadline passing) without racing the deriver.
	e.SetClock(&adjClock{t: time.Now().Add(ahead)})
	e.fabrikDir = t.TempDir()
	e.startChannelEvents()
	t.Cleanup(e.closeChannelEvents)
	ce := e.channelEvents()
	if ce == nil {
		t.Fatal("channel events did not start")
	}
	none := []string{}
	s := channelevents.Subscription{Subscriber: "T", ExcludeLabels: &none}
	if sub != nil {
		s = *sub
		s.Subscriber = "T"
	}
	if _, err := ce.hub.Subscribe(s); err != nil {
		t.Fatal(err)
	}
	sink := newChanSink()
	ce.hub.Attach("T", sink)
	return e, sink
}

func TestChannelLabelEngineWriteThenEchoIsOneEventFromFabrik(t *testing.T) {
	e, sink := channelEngine(t, 0, nil)
	e.seedAPIItem(t, 1, "Implement")

	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:paused"})
	// The webhook echo of the engine's own write is a store no-op.
	e.store.Apply(itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: "fabrik:paused", Sender: "fabrik-app[bot]", EchoOfEngine: true})

	ev := sink.nextOfType(t, channelevents.LabelApplied)
	if ev.Label != "fabrik:paused" || ev.Meta["actor"] != "fabrik" || ev.Stage != "Implement" || ev.Issue != 1 {
		t.Fatalf("unexpected event: %+v", ev)
	}
	for _, k := range []string{"repo", "issue", "event", "stage", "label", "actor"} {
		if ev.Meta[k] == "" {
			t.Errorf("meta %q missing: %+v", k, ev.Meta)
		}
	}
	// Exactly one label-applied: drain anything else and make sure it is not another.
	deadline := time.After(300 * time.Millisecond)
loop:
	for {
		select {
		case more := <-sink.ch:
			if more.Type == channelevents.LabelApplied {
				t.Fatalf("second label-applied event: %+v", more)
			}
		case <-deadline:
			break loop
		}
	}
}

func TestChannelLabelActorAttribution(t *testing.T) {
	e, sink := channelEngine(t, 0, nil)
	e.seedAPIItem(t, 1, "Implement")

	e.store.Apply(itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: "human-label", Sender: "alice"})
	if ev := sink.nextOfType(t, channelevents.LabelApplied); ev.Meta["actor"] != "human" {
		t.Fatalf("human label: %+v", ev)
	}
	e.store.Apply(itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: "bot-label", Sender: "dependabot[bot]"})
	if ev := sink.nextOfType(t, channelevents.LabelApplied); ev.Meta["actor"] != "bot" {
		t.Fatalf("bot label: %+v", ev)
	}
	e.store.Apply(itemstate.IssueUnlabeled{Repo: "owner/repo", Number: 1, Label: "human-label", Sender: "alice"})
	if ev := sink.nextOfType(t, channelevents.LabelRemoved); ev.Meta["actor"] != "human" || ev.Label != "human-label" {
		t.Fatalf("removal: %+v", ev)
	}
	// A reconcile that first observes a label has no sender: unknown.
	e.store.Apply(itemstate.ShallowBoardItemUpdated{Repo: "owner/repo", Number: 1, Item: gh.ProjectItem{
		ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Implement", Labels: []string{"bot-label", "seen-by-poll"},
	}})
	if ev := sink.nextOfType(t, channelevents.LabelApplied); ev.Label != "seen-by-poll" || ev.Meta["actor"] != "unknown" {
		t.Fatalf("reconcile label: %+v", ev)
	}
}

func TestChannelDefaultFilterSuppressesLockChurn(t *testing.T) {
	// A default subscription (no exclude list) must not see locked/in_progress.
	sub := channelevents.Subscription{Events: []channelevents.EventType{channelevents.LabelApplied}}
	e, sink := channelEngine(t, 0, &sub)
	e.seedAPIItem(t, 1, "Implement")

	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:locked:bot"})
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "stage:Implement:in_progress"})
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:blocked"})
	ev := sink.next(t)
	if ev.Label != "fabrik:blocked" {
		t.Fatalf("default filter let %q through first", ev.Label)
	}
	sink.quiet(t, 200*time.Millisecond)
}

func TestChannelNoEventsForInitialPopulation(t *testing.T) {
	e, sink := channelEngine(t, 0, nil)
	e.store.Apply(itemstate.BoardReconciled{Items: []gh.ProjectItem{
		{ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Implement", Labels: []string{"a", "fabrik:paused"}},
	}})
	e.store.Reset([]gh.ProjectItem{
		{ID: "I_C", Number: 2, Repo: "owner/repo", Status: "Implement", Labels: []string{"x", "fabrik:paused"}},
	})
	sink.quiet(t, 300*time.Millisecond)
}

func TestChannelEmitEventNilSafeWithoutHub(t *testing.T) {
	e := apiEngine(t, 0)
	e.emitEvent(channelevents.Event{Type: channelevents.Merged}) // must not panic
	e.channelNudge()
}

// R9 (zero GitHub cost) and R10 (observation only): the channel-event sources
// must not reach a GitHub-capable path or a decision-making gate.
func TestChannelEventsSourceDoesNotReachGitHub(t *testing.T) {
	files, err := filepath.Glob("channel_*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no channel source files found: %v", err)
	}
	more, _ := filepath.Glob("../internal/channelevents/*.go")
	files = append(files, more...)
	forbidden := []string{
		"e.client", "e.readClient", "FetchLabelAppliedAt", "labelAppliedAt(", "store.Get(", "e.cache()",
		".FetchItem(", "FetchProjectItem", "feedbackGateBlocks", "unprocessedFeedback(", "net/http",
		"checkReviewGate(", "attemptMergeOnValidate(", "FetchItemDetails", "FetchPRReviews", "FetchCheckRuns",
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, bad := range forbidden {
				if strings.Contains(line, bad) {
					t.Errorf("%s:%d reaches a GitHub-capable or decision path (%q): %s", f, i+1, bad, strings.TrimSpace(line))
				}
			}
		}
	}
}

// adjClock is a goroutine-safe settable clock.
type adjClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *adjClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *adjClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// A daemon restart (SIGHUP re-exec or crash) keeps subscriptions and held
// events, delivers the held ones in order on re-attach, and does not replay a
// settle episode it already announced.
func TestChannelStateSurvivesDaemonRestart(t *testing.T) {
	oldD, oldT := channelDebounce, channelTick
	channelDebounce, channelTick = 20*time.Millisecond, time.Hour
	t.Cleanup(func() { channelDebounce, channelTick = oldD, oldT })
	dir := t.TempDir()

	start := func() *Engine {
		e := apiEngine(t, 0)
		e.fabrikDir = t.TempDir()
		e.startChannelEventsIn(dir)
		t.Cleanup(e.closeChannelEvents)
		return e
	}

	e1 := start()
	if _, err := e1.channelEvents().hub.Subscribe(channelevents.Subscription{
		Subscriber: "X",
		Events:     []channelevents.EventType{channelevents.LabelApplied, channelevents.ValidateSettled},
	}); err != nil {
		t.Fatal(err)
	}
	seedValidate(t, e1, 1, "stage:Validate:complete")
	time.Sleep(120 * time.Millisecond)
	hook(e1, 1) // announces episode 1; "X" is not attached, so it is held
	waitHeld := func(e *Engine, n int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if e.channelEvents().hub.Queued("X") >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("only %d events held", e.channelEvents().hub.Queued("X"))
	}
	waitHeld(e1, 1) // events from different sources have no relative order guarantee; settle the first
	// Three label events occur while no session is attached.
	lbl(e1, 1, "fabrik:a1", "fabrik:a2", "fabrik:a3")
	waitHeld(e1, 4)
	e1.closeChannelEvents() // the restart

	e2 := start()
	hub := e2.channelEvents().hub
	if got := hub.Subscriptions("X"); len(got) != 1 {
		t.Fatalf("subscription lost across restart: %+v", got)
	}
	// The same settled item is seen again by the new process; its episode was
	// already announced, so the settle hook must not announce it again.
	seedValidate(t, e2, 1, "stage:Validate:complete")
	time.Sleep(120 * time.Millisecond)
	hook(e2, 1)
	time.Sleep(200 * time.Millisecond)

	sink := newChanSink()
	hub.Attach("X", sink)
	var types []channelevents.EventType
	var labels []string
	for i := 0; i < 4; i++ {
		ev := sink.next(t)
		types = append(types, ev.Type)
		labels = append(labels, ev.Label)
	}
	if types[0] != channelevents.ValidateSettled || labels[1] != "fabrik:a1" || labels[2] != "fabrik:a2" || labels[3] != "fabrik:a3" {
		t.Fatalf("held events not delivered in order: %v %v", types, labels)
	}
	// Live events follow the held ones; nothing was replayed.
	lbl(e2, 1, "fabrik:live")
	if ev := sink.next(t); ev.Label != "fabrik:live" {
		t.Fatalf("live event after held: %+v", ev)
	}
	sink.quiet(t, 200*time.Millisecond)
}

// The Streamer resolves bare issue numbers with the read API's resolver (and its
// ambiguity error) and surfaces validation errors as bad requests.
func TestChannelStreamerSubscribeResolvesIssuesAndValidates(t *testing.T) {
	e, _ := channelEngine(t, 0, nil)
	e.seedAPIItem(t, 7, "Implement")
	st := e.channelStreamer()
	if st == nil {
		t.Fatal("no streamer with the hub running")
	}
	res, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "S", Issues: []string{"7", "owner/repo#8"}, Milestone: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Subscription.Issues
	if len(got) != 2 || got[0] != (channelevents.IssueRef{Repo: apiRepo, Number: 7}) || got[1].Number != 8 {
		t.Fatalf("issues resolved to %+v", got)
	}
	if _, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "S", Events: []string{"nope"}}); err == nil {
		t.Fatal("unknown event type accepted")
	}
	if _, err := st.Subscribe(localapi.SubscribeParams{Subscriber: ""}); err == nil {
		t.Fatal("empty subscriber accepted")
	}
	// A second repo on the board makes a bare number ambiguous.
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{ID: "I_X", Number: 1, Repo: "other/repo", Status: "Implement"}})
	if _, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "S", Issues: []string{"7"}}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("bare number across two repos must be ambiguous, got %v", err)
	}
	un, err := st.Unsubscribe(localapi.UnsubscribeParams{Subscriber: "S"})
	if err != nil || un.Removed != 1 {
		t.Fatalf("unsubscribe: %+v %v", un, err)
	}
}

// The self-upgrade exec runs no deferred cleanup, so the engine flushes the
// hub's lazily written dedup state right before it (ADR-1966-b). With no hub
// the flush is a no-op.
func TestFlushChannelEventsPersistsDedupBeforeExec(t *testing.T) {
	apiEngine(t, 0).flushChannelEvents() // no hub: must not panic

	e, sink := channelEngine(t, 0, nil)
	ce := e.channelEvents()
	ce.hub.Publish(channelevents.Event{Type: channelevents.Merged, Repo: "owner/repo", Issue: 7, Content: "merged", DedupKey: "merged:owner/repo#7"})
	sink.next(t)

	path := filepath.Join(e.fabrikDir, ".fabrik", "state", "channel", "dedup.json")
	e.flushChannelEvents()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("dedup state not on disk right after the flush: %v", err)
	}
	if !strings.Contains(string(data), "merged:owner/repo#7") {
		t.Fatalf("flushed dedup state is missing the announced key: %s", data)
	}
}

// A subscriber that arrives after items already settled or paused still learns
// about them: a new subscription and a fresh session each get one catch-up
// snapshot of current state, tagged catch_up=true, while a reconnect does not.
func TestCatchUpDeliversWaitingItemsToLateSubscriber(t *testing.T) {
	e, live := channelEngine(t, 0, settledOnly())
	seedValidate(t, e, 1, "fabrik:cruise", "stage:Validate:complete")
	hook(e, 1)
	live.next(t) // announced live, to a subscriber that existed at the time

	e.seedAPIItem(t, 2, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	e.seedAPIItem(t, 3, "Implement") // nothing waiting on anyone

	st := e.channelStreamer()
	if _, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "late"}); err != nil {
		t.Fatal(err)
	}
	// The same subscription again is not a new subscription: no second catch-up.
	if _, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "late"}); err != nil {
		t.Fatal(err)
	}

	collect := func(catchUp bool) map[int]channelevents.Event {
		sink := newChanSink()
		detach, err := st.Attach(localapi.AttachParams{Subscriber: "late", CatchUp: catchUp}, sink)
		if err != nil {
			t.Fatal(err)
		}
		got := map[int]channelevents.Event{}
		timeout := time.After(400 * time.Millisecond)
		for {
			select {
			case ev := <-sink.ch:
				if _, dup := got[ev.Issue]; dup {
					t.Errorf("issue %d announced twice: %+v", ev.Issue, ev)
				}
				got[ev.Issue] = ev
			case <-timeout:
				detach()
				return got
			}
		}
	}

	first := collect(false) // holds exactly the subscribe-time catch-up
	if len(first) != 2 {
		t.Fatalf("want one catch-up event each for #1 and #2, got %+v", first)
	}
	if ev := first[1]; ev.Type != channelevents.ValidateSettled || ev.Meta["catch_up"] != "true" || ev.Meta["next"] != "waiting-for-human" {
		t.Errorf("settled catch-up: %+v", ev)
	}
	if ev := first[2]; ev.Type != channelevents.AwaitingInput || ev.Meta["catch_up"] != "true" {
		t.Errorf("paused catch-up: %+v", ev)
	}
	if _, ok := first[3]; ok {
		t.Error("an item waiting on nobody must not be announced")
	}

	if again := collect(false); len(again) != 0 { // a reconnect: nothing new
		t.Fatalf("a reconnect must not repeat the catch-up, got %+v", again)
	}
	if fresh := collect(true); len(fresh) != 2 { // a new session under the same name
		t.Fatalf("a fresh session wants the snapshot again, got %+v", fresh)
	}
}

// Catch-up is a snapshot, not a transition: it must not consume the live
// event's episode, so a later live settle is still announced exactly once.
func TestCatchUpDoesNotInterfereWithEpisodeDedup(t *testing.T) {
	e, live := channelEngine(t, 0, settledOnly())
	seedValidate(t, e, 1, "fabrik:cruise", "stage:Validate:complete")
	e.catchUp("T", "")
	live.next(t) // the snapshot
	hook(e, 1)
	ev := live.next(t) // the live settle of the same item is still announced
	if ev.Meta["catch_up"] == "true" || ev.DedupKey == "" {
		t.Fatalf("expected the live episode event, got %+v", ev)
	}
}
